package memory

import (
	"context"
	"log/slog"
	"unicode/utf8"

	"daidai/internal/core"
	"daidai/internal/db"
	"daidai/internal/entity"
)

func (m *MemoryEngine) embedMessage(ctx context.Context, store *db.Manager, emb core.EmbeddingProvider, platform string, id int64, text string, minRunes int, kind string) {
	if id <= 0 || utf8.RuneCountInString(text) < minRunes || ctx.Err() != nil {
		return
	}
	vector, identity, err := embedWithIdentity(ctx, emb, text)
	if err != nil {
		if ctx.Err() == nil {
			slog.Debug("memory: 消息向量生成失败", "kind", kind, "err", err)
		}
		return
	}
	if len(vector) == 0 || identity == "" || ctx.Err() != nil {
		return
	}
	if err := store.UpdateMessageEmbedding(ctx, platform, id, vector, identity); err != nil && ctx.Err() == nil {
		slog.Warn("memory: 保存消息向量失败", "kind", kind, "err", err)
	}
}

func (m *MemoryEngine) backfillUserFactEmbeddings(ctx context.Context, event *entity.SensoryEvent) {
	dbMgr := m.getDB()
	emb := m.getEmbedder()
	if dbMgr == nil || emb == nil || event == nil || event.Platform == "" || event.SenderID == "" {
		return
	}
	visibleFacts, err := dbMgr.ListVisibleFacts(ctx, event.Platform, event.SenderID, event.ChannelID, event.IsGroup)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("memory: 读取待补向量的事实失败", "err", err)
		}
		return
	}
	for _, f := range visibleFacts {
		if ctx.Err() != nil {
			return
		}
		if f.EmbeddingModel != embeddingIdentity(emb) && f.Content != "" {
			vec, identity, err := embedWithIdentity(ctx, emb, f.Content)
			if err != nil {
				if ctx.Err() == nil {
					slog.Debug("memory: 事实向量生成失败", "factID", f.ID, "err", err)
				}
				continue
			}
			if len(vec) == 0 || identity == "" || ctx.Err() != nil {
				continue
			}
			if err := dbMgr.UpdateFactEmbedding(ctx, event.Platform, f.ID, f.Content, vec, identity); err != nil && ctx.Err() == nil {
				slog.Warn("memory: 保存事实向量失败", "factID", f.ID, "err", err)
			}
		}
	}
}
