package llm

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type AGYResult struct {
	ConversationID    string `json:"conversation_id"`
	Status            string `json:"status"`
	Response          string `json:"response"`
	FinishedReason    string `json:"finished_reason"`
	Error             string `json:"error"`
	FinalStepResponse string `json:"-"`
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

type agyStreamStepUpdate struct {
	ConversationID string         `json:"conversation_id"`
	StepIndex      int            `json:"step_index"`
	State          string         `json:"state"`
	StepType       string         `json:"step_type"`
	TextDelta      string         `json:"text_delta"`
	ToolName       string         `json:"tool_name"`
	DurationSec    float64        `json:"duration_seconds"`
	ToolInfo       map[string]any `json:"tool_info"`
}

type agyStreamEnvelope struct {
	AGYResult
	Type       string               `json:"type"`
	Event      string               `json:"event"`
	StepUpdate *agyStreamStepUpdate `json:"step_update"`
	Result     *AGYResult           `json:"result"`
}

// ParseAGYResult reads the final result object from CLI output (supporting both
// single-JSON `--output-format json` and NDJSON `--output-format stream-json`).
func ParseAGYResult(raw []byte) (AGYResult, error) {
	var result AGYResult
	found := false
	initConvID := ""
	currentStepIdx := -1
	var currentStepBuf strings.Builder
	lastNonEmptyStep := ""
	flushStep := func() {
		if text := strings.TrimSpace(currentStepBuf.String()); text != "" {
			lastNonEmptyStep = text
		}
		currentStepBuf.Reset()
	}

	for raw = bytes.TrimSpace(raw); len(raw) > 0; raw = bytes.TrimSpace(raw) {
		if raw[0] == '{' {
			var entry agyStreamEnvelope
			decoder := json.NewDecoder(bytes.NewReader(raw))
			if err := decoder.Decode(&entry); err == nil {
				switch strings.ToLower(strings.TrimSpace(entry.Event)) {
				case "init":
					if cid := strings.TrimSpace(entry.ConversationID); cid != "" {
						initConvID = cid
					}
				case "step_update":
					if su := entry.StepUpdate; su != nil {
						if cid := strings.TrimSpace(su.ConversationID); cid != "" && initConvID == "" {
							initConvID = cid
						}
						if strings.EqualFold(su.StepType, "agent_response") {
							if su.StepIndex != currentStepIdx {
								flushStep()
								currentStepIdx = su.StepIndex
							}
							currentStepBuf.WriteString(su.TextDelta)
						}
					}
				case "result":
					if entry.Result != nil && *entry.Result != (AGYResult{}) {
						result = *entry.Result
						found = true
					}
				default:
					if entry.Type != "status" && entry.Type != "delta" && entry.AGYResult != (AGYResult{}) &&
						!strings.EqualFold(strings.TrimSpace(entry.Status), "RUNNING") {
						result, found = entry.AGYResult, true
					}
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
	flushStep()
	if !found {
		if initConvID != "" && lastNonEmptyStep != "" {
			// Stream exited before emitting the final result envelope; surface partial conversation ID for recovery.
			return AGYResult{ConversationID: initConvID, Status: "INCOMPLETE", Response: lastNonEmptyStep, FinalStepResponse: lastNonEmptyStep}, nil
		}
		if initConvID != "" {
			return AGYResult{ConversationID: initConvID, Status: "INCOMPLETE"}, nil
		}
		return AGYResult{}, errors.New("AGY 输出中未找到有效的结构化 JSON 结果")
	}
	if result.ConversationID == "" && initConvID != "" {
		result.ConversationID = initConvID
	}
	if lastNonEmptyStep != "" {
		result.FinalStepResponse = lastNonEmptyStep
	}
	return result, nil
}

// IsValidAGYConversationID reports whether value is a canonical 8-4-4-4-12 UUID.
func IsValidAGYConversationID(value string) bool {
	parts := strings.Split(value, "-")
	lengths := [...]int{8, 4, 4, 4, 12}
	if len(parts) != len(lengths) {
		return false
	}
	for i, part := range parts {
		if len(part) != lengths[i] {
			return false
		}
		for _, char := range part {
			valid := (char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') || (char >= 'A' && char <= 'F')
			if !valid {
				return false
			}
		}
	}
	return true
}

// CleanupEphemeralAGYConversation removes on-disk state left behind by one
// stateless daidai-spawned AGY subprocess without scanning unrelated sessions.
func CleanupEphemeralAGYConversation(conversationID string) {
	id := strings.TrimSpace(conversationID)
	if !IsValidAGYConversationID(id) {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return
	}
	base := filepath.Join(home, ".gemini", "antigravity-cli")
	brainDir := filepath.Join(base, "brain", id)
	if info, err := os.Lstat(brainDir); err == nil && info.Mode()&os.ModeSymlink == 0 && info.IsDir() {
		_ = os.RemoveAll(brainDir)
	}
	for _, path := range []string{
		filepath.Join(base, "conversations", id+".db"),
		filepath.Join(base, "conversations", id+".db-wal"),
		filepath.Join(base, "conversations", id+".db-shm"),
		filepath.Join(base, "annotations", id+".pbtxt"),
		filepath.Join(base, "implicit", id+".pb"),
	} {
		_ = os.Remove(path)
	}
}

func isSafeAGYUpstreamRejection(detail string) bool {
	lower := strings.ToLower(detail)
	if lower == "" {
		return false
	}
	for _, needle := range []string{
		"resource_exhausted",
		"individual quota reached",
		"code 429",
		"\"error_code\":429",
		"no capacity available",
		"code 503",
		"\"error_code\":503",
		"unauthenticated",
		"permission_denied",
		"invalid_argument",
	} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

// ParseProviderCooldown extracts a quota/capacity cooldown window from an upstream error.
func ParseProviderCooldown(err error) time.Duration {
	if err == nil {
		return 0
	}
	msg := err.Error()
	lower := strings.ToLower(msg)
	if idx := strings.Index(lower, "resets in "); idx >= 0 {
		rest := strings.TrimSpace(msg[idx+len("resets in "):])
		end := strings.IndexAny(rest, " \t\r\n\",;)\\")
		token := rest
		if end >= 0 {
			token = rest[:end]
		}
		token = strings.TrimRight(token, ".")
		if d, parseErr := time.ParseDuration(token); parseErr == nil && d > 0 {
			if d < 30*time.Second {
				return 30 * time.Second
			}
			if d > 2*time.Hour {
				return 2 * time.Hour
			}
			return d
		}
	}
	if strings.Contains(lower, "resource_exhausted") ||
		strings.Contains(lower, "individual quota reached") ||
		strings.Contains(lower, "code 429") ||
		strings.Contains(lower, "\"error_code\":429") {
		return 3 * time.Minute
	}
	if strings.Contains(lower, "no capacity available") ||
		(strings.Contains(lower, "unavailable") && strings.Contains(lower, "503")) {
		return 60 * time.Second
	}
	return 0
}
