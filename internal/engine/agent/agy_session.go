package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"daidai/internal/db"
)

// AgySessionState 保存单个会话的 AGY 上下文续接状态
type AgySessionState struct {
	mu             sync.Mutex
	runGate        chan struct{} // 同一会话内串行执行，并允许等待方响应取消
	resetDone      chan struct{} // 非空时，会话正在重置；新工单需等重置完成
	users          int           // 受 AGYWorker.mu 保护，包含等待 runGate 的工单
	LastCwd        string
	ConversationID string
	loaded         bool
	LastActive     time.Time
}

type agySessionKey struct {
	platform    string
	requesterID string
	channelID   string
	cwd         string
}

type agyResetScope struct {
	platform  string
	channelID string
	cwd       string
}

func (s *AgySessionState) Lock()   { s.mu.Lock() }
func (s *AgySessionState) Unlock() { s.mu.Unlock() }

func (w *AGYWorker) acquireSessionRun(ctx context.Context, session *AgySessionState) error {
	for {
		session.Lock()
		resetDone := session.resetDone
		session.Unlock()
		if resetDone != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-resetDone:
				continue
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-session.runGate:
		}
		if err := ctx.Err(); err != nil {
			session.runGate <- struct{}{}
			return err
		}

		session.Lock()
		resetDone = session.resetDone
		session.Unlock()
		if resetDone == nil {
			return nil
		}

		// A reset began while this ticket was waiting for the session gate.
		// Return the gate to the resetter and retry only after its barrier opens.
		session.runGate <- struct{}{}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-resetDone:
		}
	}
}

func (w *AGYWorker) getSession(platform, requesterID, channelID, cwd string) *AgySessionState {
	w.mu.Lock()
	defer w.mu.Unlock()

	key := agySessionKey{platform: db.NormPlatform(platform), requesterID: requesterID, channelID: channelID, cwd: cwd}
	scope := agyResetScope{platform: key.platform, channelID: channelID, cwd: cwd}

	// 清理长期未活跃的过期会话，防止 map 无界增长
	if len(w.sessions) > 200 {
		now := time.Now()
		for id, sess := range w.sessions {
			if sess.users > 0 {
				continue
			}
			sess.Lock()
			stale := sess.resetDone == nil && now.Sub(sess.LastActive) > 24*time.Hour && len(sess.runGate) > 0
			sess.Unlock()
			if stale {
				delete(w.sessions, id)
			}
		}
	}

	s, ok := w.sessions[key]
	if !ok {
		s = &AgySessionState{
			LastCwd:    cwd,
			LastActive: time.Now(),
			runGate:    make(chan struct{}, 1),
			resetDone:  w.resetting[scope],
		}
		s.runGate <- struct{}{}
		w.sessions[key] = s
	} else {
		s.Lock()
		s.LastActive = time.Now()
		if resetDone := w.resetting[scope]; resetDone != nil {
			s.resetDone = resetDone
		}
		s.Unlock()
	}
	s.users++
	return s
}

func (w *AGYWorker) releaseSession(session *AgySessionState) {
	if session == nil {
		return
	}
	w.mu.Lock()
	if session.users > 0 {
		session.users--
	}
	w.mu.Unlock()
}

// ResetSession 重置指定通道在指定工作空间下的 AGY 会话状态 (清空 ConversationID，下次工单白板启动)
func (w *AGYWorker) ResetSession(platform, channelID, cwd string) error {
	resolvedCWD, resolveErr := w.resolveWorkDir(cwd)
	if resolveErr != nil {
		return fmt.Errorf("无法解析 AGY 会话工作区 %q: %w", cwd, resolveErr)
	}
	cwd = resolvedCWD
	scope := agyResetScope{platform: db.NormPlatform(platform), channelID: channelID, cwd: cwd}
	w.resetMu.Lock()
	defer w.resetMu.Unlock()
	done := make(chan struct{})
	w.mu.Lock()
	if w.resetting == nil {
		w.resetting = make(map[agyResetScope]chan struct{})
	}
	w.resetting[scope] = done
	var sessions []*AgySessionState
	for key, s := range w.sessions {
		if key.platform != scope.platform || key.channelID != channelID || key.cwd != cwd {
			continue
		}
		s.Lock()
		s.resetDone = done
		s.Unlock()
		sessions = append(sessions, s)
	}
	w.mu.Unlock()
	// Wait only for work in this channel/workspace. The worker-wide lock stays
	// available, so unrelated sessions can continue accepting tickets.
	for _, s := range sessions {
		<-s.runGate
	}
	var resetErr error
	if w.sessionDB != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		resetErr = w.sessionDB.DeleteAGYSessions(ctx, platform, channelID, cwd)
		cancel()
	}
	w.mu.Lock()
	delete(w.resetting, scope)
	for key, s := range w.sessions {
		if key.platform != scope.platform || key.channelID != channelID || key.cwd != cwd {
			continue
		}
		s.Lock()
		if resetErr == nil {
			s.ConversationID = ""
			s.loaded = true
		}
		s.LastActive = time.Now()
		s.resetDone = nil
		s.Unlock()
	}
	close(done)
	w.mu.Unlock()
	for _, s := range sessions {
		s.runGate <- struct{}{}
	}
	if resetErr != nil {
		return fmt.Errorf("删除持久化 AGY 会话失败: %w", resetErr)
	}
	return nil
}
