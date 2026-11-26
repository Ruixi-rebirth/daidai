package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"daidai/internal/core"
	"daidai/internal/provider/llm"
)

const (
	agyOutputLimit        = 2 << 20
	maxAGYStreamLineBytes = agyOutputLimit
)

type agyProcessResult struct {
	stdout       *core.LimitedBuffer
	stderr       *core.LimitedBuffer
	duration     time.Duration
	liveLog      string
	lastSummary  string
	streamResult *llm.AGYResult
}

type agyStreamHooks struct {
	onConversationID func(string)
	onLiveProgress   func(liveLog, summary string)
}

type agyLiveStreamWriter struct {
	hooks          agyStreamHooks
	lineBuf        bytes.Buffer
	lineOverflow   bool
	steps          []string
	stepIdxByOrder map[int]int
	currRespStep   int
	currRespBuf    strings.Builder
	lastSummary    string
	initConvID     string
	finalResult    *llm.AGYResult
	lastEmit       time.Time
}

func newAGYLiveStreamWriter(hooks agyStreamHooks) *agyLiveStreamWriter {
	return &agyLiveStreamWriter{
		hooks:          hooks,
		stepIdxByOrder: make(map[int]int),
		currRespStep:   -1,
	}
}

func (w *agyLiveStreamWriter) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		idx := bytes.IndexByte(p, '\n')
		if idx < 0 {
			if !w.lineOverflow && w.lineBuf.Len()+len(p) <= maxAGYStreamLineBytes {
				_, _ = w.lineBuf.Write(p)
			} else {
				w.lineOverflow = true
			}
			break
		}
		if !w.lineOverflow && w.lineBuf.Len()+idx <= maxAGYStreamLineBytes {
			_, _ = w.lineBuf.Write(p[:idx])
			w.consumeLine(w.lineBuf.Bytes())
		}
		w.lineBuf.Reset()
		w.lineOverflow = false
		p = p[idx+1:]
	}
	return total, nil
}

func (w *agyLiveStreamWriter) Flush() {
	if !w.lineOverflow && w.lineBuf.Len() > 0 {
		w.consumeLine(w.lineBuf.Bytes())
	}
	w.lineBuf.Reset()
	w.lineOverflow = false
}

func (w *agyLiveStreamWriter) finalAGYResult() *llm.AGYResult {
	if w.finalResult == nil {
		return nil
	}
	res := *w.finalResult
	if strings.TrimSpace(res.ConversationID) == "" && w.initConvID != "" {
		res.ConversationID = w.initConvID
	}
	if w.lastSummary != "" {
		res.FinalStepResponse = w.lastSummary
	}
	return &res
}

func (w *agyLiveStreamWriter) consumeLine(raw []byte) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return
	}
	var env struct {
		llm.AGYResult
		Type       string `json:"type"`
		Event      string `json:"event"`
		StepUpdate *struct {
			ConversationID string  `json:"conversation_id"`
			StepIndex      int     `json:"step_index"`
			State          string  `json:"state"`
			StepType       string  `json:"step_type"`
			TextDelta      string  `json:"text_delta"`
			ToolName       string  `json:"tool_name"`
			DurationSec    float64 `json:"duration_seconds"`
			ToolInfo       struct {
				Parameters map[string]any `json:"parameters"`
			} `json:"tool_info"`
		} `json:"step_update"`
		Result *llm.AGYResult `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return
	}
	switch strings.ToLower(strings.TrimSpace(env.Event)) {
	case "init":
		if cid := strings.TrimSpace(env.ConversationID); cid != "" {
			w.initConvID = cid
			if w.hooks.onConversationID != nil {
				w.hooks.onConversationID(cid)
			}
		}
	case "step_update":
		su := env.StepUpdate
		if su == nil {
			return
		}
		if cid := strings.TrimSpace(su.ConversationID); cid != "" {
			if w.initConvID == "" {
				w.initConvID = cid
			}
			if w.hooks.onConversationID != nil {
				w.hooks.onConversationID(cid)
			}
		}
		switch su.StepType {
		case "tool":
			label := formatToolStepLine(su.State, su.ToolName, su.DurationSec, su.ToolInfo.Parameters)
			if pos, exists := w.stepIdxByOrder[su.StepIndex]; exists {
				w.steps[pos] = label
			} else {
				w.stepIdxByOrder[su.StepIndex] = len(w.steps)
				w.steps = append(w.steps, label)
			}
			w.emit(true)
		case "agent_response":
			if su.StepIndex != w.currRespStep {
				if prev := strings.TrimSpace(w.currRespBuf.String()); prev != "" {
					w.lastSummary = prev
				}
				w.currRespBuf.Reset()
				w.currRespStep = su.StepIndex
			}
			if w.currRespBuf.Len() < agyOutputLimit {
				w.currRespBuf.WriteString(su.TextDelta)
			}
			if cur := strings.TrimSpace(w.currRespBuf.String()); cur != "" {
				w.lastSummary = cur
			}
			w.emit(strings.EqualFold(su.State, "DONE"))
		}
	case "result":
		if env.Result != nil && *env.Result != (llm.AGYResult{}) {
			res := *env.Result
			w.finalResult = &res
		}
	default:
		if env.Type != "status" && env.Type != "delta" && env.AGYResult != (llm.AGYResult{}) &&
			!strings.EqualFold(strings.TrimSpace(env.Status), "RUNNING") {
			res := env.AGYResult
			w.finalResult = &res
		}
	}
}

func formatToolStepLine(state, toolName string, durationSec float64, params map[string]any) string {
	icon := "⏳"
	if strings.EqualFold(state, "DONE") {
		icon = "✅"
	}
	hint := ""
	for _, key := range []string{"Path", "CommandLine", "toolSummary", "Query", "Pattern"} {
		if val, ok := params[key].(string); ok && strings.TrimSpace(val) != "" {
			val = strings.TrimSpace(val)
			if key == "Path" {
				val = filepath.Base(val)
			} else {
				val = core.TruncateRunes(strings.ReplaceAll(val, "\n", " "), 48, "…")
			}
			hint = " · " + val
			break
		}
	}
	dur := ""
	if strings.EqualFold(state, "DONE") && durationSec >= 0.1 {
		dur = fmt.Sprintf(" (%.1fs)", durationSec)
	}
	if toolName == "" {
		toolName = "step"
	}
	return fmt.Sprintf("%s %s%s%s", icon, toolName, hint, dur)
}

func (w *agyLiveStreamWriter) snapshot() string {
	var sb strings.Builder
	if len(w.steps) > 0 {
		start := 0
		if len(w.steps) > 20 {
			start = len(w.steps) - 20
		}
		sb.WriteString("【实时执行步骤】\n")
		sb.WriteString(strings.Join(w.steps[start:], "\n"))
	}
	if cur := strings.TrimSpace(w.currRespBuf.String()); cur != "" {
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString("【当前阶段回复】\n")
		sb.WriteString(core.TruncateRunes(cur, 3000, "…"))
	} else if w.lastSummary != "" {
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString("【最近阶段说明】\n")
		sb.WriteString(core.TruncateRunes(w.lastSummary, 1500, "…"))
	}
	return strings.TrimSpace(sb.String())
}

func (w *agyLiveStreamWriter) emit(force bool) {
	if w.hooks.onLiveProgress == nil {
		return
	}
	now := time.Now()
	if !force && now.Sub(w.lastEmit) < 500*time.Millisecond {
		return
	}
	w.lastEmit = now
	if snap := w.snapshot(); snap != "" {
		w.hooks.onLiveProgress(snap, w.lastSummary)
	}
}

func runAGYProcess(ctx context.Context, command, cwd string, args []string, hooks ...agyStreamHooks) (agyProcessResult, error) {
	cmd := core.CommandProcessGroup(ctx, command, args...)
	cmd.Dir = cwd
	cmd.WaitDelay = 2 * time.Second

	result := agyProcessResult{
		stdout: core.NewLimitedBuffer(agyOutputLimit),
		stderr: core.NewLimitedBuffer(agyOutputLimit),
	}
	var streamWriter *agyLiveStreamWriter
	if len(hooks) > 0 {
		streamWriter = newAGYLiveStreamWriter(hooks[0])
		cmd.Stdout = io.MultiWriter(result.stdout, streamWriter)
	} else {
		cmd.Stdout = result.stdout
	}
	cmd.Stderr = result.stderr

	startedAt := time.Now()
	err := cmd.Run()
	result.duration = time.Since(startedAt)
	if streamWriter != nil {
		streamWriter.Flush()
		result.liveLog = streamWriter.snapshot()
		result.lastSummary = streamWriter.lastSummary
		result.streamResult = streamWriter.finalAGYResult()
	}
	return result, err
}
