package memory

import (
	"context"
	"strings"

	"daidai/internal/core"
	"daidai/internal/entity"
)

const defaultMaxFacts = 40

func boundedFactContent(content string) string {
	runes := []rune(content)
	if len(runes) <= core.MaxFactContentRunes {
		return content
	}
	return string(runes[:core.MaxFactContentRunes]) + "…"
}

// RecallProfile retrieves facts visible in the user's global private profile.
func (m *MemoryEngine) RecallProfile(ctx context.Context, platform, userID, query string, maxFacts int) ([]*core.Fact, error) {
	dbMgr := m.getDB()
	if dbMgr == nil || platform == "" {
		return nil, nil
	}

	activeFacts, err := dbMgr.ListVisibleFacts(ctx, platform, userID, "", false)
	if err != nil || len(activeFacts) == 0 {
		return nil, err
	}
	return rankProfileFacts(ctx, query, activeFacts, maxFacts), nil
}

// RecallProfileForEvent only injects facts visible from the current conversation scope.
func (m *MemoryEngine) RecallProfileForEvent(ctx context.Context, event *entity.SensoryEvent, query string, maxFacts int) ([]*core.Fact, error) {
	if event == nil || event.Platform == "" || event.SenderID == "" {
		return nil, nil
	}
	dbMgr := m.getDB()
	if dbMgr == nil {
		return nil, nil
	}
	facts, err := dbMgr.ListVisibleFacts(ctx, event.Platform, event.SenderID, event.ChannelID, event.IsGroup)
	if err != nil || len(facts) == 0 {
		return nil, err
	}
	return rankProfileFacts(ctx, query, facts, maxFacts), nil
}

func rankProfileFacts(ctx context.Context, query string, activeFacts []*core.Fact, maxFacts int) []*core.Fact {
	if maxFacts <= 0 {
		maxFacts = defaultMaxFacts
	}

	ranked := RankFactsByBM25(ctx, query, activeFacts, 0)
	if len(ranked) <= maxFacts {
		return ranked
	}

	// Keep the strongest query matches plus a small set of oldest facts as a stable profile baseline.
	coreKeep := maxFacts / 4
	if coreKeep > 10 {
		coreKeep = 10
	}
	dynamicKeep := maxFacts - coreKeep

	selected := make([]*core.Fact, 0, maxFacts)
	seen := make(map[int64]struct{}, maxFacts)
	for _, f := range ranked[:dynamicKeep] {
		selected = append(selected, f)
		seen[f.ID] = struct{}{}
	}
	for _, f := range activeFacts {
		if len(selected) >= maxFacts {
			break
		}
		if _, ok := seen[f.ID]; !ok {
			selected = append(selected, f)
			seen[f.ID] = struct{}{}
		}
	}
	for _, f := range ranked[dynamicKeep:] {
		if len(selected) >= maxFacts {
			break
		}
		if _, ok := seen[f.ID]; !ok {
			selected = append(selected, f)
			seen[f.ID] = struct{}{}
		}
	}
	return selected
}

func factScope(event *entity.SensoryEvent) (string, string) {
	if event != nil && event.IsGroup {
		return "group", strings.TrimSpace(event.ChannelID)
	}
	return "global", ""
}

func sameFactScope(fact *core.Fact, event *entity.SensoryEvent) bool {
	if fact == nil {
		return false
	}
	scopeType, scopeID := factScope(event)
	return fact.ScopeType == scopeType && fact.ScopeID == scopeID
}

func findFact(facts []*core.Fact, id int64) *core.Fact {
	for _, fact := range facts {
		if fact != nil && fact.ID == id {
			return fact
		}
	}
	return nil
}
