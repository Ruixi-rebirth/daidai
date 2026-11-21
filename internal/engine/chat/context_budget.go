package chat

import (
	"encoding/json"
	"log/slog"
	"unicode/utf8"

	"daidai/internal/core"
)

// maxPromptPayloadRunes bounds the assembled text sent to a model, including
// tool schemas. It is an input-size guard, not a limit on user-visible output.
const maxPromptPayloadRunes = 64 * 1024
const providerFramingReserveRunes = 2048

// fitPromptContext keeps the current request and this turn's tool results
// intact. If the complete payload grows too large, it removes optional
// episodic context first, then older complete turns from working history.
func fitPromptContext(messages []core.ChatMessage, historyStart, historyEnd int, tools []core.ToolDefinition) []core.ChatMessage {
	if historyStart < 0 || historyEnd < historyStart || historyEnd > len(messages) {
		historyStart, historyEnd = 0, 0
	}

	before := estimatePromptPayloadRunes(messages, tools)
	if before <= maxPromptPayloadRunes {
		return messages
	}
	if historyStart == historyEnd {
		slog.Warn("cortex: 当前请求与必要上下文超过 prompt 预算，保留完整内容",
			"estimatedRunes", before,
			"budgetRunes", maxPromptPayloadRunes,
		)
		return messages
	}

	start := historyStart
	after := before
	for after > maxPromptPayloadRunes && start < historyEnd {
		// A working-history window contains ordinary user/assistant messages.
		// Drop through the next user message so the retained suffix never starts
		// with an assistant answer whose question was removed.
		after -= promptMessageRunes(messages[start])
		start++
		for start < historyEnd && messages[start].Role != core.RoleUser {
			after -= promptMessageRunes(messages[start])
			start++
		}
	}

	slog.Info("cortex: prompt 超出上下文预算，已移除可选背景或较早聊天记录",
		"removedContextMessages", start-historyStart,
		"estimatedRunesBefore", before,
		"estimatedRunesAfter", after,
	)
	if after > maxPromptPayloadRunes {
		// Never silently truncate the current request or tool evidence. Leave the
		// remaining payload complete and make the over-budget condition visible.
		slog.Warn("cortex: 当前请求与必要上下文仍超过 prompt 预算，保留完整内容",
			"estimatedRunes", after,
			"budgetRunes", maxPromptPayloadRunes,
		)
	}
	trimmed := make([]core.ChatMessage, 0, len(messages)-(start-historyStart))
	trimmed = append(trimmed, messages[:historyStart]...)
	trimmed = append(trimmed, messages[start:]...)
	return trimmed
}

func estimatePromptPayloadRunes(messages []core.ChatMessage, tools []core.ToolDefinition) int {
	total := 0
	for _, message := range messages {
		total += promptMessageRunes(message)
	}
	if len(tools) > 0 {
		if encoded, err := json.Marshal(tools); err == nil {
			total += utf8.RuneCount(encoded)
		}
	}
	// Leave room for provider-specific message framing and AGY's text protocol.
	return total + providerFramingReserveRunes
}

func promptPayloadExceedsBudget(messages []core.ChatMessage, tools []core.ToolDefinition) bool {
	return estimatePromptPayloadRunes(messages, tools) > maxPromptPayloadRunes
}

func promptMessageRunes(message core.ChatMessage) int {
	total := utf8.RuneCountInString(message.Content) + utf8.RuneCountInString(message.Name) + utf8.RuneCountInString(message.ToolCallID)
	for _, call := range message.ToolCalls {
		total += utf8.RuneCountInString(call.ID) + utf8.RuneCountInString(call.Function.Name) + utf8.RuneCountInString(call.Function.Arguments)
	}
	return total
}
