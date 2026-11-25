package tool

import (
	"context"
	"fmt"
	"strings"

	"daidai/internal/config"
	"daidai/internal/core"
	"daidai/internal/engine/memory"
	"daidai/internal/entity"
)

func (t *MemoryTool) listOrSearch(ctx context.Context, event *entity.SensoryEvent, args memoryArgs, action string) (*core.ToolResult, error) {
	q := strings.TrimSpace(args.Query)
	if action == "SEARCH" && q == "" {
		return &core.ToolResult{Text: "search 操作需要提供 query 关键词；如果要查看当前范围内的全部已确认记忆，请使用 list。"}, nil
	}
	var facts []*core.Fact
	page, totalFacts := 0, 0
	var err error
	if action == "LIST" {
		pageKey := profilePageKey(event)
		page = t.resolveProfilePage(pageKey, args.Page, args.PageDirection)
		facts, totalFacts, err = t.db.PageVisibleFacts(ctx, event.Platform, event.SenderID, event.ChannelID, event.IsGroup, page, memoryFactsPerPage)
		if err != nil {
			return nil, fmt.Errorf("读取长期记忆失败: %w", err)
		}
		totalPages := (totalFacts + memoryFactsPerPage - 1) / memoryFactsPerPage
		if totalPages == 0 {
			totalPages = 1
		}
		page, atLastPage := t.reconcileProfilePage(pageKey, page, totalPages, args.Page, args.PageDirection)
		if atLastPage {
			return &core.ToolResult{Text: fmt.Sprintf("已经是最后一页（共 %d 页）。", totalPages)}, nil
		}
		if page > totalPages {
			return &core.ToolResult{Text: fmt.Sprintf("记忆列表共 %d 页，没有第 %d 页。", totalPages, page)}, nil
		}
		if len(facts) == 0 && totalFacts > 0 {
			facts, _, err = t.db.PageVisibleFacts(ctx, event.Platform, event.SenderID, event.ChannelID, event.IsGroup, page, memoryFactsPerPage)
			if err != nil {
				return nil, fmt.Errorf("重新读取长期记忆失败: %w", err)
			}
		}
	} else {
		facts, err = t.db.ListVisibleFacts(ctx, event.Platform, event.SenderID, event.ChannelID, event.IsGroup)
		if err != nil {
			return nil, fmt.Errorf("读取长期记忆失败: %w", err)
		}
	}
	searchTotalPages, searchTotalMatches, searchPage := 0, 0, 1
	if action == "SEARCH" && q != "" {
		facts = memory.RankFactsByBM25(ctx, q, facts, 0.25)
		searchTotalMatches = len(facts)
		searchTotalPages = (len(facts) + memoryFactsPerPage - 1) / memoryFactsPerPage
		pageKey := memorySearchPageKey(event, q)
		page := t.resolveProfilePage(pageKey, args.Page, args.PageDirection)
		if searchTotalPages == 0 {
			page = 1
			t.setProfilePage(pageKey, page)
		} else if page > searchTotalPages {
			page, atLastPage := t.reconcileProfilePage(pageKey, page, searchTotalPages, args.Page, args.PageDirection)
			if atLastPage {
				return &core.ToolResult{Text: fmt.Sprintf("这组记忆搜索已到最后一页（共 %d 页）。", searchTotalPages)}, nil
			}
			if page > searchTotalPages {
				return &core.ToolResult{Text: fmt.Sprintf("这组记忆搜索共 %d 页，没有第 %d 页。", searchTotalPages, page)}, nil
			}
		} else {
			page, _ = t.reconcileProfilePage(pageKey, page, searchTotalPages, args.Page, args.PageDirection)
		}
		searchPage = page
		maxInt := int(^uint(0) >> 1)
		if page-1 > maxInt/memoryFactsPerPage {
			return &core.ToolResult{Text: "请求的记忆搜索页码过大。"}, nil
		}
		start := (page - 1) * memoryFactsPerPage
		end := start + memoryFactsPerPage
		if end > len(facts) {
			end = len(facts)
		}
		facts = facts[start:end]
		if searchTotalPages > 1 {
			t.setProfilePage(pageKey, page)
		}
	}
	var factLines []string
	for _, f := range facts {
		factLines = append(factLines, fmt.Sprintf("- [ID: %d][%s] %s", f.ID, f.ScopeLabel(), f.Content))
	}

	var sections []string
	if len(factLines) > 0 {
		sections = append(sections, fmt.Sprintf("【长期记忆事实 (%d 条)】：\n%s", len(factLines), strings.Join(factLines, "\n")))
	}
	if action == "LIST" {
		if totalFacts > 0 {
			totalPages := (totalFacts + memoryFactsPerPage - 1) / memoryFactsPerPage
			sections = append(sections, fmt.Sprintf("第 %d/%d 页 · 共 %d 条已确认记忆。可以说‘下一页’或‘上一页’继续浏览。", page, totalPages, totalFacts))
		}
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
					if event.IsGroup && m.IsBot {
						if strings.HasPrefix(memory.GroupPersonalMemoryCardHistoryText(m.Text), "[呆呆已向当时的群成员展示个人记忆卡；") {
							continue
						}
					}
					speaker := "用户"
					if m.IsBot {
						speaker = config.BotName()
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
		if searchTotalPages > 1 {
			sections = append(sections, fmt.Sprintf("记忆搜索第 %d/%d 页 · 共 %d 条命中。可以说‘下一页’或‘上一页’继续浏览。", searchPage, searchTotalPages, searchTotalMatches))
		}
	}

	if len(sections) == 0 {
		if action == "SEARCH" && q != "" {
			return &core.ToolResult{Text: fmt.Sprintf("未找到与 %q 匹配的长期记忆事实或历史对话记录。", q)}, nil
		}
		return &core.ToolResult{Text: "当前用户暂无已记录的长期记忆事实。"}, nil
	}
	if action == "LIST" && totalFacts > 0 {
		t.setProfilePage(profilePageKey(event), page)
	}
	return &core.ToolResult{Text: strings.Join(sections, "\n\n")}, nil

}
