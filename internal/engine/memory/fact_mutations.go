package memory

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"daidai/internal/core"
	"daidai/internal/db"
	"daidai/internal/entity"
)

type factExtractionResponse struct {
	Mutations []db.FactMutation `json:"mutations"`
}

const (
	maxFactMutationResponseBytes = 64 << 10
	maxFactMutationsPerTurn      = 20
	maxFactContentRunes          = 40
)

// normalizeFactMutations binds model decisions to the facts actually supplied
// to it. Similarity is useful for recall, but cannot authorize an overwrite.
// Exact ADD deduplication and evidence linking belong to the DB transaction.
func normalizeFactMutations(event *entity.SensoryEvent, known []*core.Fact, mutations []db.FactMutation) []db.FactMutation {
	scopeType, scopeID := factScope(event)
	seenIDs := make(map[int64]bool)
	seenAdds := make(map[string]bool)
	normalized := make([]db.FactMutation, 0, len(mutations))
	for _, mutation := range mutations {
		bound := db.FactMutation{
			Action: mutation.Action, Fact: mutation.Fact,
			SourceMessageIDs: append([]int64(nil), event.HistoryRecordIDs...),
			ScopeType:        scopeType, ScopeID: scopeID, Quote: event.Content,
		}
		switch mutation.Action {
		case "ADD":
			if mutation.Fact == "" || seenAdds[mutation.Fact] {
				continue
			}
			seenAdds[mutation.Fact] = true
		case "UPDATE", "DELETE":
			old := findFact(known, mutation.FactID)
			if old == nil || !sameFactScope(old, event) || seenIDs[old.ID] {
				continue
			}
			if mutation.Action == "UPDATE" && mutation.Fact == "" {
				continue
			}
			bound.FactID, bound.ExpectedFact = old.ID, old.Content
			seenIDs[old.ID] = true
		default:
			continue
		}
		normalized = append(normalized, bound)
	}
	return normalized
}

func parseFactMutations(raw string) []db.FactMutation {
	text := strings.TrimSpace(raw)
	if text == "" || len(text) > maxFactMutationResponseBytes {
		return nil
	}
	start := strings.IndexByte(text, '{')
	end := strings.LastIndexByte(text, '}')
	if start >= 0 && end > start {
		text = text[start : end+1]
	}
	var parsed factExtractionResponse
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		return nil
	}
	capacity := min(len(parsed.Mutations), maxFactMutationsPerTurn)
	valid := make([]db.FactMutation, 0, capacity)
	seenFactIDs := make(map[int64]struct{})
	for _, mut := range parsed.Mutations {
		mut.Action = strings.ToUpper(strings.TrimSpace(mut.Action))
		mut.Fact = strings.TrimSpace(mut.Fact)
		switch mut.Action {
		case "ADD":
			if mut.Fact == "" || utf8.RuneCountInString(mut.Fact) > maxFactContentRunes {
				continue
			}
		case "UPDATE":
			if mut.FactID <= 0 || mut.Fact == "" || utf8.RuneCountInString(mut.Fact) > maxFactContentRunes {
				continue
			}
			if _, exists := seenFactIDs[mut.FactID]; exists {
				continue
			}
			seenFactIDs[mut.FactID] = struct{}{}
		case "DELETE":
			if mut.FactID <= 0 {
				continue
			}
			if _, exists := seenFactIDs[mut.FactID]; exists {
				continue
			}
			seenFactIDs[mut.FactID] = struct{}{}
		default:
			continue
		}
		valid = append(valid, mut)
		if len(valid) == maxFactMutationsPerTurn {
			// Enforce the write limit here so one extra model suggestion does not
			// discard every otherwise valid fact mutation in the response.
			break
		}
	}
	return valid
}
