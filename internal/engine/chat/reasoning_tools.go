package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"

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

func (c *Cortex) executeToolBatch(
	toolCtx context.Context,
	activeTools []core.ToolDefinition,
	calls []core.ToolCall,
	used int,
	processedLinks map[string]struct{},
	seenCalls map[string]struct{},
	media *toolMediaDelivery,
	presentation *thinkPresentation,
) (toolBatchResult, error) {
	result := toolBatchResult{toolCallsUsed: used}
	if seenCalls == nil {
		seenCalls = make(map[string]struct{}, len(calls))
	}
	for _, call := range calls {
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
		if !allowToolLinks(call, processedLinks, &result) {
			continue
		}
		if c.hands == nil {
			result.appendToolMessage(call, "工具系统未配置，无法执行此调用。")
			continue
		}

		toolResult, toolErr := c.hands.Execute(toolCtx, call.Function.Name, json.RawMessage(call.Function.Arguments))
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

func allowToolLinks(call core.ToolCall, processedLinks map[string]struct{}, result *toolBatchResult) bool {
	switch call.Function.Name {
	case "screenshot_web", "read_link":
	default:
		return true
	}
	var args struct {
		URL string `json:"url"`
	}
	if json.Unmarshal([]byte(call.Function.Arguments), &args) != nil {
		return true
	}
	target, err := core.ExternalURLBudgetKey(args.URL)
	if err != nil {
		return true
	}
	if _, seen := processedLinks[target]; seen {
		return true
	}
	if len(processedLinks) >= 2 {
		slog.Info("cortex: 拦截超限的网页处理调用", "name", call.Function.Name, "uniqueLinks", len(processedLinks))
		result.appendToolMessage(call, fmt.Sprintf("本轮最多处理两个不同网页链接，已跳过本次 %s 调用。", call.Function.Name))
		return false
	}
	processedLinks[target] = struct{}{}
	return true
}
