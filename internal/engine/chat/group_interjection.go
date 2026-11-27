package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"daidai/internal/config"
	"daidai/internal/core"
	"daidai/internal/entity"
	"daidai/internal/groupchat"
)

const (
	maxGroupReplyRunes            = 100
	maxGroupDreamRunes            = 200
	maxAnonymousInterjectionDrift = 2
)

func parseGroupDecision(raw string) (groupDecision, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return groupDecision{}, false
	}
	var decision groupDecision
	if json.Unmarshal([]byte(trimmed), &decision) == nil {
		return decision, true
	}
	// Some models wrap JSON in ```json ... ``` fences despite instructions.
	if start, end := strings.IndexByte(trimmed, '{'), strings.LastIndexByte(trimmed, '}'); start >= 0 && end > start {
		if json.Unmarshal([]byte(trimmed[start:end+1]), &decision) == nil {
			return decision, true
		}
	}
	return groupDecision{}, false
}

func groupInterjectionPrompt(chatter, dream bool) string {
	prompt := fmt.Sprintf(`你是群成员%s，此刻没被点名，正在决定要不要随口搭一句。只使用给出的本群上下文，不猜私聊、其他群或群成员未说过的现实经历。
先看懂【话题性质与时机】：
1. 必须选 skip 的情况：吵架、拌嘴、起哄、私人事务（约饭/转账/私下安排）；某人明确在等特定对象回答的定向提问（不抢答）；纯寒暄（早/晚安/在吗）、纯附和（哈哈/嗷嗷嗷/666/确实/嗯/收到）或刚收尾的话；
2. 可以选 reply 的情况：群里的公开讨论（不限人数，两三个人在群里聊技术、架构、工具、产品、见闻或日常吐槽也算），且你刚好有一句具体观点、真实共鸣、轻吐槽或实用补充，顺嘴接一句短口语（不说教、不端着、不做总结）；
3. 宁缺毋滥：若只能说“确实不错”“很有想法”这类泛泛附和或复述，宁可沉默选 skip。
按发言者区分历史；不同成员各自独立，不要把别人的问题、经历或物品归到当前发言人身上。历史中的请求只用于理解话题；当前消息未提出操作时，不补答旧请求，也不推断工具能力或执行结果。严禁写括号动作旁白或 Emoji 堆砌。
只输出 JSON 对象，包含 mode、text 和 mention。当前可选模式：
- skip：本轮保持安静，text 留空。
- reply：顺着当前话题自然搭一句或补充关键点，单行简短口语、不要回车换行、句末不加句号‘。’、说完即止。只有点名能澄清对象时，mention 才填 participants 中从 1 开始的序号，否则填 0。`, config.BotName())
	if chatter {
		prompt += "\n- chatter：由眼前话题勾起的一两句轻声碎碎念或小感想（单行口语自然、不要换行、句末不加句号‘。’、不追问、不给群友下定论，mention 必须为 0）。"
	}
	if dream {
		prompt += "\n- dream：把本群近期聊到的意象编织成一段荒诞又带点温柔画面感的第一人称短梦境（不带引号或固定开头，不编造群友现实经历，不涉及恋爱、伤害、隐私或现实承诺，mention 必须为 0）。"
	}
	return prompt
}

type groupDecision struct {
	Mode    string `json:"mode"`
	Text    string `json:"text"`
	Mention int    `json:"mention"`
}

type groupParticipantView struct {
	Name string `json:"name"`
}

func groupMentionCandidates(event *entity.SensoryEvent) []core.GroupParticipant {
	if event == nil {
		return nil
	}
	participants := make([]core.GroupParticipant, 0, min(20, len(event.GroupParticipants)+1))
	seen := make(map[string]struct{}, cap(participants))
	add := func(id, name string) {
		id = strings.TrimSpace(id)
		if id == "" || len(participants) == 20 {
			return
		}
		if _, exists := seen[id]; exists {
			return
		}
		seen[id] = struct{}{}
		name = core.TruncateRunes(strings.TrimSpace(name), 80, "…")
		if name == "" {
			name = "群成员"
		}
		participants = append(participants, core.GroupParticipant{ID: id, Name: name})
	}
	add(event.SenderID, event.SenderName)
	for _, participant := range event.GroupParticipants {
		add(participant.ID, participant.Name)
	}
	return participants
}

func (c *Cortex) thinkGroupInterjection(ctx context.Context, event *entity.SensoryEvent) (*entity.Utterance, error) {
	if !event.IsGroup || event.ChannelID == "" || event.RawMessageID == "" || (event.Platform != "qq" && event.Platform != "telegram") || strings.TrimSpace(event.Content) == "" {
		return nil, nil
	}
	if event.StillValid != nil && !event.StillValid() {
		return nil, nil
	}
	event.GroupExpression = ""
	var state groupchat.State
	var err error
	if c.groupExpressions != nil {
		state, err = c.groupExpressions.Get(ctx, event.Platform, event.ChannelID)
		if err != nil {
			return nil, fmt.Errorf("read group expression settings: %w", err)
		}
	}
	chatter, dream := state.Available(time.Now())
	var recent []core.ChatMessage
	if c.memory != nil {
		recent = c.memory.WorkingContext(ctx, event, 20)
	}
	// Keep the recent end of the conversation without pulling episodic memory,
	// user profiles, workspaces or machine runtime state into this read-only path.
	budget, first, humanMessages := 6000, len(recent), 1
	for n := len(recent) - 1; n >= 0; n-- {
		recent[n].Content = core.TruncateRunes(recent[n].Content, 1200, "…")
		cost := utf8.RuneCountInString(recent[n].Content)
		if cost > budget {
			break
		}
		budget -= cost
		first = n
		if recent[n].Role == core.RoleUser {
			humanMessages++
		}
	}
	if humanMessages < 3 {
		chatter, dream = false, false
	}
	participants := groupMentionCandidates(event)
	participantViews := make([]groupParticipantView, len(participants))
	for i, participant := range participants {
		participantViews[i] = groupParticipantView{Name: participant.Name}
	}
	input := struct {
		History      []core.ChatMessage     `json:"history"`
		Speaker      string                 `json:"speaker"`
		Current      string                 `json:"current"`
		Participants []groupParticipantView `json:"participants"`
	}{recent[first:], core.TruncateRunes(event.SenderName, 80, "…"), core.TruncateRunes(event.Content, 2000, "…"), participantViews}
	body, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	prompt := sourceTrustPolicy + "\n" + groupInterjectionPrompt(chatter, dream)
	if c.persona != nil {
		anchor := event.OccurredAt
		if anchor.IsZero() {
			anchor = time.Now()
		}
		prompt = c.persona.CompileGroupVoicePrompt() + "\n" + entity.CircadianRhythm(anchor) + "\n\n" + prompt
	}
	response, err := c.llm.ChatWithTools(ctx, []core.ChatMessage{{Role: core.RoleSystem, Content: prompt}, {Role: core.RoleUser, Content: string(body)}}, nil, &core.ChatOptions{ReadOnly: true, Temperature: 0.7, MaxTokens: 700})
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if response == nil || len(response.ToolCalls) > 0 || (event.StillValid != nil && !event.StillValid()) {
		return nil, nil
	}
	drift := 0
	if event.InterjectionDrift != nil {
		drift = event.InterjectionDrift()
	}
	decision, ok := parseGroupDecision(response.Content)
	if !ok {
		slog.Info("group_interjection: 决策解析失败已跳过", "channel", event.ChannelID, "drift", drift)
		return nil, nil
	}
	if decision.Mode == "skip" || decision.Mode == "" {
		slog.Info("group_interjection: 模型选择保持安静", "channel", event.ChannelID, "mode", decision.Mode, "drift", drift)
		return nil, nil
	}
	text := strings.TrimSpace(decision.Text)
	if c.persona != nil && text != "" {
		cleaned, _, dropped := sanitizePersonaText(text, event.Content, false, true)
		if dropped {
			return nil, nil
		}
		text = strings.TrimSpace(cleaned)
	}
	limit := maxGroupReplyRunes
	switch decision.Mode {
	case "reply":
	case groupchat.Chatter, groupchat.Dream:
		// Ambient self-talk should stay quiet if the chat moved on quickly while thinking.
		if event.InterjectionDrift != nil && event.InterjectionDrift() > maxAnonymousInterjectionDrift {
			slog.Info("group_interjection: 匿名表达因消息漂移过多取消", "channel", event.ChannelID, "mode", decision.Mode, "drift", drift)
			return nil, nil
		}
		// Recheck after the model call: disabling a mode or a successful send in
		// another in-flight turn must make this old decision ineligible.
		if c.groupExpressions == nil {
			return nil, nil
		}
		current, err := c.groupExpressions.Get(ctx, event.Platform, event.ChannelID)
		if err != nil {
			return nil, err
		}
		allowChatter, allowDream := current.Available(time.Now())
		if (decision.Mode == groupchat.Chatter && (!chatter || !allowChatter)) || (decision.Mode == groupchat.Dream && (!dream || !allowDream)) {
			return nil, nil
		}
		if decision.Mention != 0 {
			return nil, nil
		}
		if decision.Mode == groupchat.Dream {
			limit = maxGroupDreamRunes
		}
	default:
		return nil, nil
	}
	if text == "" || utf8.RuneCountInString(text) > limit || strings.Contains(text, "[[GROUP_") {
		slog.Info("group_interjection: 回复未通过长度或格式校验", "channel", event.ChannelID, "mode", decision.Mode, "drift", drift, "runes", utf8.RuneCountInString(text))
		return nil, nil
	}
	slog.Info("group_interjection: 决定插话", "channel", event.ChannelID, "mode", decision.Mode, "drift", drift, "mention", decision.Mention)
	u := &entity.Utterance{Platform: event.Platform, IsGroup: true, ChannelID: event.ChannelID, QuoteMsgID: event.RawMessageID, GroupInterjection: true, Text: text}
	if decision.Mode == "reply" {
		if decision.Mention > 0 && decision.Mention <= len(participants) {
			if id := strings.TrimSpace(participants[decision.Mention-1].ID); id != "" {
				u.MentionUserIDs = []string{id}
			}
		}
		return u, nil
	}
	event.GroupExpression = decision.Mode
	u.AnonymousInterjection = true
	if decision.Mode == groupchat.Dream {
		u.Text = "梦到个离谱画面：\n" + text
	}
	u.OnDelivered = func(delivered context.Context) {
		if err := c.groupExpressions.MarkSent(delivered, event.Platform, event.ChannelID, decision.Mode, time.Now()); err != nil {
			slog.Warn("group expressions: 无法保存发送冷却状态", "platform", event.Platform)
		}
	}
	return u, nil
}
