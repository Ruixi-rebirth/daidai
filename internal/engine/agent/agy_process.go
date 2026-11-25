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
)

const agyOutputLimit = 2 << 20

type agyProcessResult struct {
	stdout      *core.LimitedBuffer
	stderr      *core.LimitedBuffer
	duration    time.Duration
	liveLog     string
	lastSummary string
}

type agyStreamHooks struct {
	onConversationID func(string)
	onLiveProgress   func(liveLog, summary string)
}

type agyLiveStreamWriter struct {
	hooks          agyStreamHooks
	lineBuf        bytes.Buffer
	steps          []string
	stepIdxByOrder map[int]int
	currRespStep   int
	currRespBuf    strings.Builder
	lastSummary    string
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
			if w.lineBuf.Len()+len(p) <= 256<<10 {
				_, _ = w.lineBuf.Write(p)
			}
			break
		}
		if w.lineBuf.Len()+idx <= 256<<10 {
			_, _ = w.lineBuf.Write(p[:idx])
			w.consumeLine(w.lineBuf.Bytes())
		}
		w.lineBuf.Reset()
		p = p[idx+1:]
	}
	return total, nil
}

func (w *agyLiveStreamWriter) consumeLine(raw []byte) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return
	}
	var env struct {
		Event          string `json:"event"`
		ConversationID string `json:"conversation_id"`
		StepUpdate     *struct {
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
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return
	}
	switch env.Event {
	case "init":
		if cid := strings.TrimSpace(env.ConversationID); cid != "" && w.hooks.onConversationID != nil {
			w.hooks.onConversationID(cid)
		}
	case "step_update":
		su := env.StepUpdate
		if su == nil {
			return
		}
		if cid := strings.TrimSpace(su.ConversationID); cid != "" && w.hooks.onConversationID != nil {
			w.hooks.onConversationID(cid)
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
			w.currRespBuf.WriteString(su.TextDelta)
			if cur := strings.TrimSpace(w.currRespBuf.String()); cur != "" {
				w.lastSummary = cur
			}
			w.emit(strings.EqualFold(su.State, "DONE"))
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
		result.liveLog = streamWriter.snapshot()
		result.lastSummary = streamWriter.lastSummary
	}
	return result, err
}
