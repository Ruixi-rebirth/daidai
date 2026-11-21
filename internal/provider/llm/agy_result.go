package llm

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

type AGYResult struct {
	ConversationID string `json:"conversation_id"`
	Status         string `json:"status"`
	Response       string `json:"response"`
	FinishedReason string `json:"finished_reason"`
	Error          string `json:"error"`
}

// FailureDetail reports explicit CLI failures. Callers still decide what
// evidence they need to confirm that their requested operation succeeded.
func (r AGYResult) FailureDetail() string {
	if detail := strings.TrimSpace(r.Error); detail != "" {
		return detail
	}
	if strings.EqualFold(strings.TrimSpace(r.FinishedReason), "error") {
		return "finished_reason: error"
	}
	if status := strings.TrimSpace(r.Status); status != "" && !strings.EqualFold(status, "SUCCESS") {
		return status
	}
	return ""
}

// ParseAGYResult reads the last result object from CLI output. Progress events
// and surrounding log lines can share stdout with the final JSON.
func ParseAGYResult(raw []byte) (AGYResult, error) {
	var result AGYResult
	found := false
	for raw = bytes.TrimSpace(raw); len(raw) > 0; raw = bytes.TrimSpace(raw) {
		if raw[0] == '{' {
			var entry struct {
				AGYResult
				Type string `json:"type"`
			}
			decoder := json.NewDecoder(bytes.NewReader(raw))
			if err := decoder.Decode(&entry); err == nil {
				if entry.Type != "status" && entry.Type != "delta" && entry.AGYResult != (AGYResult{}) &&
					!strings.EqualFold(strings.TrimSpace(entry.Status), "RUNNING") {
					result, found = entry.AGYResult, true
				}
				raw = raw[decoder.InputOffset():]
				continue
			}
		}
		_, remainder, ok := bytes.Cut(raw, []byte("\n"))
		if !ok {
			break
		}
		raw = remainder
	}
	if !found {
		return AGYResult{}, errors.New("AGY 输出中未找到有效的结构化 JSON 结果")
	}
	return result, nil
}
