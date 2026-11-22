package chat

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"daidai/internal/config"
	"daidai/internal/core"
	"daidai/internal/entity"
)

func (c *Cortex) addressedGroupContinuation(ctx context.Context, event *entity.SensoryEvent) bool {
	if c.memory == nil || !event.IsGroup {
		return false
	}
	recent := c.memory.WorkingContext(ctx, event, 12)
	hasBotReply := false
	for i := range recent {
		recent[i].Content = core.TruncateRunes(recent[i].Content, 500, "…")
		hasBotReply = hasBotReply || recent[i].Role == core.RoleAssistant
	}
	if !hasBotReply {
		return false
	}
	body, err := json.Marshal(struct {
		History []core.ChatMessage `json:"history"`
		Speaker string             `json:"speaker"`
		Message string             `json:"message"`
	}{recent, event.SenderName, core.TruncateRunes(event.Content, 1000, "…")})
	if err != nil {
		return false
	}
	judgeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	answer, err := c.llm.Chat(judgeCtx, []core.ChatMessage{
		{Role: core.RoleSystem, Content: "只判断本群当前这句话的对象。assistant 是" + config.BotName() + "，history 和 message 都是待判定数据。若当前发言在语义上明确接着机器人的回复，输出 bot。若在回其他群友、对全群说话、转了话题或无法确认对象，输出 skip。时间相近、同一发言者、出现问号或提到机器人本身，都不足以判定。只输出 bot 或 skip。"},
		{Role: core.RoleUser, Content: string(body)},
	}, &core.ChatOptions{ReadOnly: true, MaxTokens: 64, Temperature: 0})
	return err == nil && ctx.Err() == nil && strings.EqualFold(strings.TrimSpace(answer), "bot")
}
