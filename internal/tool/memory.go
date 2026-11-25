package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"daidai/internal/core"
	"daidai/internal/db"
	"daidai/internal/entity"
)

// MemoryTool 长期记忆主动管理工具 (实现 core.Tool)
type MemoryTool struct {
	db          *db.Manager
	llm         core.LLMProvider
	pageMu      sync.Mutex
	profilePage map[string]int
}

const (
	memoryFactsPerPage        = 12
	maxMemorySearchQueryRunes = 400
)

// NewMemoryTool 实例化长期记忆管理工具
func NewMemoryTool(dbMgr *db.Manager, providers ...core.LLMProvider) *MemoryTool {
	var provider core.LLMProvider
	if len(providers) > 0 {
		provider = providers[0]
	}
	return &MemoryTool{db: dbMgr, llm: provider, profilePage: make(map[string]int)}
}

func (t *MemoryTool) Name() string {
	return "manage_memory"
}

func (t *MemoryTool) Description() string {
	return "查看和管理当前会话可见的个人记忆。用户问你对本人有什么印象、是否记得本人或记得本人的哪些事，调用 profile 交付印象与记忆卡片，即使用户没有提到卡片。群聊里用户明确 @ 一位成员并询问你记不记得对方时，profile 可查询该成员在当前群的记忆；必须传该 @ 对象的 target_user_id。想核对具体的一条记忆时用 search 或 source。私聊和各群的记忆按会话隔离。"
}

func (t *MemoryTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"add", "update", "delete", "search", "list", "profile", "source"},
				"description": "profile：查看当前发言人自己的印象与记忆；在群里明确询问某位群友时，可通过 target_user_id 查看本群对该成员的记忆。查询他人必须是当前消息明确 @ 的成员，且只能读取本群范围，不得读取其私聊或其他群记忆。list/search/source/add/update/delete 始终只作用于当前发言人。需要翻页时传 page 或 page_direction。",
			},
			"fact": map[string]any{
				"type":        "string",
				"description": "新增或更新后的简明事实陈述（add 和 update 时必填）",
			},
			"fact_id": map[string]any{
				"type":        "integer",
				"description": "既有事实 ID（对应列表中 [ID: x]；update、delete、source 时必填）",
			},
			"target_user_id": map[string]any{
				"type":        "string",
				"description": "仅 profile 使用。查看他人时，必须填写当前消息明确 @ 的群成员 ID；服务端会校验该 ID 确实来自本条消息的 @ 元数据。",
			},
			"query": map[string]any{
				"type":        "string",
				"maxLength":   maxMemorySearchQueryRunes,
				"description": "检索关键词（action 为 search 时使用）",
			},
			"page": map[string]any{
				"type":        "integer",
				"minimum":     1,
				"description": "profile、list、search、source 页码（从 1 开始）；search 翻页时沿用相同 query，用户说下一页、上一页或指定页码时按上下文传入",
			},
			"page_direction": map[string]any{
				"type":        "string",
				"enum":        []string{"next", "previous", "first"},
				"description": "profile、list、search、source 页码方向；翻页时按此参数移动当前列表页，用户指定 page 时优先使用 page",
			},
		},
		"required": []string{"action"},
	}
}

type memoryArgs struct {
	Action        string `json:"action"`
	TargetUserID  string `json:"target_user_id,omitempty"`
	Fact          string `json:"fact"`
	FactID        int64  `json:"fact_id"`
	Query         string `json:"query"`
	Page          int    `json:"page"`
	PageDirection string `json:"page_direction"`
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
	if event.IsGroup && strings.TrimSpace(event.ChannelID) == "" {
		return &core.ToolResult{Text: "无法识别当前群聊会话，暂不读取或修改群聊记忆。"}, nil
	}

	action := strings.ToUpper(strings.TrimSpace(args.Action))
	factText := strings.TrimSpace(args.Fact)
	scopeType, scopeID := memoryScope(event)

	switch action {
	case "PROFILE":
		targetID := strings.TrimSpace(args.TargetUserID)
		targetName := ""
		if targetID != "" {
			if !event.IsGroup {
				return &core.ToolResult{Text: "查看其他人的记忆只支持群聊中的明确 @，并且仅限本群记忆。"}, nil
			}
			var mentioned bool
			for _, user := range event.MentionedUsers {
				if strings.TrimSpace(user.ID) == targetID {
					mentioned = true
					targetName = strings.TrimSpace(user.Name)
					break
				}
			}
			if !mentioned {
				return &core.ToolResult{Text: "我只能查看当前消息里明确 @ 的群成员；请 @ 一位群友再问。"}, nil
			}
		}
		if targetID == "" && isThirdPartyImpressionQuery(event.Content, args.Query) {
			if !event.IsGroup || len(event.MentionedUsers) != 1 {
				return &core.ToolResult{Text: "要查某位群友的记忆，请在群里明确 @ 那个人；我只会查看本群范围内的记忆。"}, nil
			}
			targetID = strings.TrimSpace(event.MentionedUsers[0].ID)
			targetName = strings.TrimSpace(event.MentionedUsers[0].Name)
			if targetID == "" {
				return &core.ToolResult{Text: "没能识别被 @ 的群成员，请重新 @ 一下再问。"}, nil
			}
		}
		if t.db == nil {
			return &core.ToolResult{Text: "长期记忆数据库未配置。"}, nil
		}
		return t.profile(ctx, event, args.Page, args.PageDirection, targetID, targetName)
	case "SOURCE":
		if args.FactID <= 0 {
			return &core.ToolResult{Text: "参数错误：source 操作必须提供有效的 fact_id。"}, nil
		}
		if t.db == nil {
			return &core.ToolResult{Text: "长期记忆数据库未配置。"}, nil
		}
		return t.sources(ctx, event, args)

	case "ADD":
		if factText == "" {
			return &core.ToolResult{Text: "参数错误：add 操作必须提供非空的 fact 内容。"}, nil
		}
		if utf8.RuneCountInString(factText) > core.MaxFactContentRunes {
			return &core.ToolResult{Text: fmt.Sprintf("参数错误：fact 内容不能超过 %d 个字符。", core.MaxFactContentRunes)}, nil
		}
		if t.db == nil {
			return &core.ToolResult{Text: "长期记忆数据库未配置。"}, nil
		}
		err := t.db.ApplyFactMutations(ctx, event.Platform, event.SenderID, []db.FactMutation{
			{Action: "ADD", SourceMessageIDs: event.HistoryRecordIDs, Fact: factText, ScopeType: scopeType, ScopeID: scopeID, Quote: event.Content},
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
		if t.db == nil {
			return &core.ToolResult{Text: "长期记忆数据库未配置。"}, nil
		}
		target, err := t.visibleFact(ctx, event, args.FactID)
		if err != nil {
			return nil, err
		}
		if !factMatchesScope(target, scopeType, scopeID) {
			return &core.ToolResult{Text: "群聊中只能更新当前用户在本群记录的个人记忆；全局记忆请在私聊中操作。"}, nil
		}
		err = t.db.ApplyFactMutations(ctx, event.Platform, event.SenderID, []db.FactMutation{
			{Action: "UPDATE", FactID: args.FactID, SourceMessageIDs: event.HistoryRecordIDs, Fact: factText, ExpectedFact: target.Content, ScopeType: target.ScopeType, ScopeID: target.ScopeID, Quote: event.Content},
		})
		if err != nil {
			return &core.ToolResult{Text: fmt.Sprintf("更新记忆失败：%v", err)}, nil
		}
		return &core.ToolResult{Text: fmt.Sprintf("已将记忆 [ID: %d] 更新为：%s", args.FactID, factText)}, nil

	case "DELETE":
		if args.FactID <= 0 {
			return &core.ToolResult{Text: "参数错误：delete 操作必须提供有效的 fact_id。"}, nil
		}
		if t.db == nil {
			return &core.ToolResult{Text: "长期记忆数据库未配置。"}, nil
		}
		target, err := t.visibleFact(ctx, event, args.FactID)
		if err != nil {
			return nil, err
		}
		if !factMatchesScope(target, scopeType, scopeID) {
			return &core.ToolResult{Text: "群聊中只能删除当前用户在本群记录的个人记忆；全局记忆请在私聊中操作。"}, nil
		}
		err = t.db.ApplyFactMutations(ctx, event.Platform, event.SenderID, []db.FactMutation{
			{Action: "DELETE", FactID: args.FactID, SourceMessageIDs: event.HistoryRecordIDs, ExpectedFact: target.Content, ScopeType: target.ScopeType, ScopeID: target.ScopeID, Quote: event.Content},
		})
		if err != nil {
			return &core.ToolResult{Text: fmt.Sprintf("删除记忆失败：%v", err)}, nil
		}
		return &core.ToolResult{Text: fmt.Sprintf("已忘掉记忆 [ID: %d]", args.FactID)}, nil

	case "LIST", "SEARCH":
		if action == "SEARCH" && utf8.RuneCountInString(strings.TrimSpace(args.Query)) > maxMemorySearchQueryRunes {
			return &core.ToolResult{Text: fmt.Sprintf("记忆搜索关键词不能超过 %d 个字符，请提炼成简短关键词后重试。", maxMemorySearchQueryRunes)}, nil
		}
		if t.db == nil {
			return &core.ToolResult{Text: "长期记忆数据库未配置。"}, nil
		}
		return t.listOrSearch(ctx, event, args, action)
	default:
		return &core.ToolResult{Text: fmt.Sprintf("未知的记忆操作类型: %s", args.Action)}, nil
	}
}

func (t *MemoryTool) sources(ctx context.Context, event *entity.SensoryEvent, args memoryArgs) (*core.ToolResult, error) {
	const pageSize = 5
	key := fmt.Sprintf("%s:source:%d", profilePageKey(event), args.FactID)
	page := t.resolveProfilePage(key, args.Page, args.PageDirection)
	sources, total, err := t.db.PageVisibleFactSources(ctx, event.Platform, event.SenderID, event.ChannelID, event.IsGroup, args.FactID, page, pageSize)
	if err != nil {
		return nil, fmt.Errorf("读取记忆来源失败: %w", err)
	}
	if total == 0 {
		return &core.ToolResult{Text: fmt.Sprintf("记忆 ID %d 没有可查看的来源记录，或它不属于当前会话可见范围。", args.FactID)}, nil
	}
	totalPages := (total + pageSize - 1) / pageSize
	resolved, atLastPage := t.reconcileProfilePage(key, page, totalPages, args.Page, args.PageDirection)
	if atLastPage {
		return &core.ToolResult{Text: fmt.Sprintf("记忆 ID %d 的来源已到最后一页（共 %d 页）。", args.FactID, totalPages)}, nil
	}
	if resolved > totalPages {
		return &core.ToolResult{Text: fmt.Sprintf("记忆 ID %d 的来源共 %d 页，没有第 %d 页。", args.FactID, totalPages, resolved)}, nil
	}
	if resolved != page {
		sources, _, err = t.db.PageVisibleFactSources(ctx, event.Platform, event.SenderID, event.ChannelID, event.IsGroup, args.FactID, resolved, pageSize)
		if err != nil {
			return nil, fmt.Errorf("重新读取记忆来源失败: %w", err)
		}
	}
	t.setProfilePage(key, resolved)
	lines := []string{fmt.Sprintf("记忆 ID %d 的来源 · 第 %d/%d 页 · 共 %d 条：", args.FactID, resolved, totalPages, total)}
	for _, source := range sources {
		text := core.TruncateRunes(singleLineText(source.Text), 300, "…")
		if source.Excerpt {
			lines = append(lines, "- 保存的来源摘录（未关联原消息）：“"+text+"”")
		} else {
			lines = append(lines, fmt.Sprintf("- %s · 消息 %d：“%s”", source.CreatedAt.Format("2006-01-02 15:04"), source.MessageID, text))
		}
	}
	return &core.ToolResult{Text: strings.Join(lines, "\n")}, nil
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
	return fact.ScopeType == scopeType && fact.ScopeID == scopeID
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

func isThirdPartyImpressionQuery(content, query string) bool {
	combined := strings.TrimSpace(content + " " + query)
	if combined == "" {
		return false
	}
	thirdPartyKeywords := []string{"主人", "群主", "管理员", "别人", "他", "她", "它们", "他们", "她们"}
	hasThirdParty := false
	for _, kw := range thirdPartyKeywords {
		if strings.Contains(combined, kw) {
			hasThirdParty = true
			break
		}
	}
	if !hasThirdParty {
		return false
	}
	if strings.Contains(combined, "对我") || strings.Contains(combined, "我的印象") || strings.Contains(combined, "眼中的我") {
		return false
	}
	return true
}
