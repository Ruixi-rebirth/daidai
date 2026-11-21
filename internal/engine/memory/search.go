package memory

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"daidai/internal/core"
	"daidai/internal/db"
)

type scoredFact struct {
	fact  *core.Fact
	score float32
}

// ScoreFactRelevance 计算单条事实与查询词的综合相关度
func ScoreFactRelevance(query, content string) float32 {
	scores := ScoreBM25(query, []string{content})
	if len(scores) == 0 {
		return 0
	}
	return scores[0]
}

// RankFactsByBM25 对候选事实列表执行 BM25 稀疏检索 + BAAI/bge-m3 稠密向量 RRF 双路混合重排 + 时间衰减，并按阈值过滤
func RankFactsByBM25(ctx context.Context, query string, facts []*core.Fact, minScore float32) []*core.Fact {
	if len(facts) == 0 {
		return nil
	}
	docs := make([]string, len(facts))
	hasFactEmb := false
	queryIdentity := embeddingIdentity(GetDefaultEmbedder())
	for i, f := range facts {
		docs[i] = f.Content
		if len(f.Embedding) > 0 && f.EmbeddingModel == queryIdentity {
			hasFactEmb = true
		}
	}
	bm25Scores := ScoreBM25(query, docs)

	// 仅在主动检索过滤 (minScore > 0) 或存量事实数超过常驻窗口上限需要淘汰截断时请求查询向量；
	// 若全量事实本就少于常驻上限且无需过滤，直接本地 0ms 完成 BM25 + 时间衰减排序，避免主对话热路径发生不必要的网络等待
	var vecScores []float32
	if hasFactEmb && strings.TrimSpace(query) != "" && (minScore > 0 || len(facts) > defaultMaxFacts) {
		if qVec, identity := EmbedTextWithIdentity(ctx, query); len(qVec) > 0 && identity == queryIdentity {
			vecScores = make([]float32, len(facts))
			for i, f := range facts {
				if len(f.Embedding) > 0 && f.EmbeddingModel == identity {
					vecScores[i] = CosineSimilarity(qVec, f.Embedding)
				}
			}
		}
	}

	fusedScores := FuseRRF(bm25Scores, vecScores, 60.0)
	now := time.Now()

	scored := make([]scoredFact, 0, len(facts))
	for i, f := range facts {
		var s float32
		if i < len(fusedScores) {
			s = fusedScores[i]
		}
		if s < minScore {
			continue
		}
		if !f.UpdatedAt.IsZero() {
			days := now.Sub(f.UpdatedAt).Hours() / 24.0
			if days < 0 {
				days = 0
			}
			s += float32(0.15 * math.Exp(-days/90.0))
		}
		scored = append(scored, scoredFact{
			fact:  f,
			score: s,
		})
	}

	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		return scored[i].fact.ID > scored[j].fact.ID
	})

	results := make([]*core.Fact, 0, len(scored))
	for _, item := range scored {
		results = append(results, item.fact)
	}
	return results
}

// SearchMessagesInChatByBM25 keeps lexical and vector recall inside one chat type.
func SearchMessagesInChatByBM25(ctx context.Context, dbMgr *db.Manager, platform, chatType, convID, query string, limit int) ([]db.StoredMessage, error) {
	if chatType != "group" && chatType != "private" {
		return nil, fmt.Errorf("chat type must be group or private")
	}
	return searchMessagesByBM25(ctx, dbMgr, platform, chatType, convID, query, limit)
}

func searchMessagesByBM25(ctx context.Context, dbMgr *db.Manager, platform, chatType, convID, query string, limit int) ([]db.StoredMessage, error) {
	if dbMgr == nil || convID == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}

	terms := ExtractSearchTerms(query)
	var candidates []db.StoredMessage
	seenIDs := make(map[int64]struct{})

	if len(terms) > 0 {
		var lexMsgs []db.StoredMessage
		var err error
		lexMsgs, err = dbMgr.SearchMessagesInChat(ctx, platform, chatType, convID, terms, limit*5)
		if err != nil {
			return nil, err
		}
		for _, m := range lexMsgs {
			candidates = append(candidates, m)
			if m.ID > 0 {
				seenIDs[m.ID] = struct{}{}
			}
		}
	}

	qVec, modelIdentity := EmbedTextWithIdentity(ctx, query)
	if len(qVec) > 0 {
		var vecMsgs []db.StoredMessage
		var err error
		vecMsgs, err = dbMgr.RecentVectorMessagesInChat(ctx, platform, chatType, convID, modelIdentity, 500)
		if err == nil {
			for _, m := range vecMsgs {
				if m.ID > 0 {
					if _, exists := seenIDs[m.ID]; exists {
						continue
					}
				}
				if m.EmbeddingModel == modelIdentity && CosineSimilarity(qVec, m.Embedding) >= 0.48 {
					candidates = append(candidates, m)
					if m.ID > 0 {
						seenIDs[m.ID] = struct{}{}
					}
				}
			}
		}
	}

	if len(candidates) == 0 {
		return nil, nil
	}

	docs := make([]string, len(candidates))
	var vecScores []float32
	if len(qVec) > 0 {
		vecScores = make([]float32, len(candidates))
	}
	for i, m := range candidates {
		docs[i] = m.Text
		if len(qVec) > 0 && len(m.Embedding) > 0 && m.EmbeddingModel == modelIdentity {
			vecScores[i] = CosineSimilarity(qVec, m.Embedding)
		}
	}
	bm25Scores := ScoreBM25(query, docs)
	scores := FuseRRF(bm25Scores, vecScores, 60.0)

	type indexedMsg struct {
		idx   int
		score float32
	}
	ranked := make([]indexedMsg, len(candidates))
	for i := range candidates {
		ranked[i] = indexedMsg{idx: i, score: scores[i]}
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return candidates[ranked[i].idx].ID > candidates[ranked[j].idx].ID
	})
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	sort.Slice(ranked, func(i, j int) bool {
		if candidates[ranked[i].idx].ID != candidates[ranked[j].idx].ID {
			return candidates[ranked[i].idx].ID < candidates[ranked[j].idx].ID
		}
		return ranked[i].idx < ranked[j].idx
	})

	res := make([]db.StoredMessage, len(ranked))
	for i, item := range ranked {
		res[i] = candidates[item.idx]
	}
	return res, nil
}

// RecallProfile 获取用于对话背景注入的用户画像事实 (BM25 稀疏检索 + 时间衰减混合重排 + 核心基础事实保底)
