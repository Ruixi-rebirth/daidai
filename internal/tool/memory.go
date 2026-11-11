package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"daidai/internal/core"
	"daidai/internal/db"
	"daidai/internal/engine/memory"
	"daidai/internal/entity"
)

// MemoryTool 长期记忆主动管理工具 (实现 core.Tool)
type MemoryTool struct {
	db *db.Manager
}

// NewMemoryTool 实例化长期记忆管理工具
func NewMemoryTool(dbMgr *db.Manager) *MemoryTool {
	if dbMgr == nil {
		dbMgr = db.DefaultManager()
	}
	return &MemoryTool{db: dbMgr}
}

func (t *MemoryTool) Name() string {
	return "manage_memory"
}

func (t *MemoryTool) Description() string {
	return "管理当前会话可见的长期记忆：私聊维护全局事实，群聊维护本群事实，并可检索或列出当前可见事实。"
}

func (t *MemoryTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"add", "update", "delete", "search", "list"},
				"description": "操作类型：add（新增当前范围事实）、update（按 fact_id 更新当前范围事实）、delete（按 fact_id 删除当前范围事实）、search（检索当前可见事实和会话历史）、list（列出当前会话可见事实）",
			},
			"fact": map[string]any{
				"type":        "string",
				"description": "新增或更新后的简明事实陈述（add 和 update 时必填）",
			},
			"fact_id": map[string]any{
				"type":        "integer",
				"description": "要更新或删除的既有事实 ID（对应长期记忆列表中 [ID: x] 的数字编号，update 和 delete 时必填）",
			},
			"query": map[string]any{
				"type":        "string",
				"description": "检索关键词（action 为 search 时使用）",
			},
		},
		"required": []string{"action"},
	}
}

type memoryArgs struct {
	Action string `json:"action"`
	Fact   string `json:"fact"`
	FactID int64  `json:"fact_id"`
	Query  string `json:"query"`
}

func (t *MemoryTool) Execute(ctx context.Context, rawArgs json.RawMessage) (*core.ToolResult, error) {
	var args memoryArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, fmt.Errorf("invalid manage_memory args: %w", err)
	}

	event := entity.SensoryEventFromContext(ctx)
	if event == nil || event.Platform == "" || event.SenderID == "" {
		return &core.ToolResult{Text: "无法识别当前对话用户身份，暂无法操作长期记忆。"}, nil
	}

	action := strings.ToUpper(strings.TrimSpace(args.Action))
	factText := strings.TrimSpace(args.Fact)
	scopeType, scopeID := memoryScope(event)

	switch action {
	case "ADD":
		if factText == "" {
			return &core.ToolResult{Text: "参数错误：add 操作必须提供非空的 fact 内容。"}, nil
		}
		if utf8.RuneCountInString(factText) > core.MaxFactContentRunes {
			return &core.ToolResult{Text: fmt.Sprintf("参数错误：fact 内容不能超过 %d 个字符。", core.MaxFactContentRunes)}, nil
		}
		factVec := memory.EmbedText(ctx, factText)
		existing, err := t.db.ListVisibleFacts(ctx, event.Platform, event.SenderID, event.ChannelID, event.IsGroup)
		if err != nil {
			return nil, fmt.Errorf("读取长期记忆失败: %w", err)
		}
		for _, f := range existing {
			if !factMatchesScope(f, scopeType, scopeID) {
				continue
			}
			if f.Content == factText {
				return &core.ToolResult{Text: fmt.Sprintf("该记忆已存在 [ID: %d]：%s", f.ID, f.Content)}, nil
			}
		}
		for _, f := range existing {
			if !factMatchesScope(f, scopeType, scopeID) {
				continue
			}
			if memory.LexicalOverlap(factText, f.Content) >= 0.78 ||
				(len(factVec) > 0 && len(f.Embedding) > 0 && memory.CosineSimilarity(factVec, f.Embedding) >= 0.88) {
				if err := t.db.ApplyFactMutations(ctx, event.Platform, event.SenderID, []db.FactMutation{
					{Action: "UPDATE", FactID: f.ID, Fact: factText, ExpectedFact: f.Content, ScopeType: scopeType, ScopeID: scopeID, Quote: event.Content, Embedding: factVec},
				}); err != nil {
					return nil, fmt.Errorf("更新近似记忆失败: %w", err)
				}
				return &core.ToolResult{Text: fmt.Sprintf("已合并更新近似记忆 [ID: %d] 为：%s", f.ID, factText)}, nil
			}
		}
		err = t.db.ApplyFactMutations(ctx, event.Platform, event.SenderID, []db.FactMutation{
			{Action: "ADD", Fact: factText, ScopeType: scopeType, ScopeID: scopeID, Quote: event.Content, Embedding: factVec},
		})
		if err != nil {
			return nil, fmt.Errorf("写入长期记忆失败: %w", err)
		}
		return &core.ToolResult{Text: fmt.Sprintf("已记下长期记忆：%s", factText)}, nil

	case "UPDATE":
		if args.FactID <= 0 || factText == "" {
			return &core.ToolResult{Text: "参数错误：update 操作必须提供有效的 fact_id 与新的 fact 内容。"}, nil
		}
		if utf8.RuneCountInString(factText) > core.MaxFactContentRunes {
			return &core.ToolResult{Text: fmt.Sprintf("参数错误：fact 内容不能超过 %d 个字符。", core.MaxFactContentRunes)}, nil
		}
		target, err := t.visibleFact(ctx, event, args.FactID)
		if err != nil {
			return nil, err
		}
		if !factMatchesScope(target, scopeType, scopeID) {
			return &core.ToolResult{Text: "群聊中只能更新本群记忆；全局记忆请在私聊中操作。"}, nil
		}
		factVec := memory.EmbedText(ctx, factText)
		err = t.db.ApplyFactMutations(ctx, event.Platform, event.SenderID, []db.FactMutation{
			{Action: "UPDATE", FactID: args.FactID, Fact: factText, ExpectedFact: target.Content, ScopeType: target.ScopeType, ScopeID: target.ScopeID, Quote: event.Content, Embedding: factVec},
		})
		if err != nil {
			return &core.ToolResult{Text: fmt.Sprintf("更新记忆失败：%v", err)}, nil
		}
		return &core.ToolResult{Text: fmt.Sprintf("已将记忆 [ID: %d] 更新为：%s", args.FactID, factText)}, nil

	case "DELETE":
		if args.FactID <= 0 {
			return &core.ToolResult{Text: "参数错误：delete 操作必须提供有效的 fact_id。"}, nil
		}
		target, err := t.visibleFact(ctx, event, args.FactID)
		if err != nil {
			return nil, err
		}
		if !factMatchesScope(target, scopeType, scopeID) {
			return &core.ToolResult{Text: "群聊中只能删除本群记忆；全局记忆请在私聊中操作。"}, nil
		}
		err = t.db.ApplyFactMutations(ctx, event.Platform, event.SenderID, []db.FactMutation{
			{Action: "DELETE", FactID: args.FactID, ExpectedFact: target.Content, ScopeType: target.ScopeType, ScopeID: target.ScopeID, Quote: event.Content},
		})
		if err != nil {
			return &core.ToolResult{Text: fmt.Sprintf("删除记忆失败：%v", err)}, nil
		}
		return &core.ToolResult{Text: fmt.Sprintf("已忘掉记忆 [ID: %d]", args.FactID)}, nil

	case "LIST", "SEARCH":
		facts, err := t.db.ListVisibleFacts(ctx, event.Platform, event.SenderID, event.ChannelID, event.IsGroup)
		if err != nil {
			return nil, fmt.Errorf("读取长期记忆失败: %w", err)
		}
		q := strings.TrimSpace(args.Query)
		if action == "SEARCH" && q != "" {
			facts = memory.RankFactsByBM25(ctx, q, facts, 0.25)
		}
		var factLines []string
		for _, f := range facts {
			label := "全局"
			if f.ScopeType == "group" {
				label = "本群"
			}
			factLines = append(factLines, fmt.Sprintf("- [ID: %d][%s] %s", f.ID, label, f.Content))
		}

		var sections []string
		if len(factLines) > 0 {
			sections = append(sections, fmt.Sprintf("【长期记忆事实 (%d 条)】：\n%s", len(factLines), strings.Join(factLines, "\n")))
		}

		if action == "SEARCH" && q != "" {
			if event.ChannelID == "" {
				sections = append(sections, "【历史对话搜索不可用】当前会话缺少会话 ID。")
			} else {
				chatType := "private"
				if event.IsGroup {
					chatType = "group"
				}
				msgs, mErr := memory.SearchMessagesInChatByBM25(ctx, t.db, event.Platform, chatType, event.ChannelID, q, 10)
				if mErr != nil {
					sections = append(sections, fmt.Sprintf("【历史对话搜索失败】%v", mErr))
				} else if len(msgs) > 0 {
					var msgLines []string
					for _, m := range msgs {
						speaker := "用户"
						if m.IsBot {
							speaker = "呆呆"
						} else if m.Name != "" {
							speaker = m.Name
						}
						text := strings.ReplaceAll(strings.TrimSpace(m.Text), "\n", " ")
						if r := []rune(text); len(r) > 200 {
							text = string(r[:200]) + "..."
						}
						msgLines = append(msgLines, fmt.Sprintf("- [%s %s]: %s", m.CreatedAt.Format("2006-01-02 15:04"), speaker, text))
					}
					sections = append(sections, fmt.Sprintf("【历史对话原文命中 (%d 条)】：\n%s", len(msgLines), strings.Join(msgLines, "\n")))
				}
			}
		}

		if len(sections) == 0 {
			if action == "SEARCH" && q != "" {
				return &core.ToolResult{Text: fmt.Sprintf("未找到与 %q 匹配的长期记忆事实或历史对话记录。", q)}, nil
			}
			return &core.ToolResult{Text: "当前用户暂无已记录的长期记忆事实。"}, nil
		}
		return &core.ToolResult{Text: strings.Join(sections, "\n\n")}, nil

	default:
		return &core.ToolResult{Text: fmt.Sprintf("未知的记忆操作类型: %s", args.Action)}, nil
	}
}

func memoryScope(event *entity.SensoryEvent) (string, string) {
	if event != nil && event.IsGroup {
		return "group", strings.TrimSpace(event.ChannelID)
	}
	return "global", ""
}

func factMatchesScope(fact *core.Fact, scopeType, scopeID string) bool {
	if fact == nil {
		return false
	}
	if fact.ScopeType == "" {
		return scopeType == "global"
	}
	factScopeID := fact.ScopeID
	if factScopeID == "" {
		factScopeID = fact.ConvID
	}
	return fact.ScopeType == scopeType && factScopeID == scopeID
}

func (t *MemoryTool) visibleFact(ctx context.Context, event *entity.SensoryEvent, factID int64) (*core.Fact, error) {
	facts, err := t.db.ListVisibleFacts(ctx, event.Platform, event.SenderID, event.ChannelID, event.IsGroup)
	if err != nil {
		return nil, fmt.Errorf("读取长期记忆失败: %w", err)
	}
	for _, fact := range facts {
		if fact.ID == factID {
			return fact, nil
		}
	}
	return nil, fmt.Errorf("未找到当前会话可见的记忆事实 ID %d", factID)
}
