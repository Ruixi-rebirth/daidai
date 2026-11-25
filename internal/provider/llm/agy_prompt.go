package llm

import (
	"encoding/json"
	"fmt"
	"strings"

	"daidai/internal/core"
)

func (p *AGYMindProvider) assemblePrompt(messages []core.ChatMessage, tools []core.ToolDefinition, readOnly bool) string {
	var sb strings.Builder

	var systemInstructions []string
	if readOnly {
		systemInstructions = append(systemInstructions, "【只读推理】仅处理提供的会话资料与应用工具结果，不读取宿主机代码、配置或敏感环境信息，不执行命令或修改文件；对外正常回应，不把内部受限模式表述成‘没开权限’或‘需改配置’。")
	}
	var historyLines []string
	var toolResults []string
	var currentUserMessage string

	lastUserIdx := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == core.RoleUser {
			lastUserIdx = i
			break
		}
	}

	for i, msg := range messages {
		switch msg.Role {
		case core.RoleSystem:
			if msg.Content != "" {
				systemInstructions = append(systemInstructions, msg.Content)
			}
		case core.RoleTool:
			toolResults = append(toolResults, formatAGYToolResult(msg))
		case core.RoleAssistant:
			if msg.Content != "" {
				historyLines = append(historyLines, fmt.Sprintf("呆呆: %s", msg.Content))
			}
			for _, tc := range msg.ToolCalls {
				arguments := core.TruncateRunes(strings.TrimSpace(tc.Function.Arguments), maxToolTraceArgumentRunes, "…")
				if arguments == "" || arguments == "{}" {
					toolResults = append(toolResults, fmt.Sprintf("[已发起工具调用]: %s", tc.Function.Name))
					continue
				}
				toolResults = append(toolResults, fmt.Sprintf("[已发起工具调用]: %s(参数摘要: %s)", tc.Function.Name, arguments))
			}
		case core.RoleUser:
			if i == lastUserIdx {
				currentUserMessage = msg.Content
			} else if msg.Content != "" {
				historyLines = append(historyLines, fmt.Sprintf("用户: %s", msg.Content))
			}
		}
	}

	if len(systemInstructions) > 0 {
		sb.WriteString("=== 系统角色与运行指令 ===\n")
		sb.WriteString(strings.Join(systemInstructions, "\n\n"))
		sb.WriteString("\n\n")
	}

	if len(historyLines) > 0 {
		sb.WriteString("=== 近期会话记录 ===\n")
		sb.WriteString(strings.Join(historyLines, "\n"))
		sb.WriteString("\n\n")
	}

	if len(tools) > 0 {
		sb.WriteString("=== 可用宿主工具列表 ===\n")
		sb.WriteString("你可以直接完成对话、代码分析或联网检索；若根据用户意图需要使用下列宿主工具，请按协议输出调用指令：\n")
		toolBytes, _ := json.Marshal(tools)
		sb.WriteString(string(toolBytes))
		sb.WriteString("\n\n")

		sb.WriteString("=== 工具调用协议 ===\n")
		sb.WriteString("调用上述宿主工具时，整条回复只输出一个 JSON 代码块，说明文字留到工具返回后：\n")
		sb.WriteString("```json\n{\n  \"tool_calls\": [\n    {\n      \"name\": \"工具名称\",\n      \"arguments\": { \"参数键\": \"参数值\" }\n    }\n  ]\n}\n```\n")
		sb.WriteString("若无需调用上述宿主专属工具，请直接用你自身能力处理并输出自然亲切的中文回复，不要输出 JSON 代码块。\n\n")
	}

	sb.WriteString("=== 用户当前发言 ===\n")
	sb.WriteString(currentUserMessage)

	if len(toolResults) > 0 {
		sb.WriteString("\n\n=== 本轮工具请求与反馈 ===\n")
		sb.WriteString(strings.Join(toolResults, "\n\n"))
		sb.WriteString("\n\n若已有反馈足以回应用户，请直接回答。")
	}

	return sb.String()
}

func agyHostTools(tools []core.ToolDefinition) []core.ToolDefinition {
	var available []core.ToolDefinition
	for _, tool := range tools {
		// AGY already provides native web search.
		if tool.Function.Name != "search_web" {
			available = append(available, tool)
		}
	}
	return available
}

const maxToolTraceArgumentRunes = 240

func formatAGYToolResult(msg core.ChatMessage) string {
	return fmt.Sprintf("[工具 %s 执行结果]: %s", msg.Name, msg.Content)
}

// stripAGYToolEcho removes exact trailing copies of results from the prompt.
func stripAGYToolEcho(text string, messages []core.ChatMessage) string {
	for {
		before := text
		for _, msg := range messages {
			if msg.Role != core.RoleTool {
				continue
			}
			result := strings.TrimSpace(formatAGYToolResult(msg))
			if text == result {
				return ""
			}
			if prefix, ok := strings.CutSuffix(text, "\n"+result); ok {
				text = strings.TrimSpace(prefix)
				break
			}
		}
		if text == before {
			return text
		}
	}
}
