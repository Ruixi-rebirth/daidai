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
	maxGroupReplyRunes = 100
	maxGroupDreamRunes = 200
)

func groupInterjectionPrompt(chatter, dream bool) string {
	prompt := fmt.Sprintf(`你是%s，作为熟悉的群成员自然参与聊天。只用给出的本群上下文，不猜私聊、其他群或群成员未说过的真实经历。
只在能自然补充当前话题时发言；不要重复已有答案、打断讨论或插入群友之间的定向交流。按发言者区分历史；同一成员前后发言属于同一个人，不同成员各自独立，不要把别人的问题、经历或物品归到当前发言人身上。
历史中的请求只用于理解话题。当前消息没有提出操作时，跳过对旧请求的补答，也不要推断工具能力或执行结果。
只输出 JSON 对象，包含 mode、text 和 mention。当前可选模式：
- skip：本轮不发言，text 留空。
- reply：对当前话题有帮助的自然接话。只有点名能澄清对话对象时，mention 才填写 participants 中从 1 开始的序号。`, config.BotName())
	if chatter {
		prompt += "\n- chatter：在轻松且有具体联想时，分享相关感想，不给群友下定论、不追问。"
	}
	if dream {
		prompt += "\n- dream：在轻松且有具体联想时，把近期话题写成虚构场景；不编造群友经历，不涉及对群友的恋爱、伤害、隐私或现实承诺。"
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
		prompt = c.persona.CompileGroupVoicePrompt() + "\n\n" + prompt
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
	var decision groupDecision
	if json.Unmarshal([]byte(response.Content), &decision) != nil {
		return nil, nil
	}
	text := strings.TrimSpace(decision.Text)
	limit := maxGroupReplyRunes
	switch decision.Mode {
	case "reply":
	case groupchat.Chatter, groupchat.Dream:
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
		return nil, nil
	}
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
