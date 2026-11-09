package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

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
	return "管理关于当前用户的长期记忆事实：支持新增（add）、更新（update）、删除（delete）、检索（search）或列出全部记忆（list）。"
}

func (t *MemoryTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"add", "update", "delete", "search", "list"},
				"description": "操作类型：add（新增事实）、update（按 fact_id 更新既有事实）、delete（按 fact_id 删除既有事实）、search（按 query 检索历史事实）、list（列出该用户全部生效事实）",
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

	switch action {
	case "ADD":
		if factText == "" {
			return &core.ToolResult{Text: "参数错误：add 操作必须提供非空的 fact 内容。"}, nil
		}
		factVec := memory.EmbedText(ctx, factText)
		if existing, err := t.db.ListActiveFacts(ctx, event.Platform, event.SenderID); err == nil {
			for _, f := range existing {
				if f.Content == factText {
					return &core.ToolResult{Text: fmt.Sprintf("该记忆已存在 [ID: %d]：%s", f.ID, f.Content)}, nil
				}
				if memory.LexicalOverlap(factText, f.Content) >= 0.78 ||
					(len(factVec) > 0 && len(f.Embedding) > 0 && memory.CosineSimilarity(factVec, f.Embedding) >= 0.88) {
					_ = t.db.ApplyFactMutations(ctx, event.Platform, event.SenderID, []db.FactMutation{
						{Action: "UPDATE", FactID: f.ID, Fact: factText, Quote: event.Content, Embedding: factVec},
					})
					return &core.ToolResult{Text: fmt.Sprintf("已合并更新近似记忆 [ID: %d] 为：%s", f.ID, factText)}, nil
				}
			}
		}
		err := t.db.ApplyFactMutations(ctx, event.Platform, event.SenderID, []db.FactMutation{
			{Action: "ADD", Fact: factText, Quote: event.Content, Embedding: factVec},
		})
		if err != nil {
			return nil, fmt.Errorf("写入长期记忆失败: %w", err)
		}
		return &core.ToolResult{Text: fmt.Sprintf("已记下长期记忆：%s", factText)}, nil

	case "UPDATE":
		if args.FactID <= 0 || factText == "" {
			return &core.ToolResult{Text: "参数错误：update 操作必须提供有效的 fact_id 与新的 fact 内容。"}, nil
		}
		factVec := memory.EmbedText(ctx, factText)
		err := t.db.ApplyFactMutations(ctx, event.Platform, event.SenderID, []db.FactMutation{
			{Action: "UPDATE", FactID: args.FactID, Fact: factText, Quote: event.Content, Embedding: factVec},
		})
		if err != nil {
			return &core.ToolResult{Text: fmt.Sprintf("更新记忆失败：%v", err)}, nil
		}
		return &core.ToolResult{Text: fmt.Sprintf("已将记忆 [ID: %d] 更新为：%s", args.FactID, factText)}, nil

	case "DELETE":
		if args.FactID <= 0 {
			return &core.ToolResult{Text: "参数错误：delete 操作必须提供有效的 fact_id。"}, nil
		}
		err := t.db.ApplyFactMutations(ctx, event.Platform, event.SenderID, []db.FactMutation{
			{Action: "DELETE", FactID: args.FactID, Quote: event.Content},
		})
		if err != nil {
			return &core.ToolResult{Text: fmt.Sprintf("删除记忆失败：%v", err)}, nil
		}
		return &core.ToolResult{Text: fmt.Sprintf("已忘掉记忆 [ID: %d]", args.FactID)}, nil

	case "LIST", "SEARCH":
		facts, err := t.db.ListActiveFacts(ctx, event.Platform, event.SenderID)
		if err != nil {
			return nil, fmt.Errorf("读取长期记忆失败: %w", err)
		}
		q := strings.TrimSpace(args.Query)
		if action == "SEARCH" && q != "" {
			facts = memory.RankFactsByBM25(q, facts, 0.25)
		}
		var factLines []string
		for _, f := range facts {
			factLines = append(factLines, fmt.Sprintf("- [ID: %d] %s", f.ID, f.Content))
		}

		var sections []string
		if len(factLines) > 0 {
			sections = append(sections, fmt.Sprintf("【长期记忆事实 (%d 条)】：\n%s", len(factLines), strings.Join(factLines, "\n")))
		}

		if action == "SEARCH" && q != "" && event.ChannelID != "" {
			if msgs, mErr := memory.SearchMessagesByBM25(ctx, t.db, event.Platform, event.ChannelID, q, 10); mErr == nil && len(msgs) > 0 {
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
