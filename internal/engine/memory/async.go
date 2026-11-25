package memory

import (
	"context"
	"log/slog"
	"time"

	"daidai/internal/core"
	"daidai/internal/db"
	"daidai/internal/entity"
)

type postCommitJob struct {
	dbMgr       *db.Manager
	event       entity.SensoryEvent
	userText    string
	replyText   string
	userMsgID   int64
	botMsgID    int64
	needExtract bool
	emb         core.EmbeddingProvider
}

func (m *MemoryEngine) schedulePostCommit(dbMgr *db.Manager, event *entity.SensoryEvent, userText, replyText string, userMsgID, botMsgID int64) {
	emb := m.getEmbedder()
	if event != nil && event.SkipVectorIndex {
		emb = nil
	}
	needExtract := m.llm != nil && event.SenderID != "" && shouldExtractFacts(event)
	if !needExtract && emb == nil {
		return
	}

	evCopy := entity.SensoryEvent{
		Platform:         event.Platform,
		IsGroup:          event.IsGroup,
		ChannelID:        event.ChannelID,
		SenderID:         event.SenderID,
		GroupExpression:  event.GroupExpression,
		HistoryRecordIDs: append([]int64(nil), event.HistoryRecordIDs...),
		Content:          core.TruncateRunes(event.Content, core.MaxUserMessageRunes, "\n[消息过长，后续内容已截断]"),
		VisionSummary:    core.TruncateRunes(event.VisionSummary, core.MaxVisionSummaryRunes, "\n[视觉摘要过长，后续内容已截断]"),
	}
	if event.ReplyContext != nil {
		evCopy.ReplyContext = &entity.SensoryEvent{
			Content: entity.SanitizeQuotedMessageContent(event.ReplyContext.Content),
		}
	}
	job := postCommitJob{
		dbMgr: dbMgr, event: evCopy, userText: userText, replyText: replyText,
		userMsgID: userMsgID, botMsgID: botMsgID, needExtract: needExtract, emb: emb,
	}

	m.asyncMu.Lock()
	if m.asyncClosed {
		m.asyncMu.Unlock()
		return
	}
	if !m.workersOn {
		m.workersOn = true
		for i := 0; i < postCommitWorkerCount; i++ {
			m.workerWG.Add(1)
			go m.postCommitWorker()
		}
	}
	m.asyncWG.Add(1)
	select {
	case m.asyncJobs <- job:
		m.asyncMu.Unlock()
	default:
		m.asyncWG.Done()
		m.asyncMu.Unlock()
		slog.Warn("memory: 后台记忆任务队列已满，本轮只保留会话记录", "platform", event.Platform, "channel", event.ChannelID, "sender", event.SenderID)
	}
}

func (m *MemoryEngine) postCommitWorker() {
	defer m.workerWG.Done()
	for {
		select {
		case job := <-m.asyncJobs:
			m.processPostCommitJob(job)
			m.asyncWG.Done()
		case <-m.asyncCtx.Done():
			for {
				select {
				case <-m.asyncJobs:
					m.asyncWG.Done()
				default:
					return
				}
			}
		}
	}
}

func (m *MemoryEngine) processPostCommitJob(job postCommitJob) {
	if m.asyncCtx.Err() != nil {
		return
	}
	if job.needExtract {
		m.consolidateUserFacts(m.asyncCtx, &job.event)
	}
	if job.emb == nil || m.asyncCtx.Err() != nil {
		return
	}
	bgCtx, cancel := context.WithTimeout(m.asyncCtx, 25*time.Second)
	defer cancel()

	m.embedMessage(bgCtx, job.dbMgr, job.emb, job.event.Platform, job.userMsgID, job.userText, 4, "user")
	if job.event.GroupExpression == "" {
		m.embedMessage(bgCtx, job.dbMgr, job.emb, job.event.Platform, job.botMsgID, job.replyText, 6, "assistant")
	}
	if bgCtx.Err() != nil {
		return
	}
	if job.event.SenderID != "" {
		m.backfillUserFactEmbeddings(bgCtx, &job.event)
	}
}

// Shutdown cancels background memory work and waits until it exits or ctx expires.
func (m *MemoryEngine) Shutdown(ctx context.Context) error {
	m.asyncMu.Lock()
	m.asyncClosed = true
	if m.asyncCancel != nil {
		m.asyncCancel()
	}
	m.asyncMu.Unlock()

	done := make(chan struct{})
	go func() {
		m.asyncWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		workerDone := make(chan struct{})
		go func() {
			m.workerWG.Wait()
			close(workerDone)
		}()
		select {
		case <-workerDone:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}
