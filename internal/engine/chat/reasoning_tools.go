package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"daidai/internal/core"
	"daidai/internal/entity"
)

type toolBatchResult struct {
	messages        []core.ChatMessage
	toolCallsUsed   int
	uniqueToolCalls int
	hasToolResult   bool
	needsSynthesis  bool
	directReply     string
	preparedViews   int
}

func toolCallIdentity(call core.ToolCall) string {
	name := strings.TrimSpace(call.Function.Name)
	if name == "" || !json.Valid([]byte(call.Function.Arguments)) {
		return ""
	}
	decoder := json.NewDecoder(strings.NewReader(call.Function.Arguments))
	decoder.UseNumber()
	var arguments any
	if err := decoder.Decode(&arguments); err != nil {
		return ""
	}
	canonical, err := json.Marshal(arguments)
	if err != nil {
		return ""
	}
	return name + "\x00" + string(canonical)
}

func toolArgumentProblem(arguments string) string {
	if len(arguments) > maxToolArgumentsBytes {
		return fmt.Sprintf("参数超过 %d 字节上限", maxToolArgumentsBytes)
	}
	trimmed := strings.TrimSpace(arguments)
	if !strings.HasPrefix(trimmed, "{") || !json.Valid([]byte(arguments)) {
		return "参数必须是有效的 JSON 对象"
	}
	return ""
}

// Keep the call ID for pairing with its rejection without carrying the
// rejected payload into subsequent model requests or draft storage.
func toolCallsForTranscript(calls []core.ToolCall) []core.ToolCall {
	recorded := slices.Clone(calls)
	for i := range recorded {
		if toolArgumentProblem(recorded[i].Function.Arguments) != "" {
			recorded[i].Function.Arguments = "{}"
		}
	}
	return recorded
}

type toolCallOutcome struct {
	res *core.ToolResult
	err error
}

func isConcurrentReadOnlyTool(name string) bool {
	switch name {
	case "search_web", "read_link", "screenshot_web", "search_nix_package":
		return true
	default:
		return false
	}
}

func planConcurrentReadOnlyCalls(
	activeTools []core.ToolDefinition,
	calls []core.ToolCall,
	used int,
	seenCalls map[string]struct{},
) map[int]struct{} {
	if len(calls) < 2 {
		return nil
	}
	plannedSeen := make(map[string]struct{}, len(calls))
	runnable := make(map[int]struct{}, len(calls))
	simUsed := used
	for i, call := range calls {
		identity := toolCallIdentity(call)
		if identity != "" {
			if _, dup := seenCalls[identity]; dup {
				continue
			}
			// If the batch repeats an identical call, run sequentially so
			// first-attempt failures can retry cleanly and successes deduplicate.
			if _, dup := plannedSeen[identity]; dup {
				return nil
			}
		}
		if simUsed >= maxToolCallsPerThink {
			continue
		}
		simUsed++
		if toolArgumentProblem(call.Function.Arguments) != "" || !toolAvailable(activeTools, call.Function.Name) {
			continue
		}
		if !isConcurrentReadOnlyTool(call.Function.Name) {
			return nil
		}
		if identity != "" {
			plannedSeen[identity] = struct{}{}
		}
		runnable[i] = struct{}{}
	}
	if len(runnable) < 2 {
		return nil
	}
	return runnable
}

func (c *Cortex) executeToolBatch(
	toolCtx context.Context,
	activeTools []core.ToolDefinition,
	calls []core.ToolCall,
	used int,
	seenCalls map[string]struct{},
	media *toolMediaDelivery,
	presentation *thinkPresentation,
) (toolBatchResult, error) {
	result := toolBatchResult{toolCallsUsed: used}
	if seenCalls == nil {
		seenCalls = make(map[string]struct{}, len(calls))
	}
	var preExecuted map[int]toolCallOutcome
	if c.hands != nil {
		if plan := planConcurrentReadOnlyCalls(activeTools, calls, used, seenCalls); len(plan) >= 2 {
			outcomes := make([]toolCallOutcome, len(calls))
			var wg sync.WaitGroup
			for idx := range plan {
				wg.Add(1)
				go func(i int, call core.ToolCall) {
					defer wg.Done()
					res, err := c.hands.Execute(toolCtx, call.Function.Name, json.RawMessage(call.Function.Arguments))
					outcomes[i] = toolCallOutcome{res: res, err: err}
				}(idx, calls[idx])
			}
			wg.Wait()
			if err := toolCtx.Err(); err != nil {
				return result, err
			}
			preExecuted = make(map[int]toolCallOutcome, len(plan))
			for idx := range plan {
				preExecuted[idx] = outcomes[idx]
			}
		}
	}
	for i, call := range calls {
		if err := toolCtx.Err(); err != nil {
			return result, err
		}
		identity := toolCallIdentity(call)
		if identity != "" {
			if _, duplicate := seenCalls[identity]; duplicate {
				result.appendToolMessage(call, "本轮已执行过相同参数的工具调用，跳过重复执行。", false)
				continue
			}
		}
		result.uniqueToolCalls++
		if result.toolCallsUsed >= maxToolCallsPerThink {
			result.appendToolMessage(call, fmt.Sprintf("本次认知最多执行 %d 个工具调用，后续调用已跳过。", maxToolCallsPerThink))
			continue
		}
		result.toolCallsUsed++
		slog.Info("cortex: 大脑触发工具调用", "name", call.Function.Name, "argsBytes", len(call.Function.Arguments))
		if problem := toolArgumentProblem(call.Function.Arguments); problem != "" {
			result.appendToolMessage(call, fmt.Sprintf("工具 %s 调用已拒绝：%s。", call.Function.Name, problem))
			continue
		}
		if !toolAvailable(activeTools, call.Function.Name) {
			result.appendToolMessage(call, fmt.Sprintf("工具 %s 未在当前推理轮次开放，调用已拒绝。", call.Function.Name))
			continue
		}
		if c.hands == nil {
			result.appendToolMessage(call, "工具系统未配置，无法执行此调用。")
			continue
		}

		var (
			toolResult *core.ToolResult
			toolErr    error
		)
		if outcome, ok := preExecuted[i]; ok {
			toolResult, toolErr = outcome.res, outcome.err
		} else {
			toolResult, toolErr = c.hands.Execute(toolCtx, call.Function.Name, json.RawMessage(call.Function.Arguments))
		}
		if err := toolCtx.Err(); err != nil {
			return result, err
		}
		if toolErr != nil {
			result.appendToolMessage(call, truncateToolOutput(call.Function.Name, fmt.Sprintf("工具 %s 执行失败：%v", call.Function.Name, toolErr)))
			continue
		}
		if toolResult == nil {
			result.appendToolMessage(call, "工具未返回结果。")
			continue
		}
		// A rejected or failed attempt must remain retryable in this turn. Record
		// the identity only after Execute returned a usable result.
		if identity != "" {
			seenCalls[identity] = struct{}{}
		}

		firstImage := len(media.imageSources)
		hasDelivery := media.absorb(call.Function.Name, toolResult, presentation)
		prepared := hasDelivery && toolResult.ImageOnly
		if prepared {
			result.preparedViews++
		}
		result.hasToolResult = true
		output := toolResult.Text
		if prepared {
			output = "该结果已由程序排版，将随回复交付。"
		} else if output == "" {
			output = "工具未提供文字结果。"
			if hasDelivery {
				output = "工具已准备好待发送的内容。"
			}
		}
		if len(calls) == 1 && toolResult.DirectReply {
			result.directReply = toolResult.Text
		}
		result.appendToolMessage(call, truncateToolOutput(call.Function.Name, output), !prepared)
		result.messages[len(result.messages)-1].PageProgress = toolResult.PageProgress
		if prepared {
			evidence := strings.TrimSpace(toolResult.FallbackText)
			if evidence == "" {
				sources := media.imageSources[firstImage:]
				evidence, _ = entity.ImageDeliveryText(sources, len(sources))
			}
			caption := strings.TrimSpace(toolResult.Caption)
			if evidence == "" && caption == "" {
				evidence = toolResult.Text
			}
			if evidence == caption {
				evidence = ""
			}
			result.messages[len(result.messages)-1].RecoveryContent = strings.TrimSpace(
				truncateToolOutput(call.Function.Name, evidence) + "\n\n" + truncateToolOutput(call.Function.Name, caption),
			)
		}
	}
	return result, nil
}

func (r *toolBatchResult) appendToolMessage(call core.ToolCall, content string, needsSynthesis ...bool) {
	if len(needsSynthesis) == 0 || needsSynthesis[0] {
		r.needsSynthesis = true
	}
	r.messages = append(r.messages, core.ChatMessage{
		Role:       core.RoleTool,
		ToolCallID: call.ID,
		Name:       call.Function.Name,
		Content:    content,
	})
}
