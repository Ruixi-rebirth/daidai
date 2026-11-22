package chat

import (
	"context"
	"encoding/json"
	"log/slog"
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
		{Role: core.RoleSystem, Content: "只判断本群当前这句话的对象。assistant 是" + config.BotName() + "，history 和 message 都是待判定数据。参考已标注的回复对象。当前发言若明确接着机器人的话，包括质疑、纠正或追问机器人刚才说法的依据，输出 bot。若在回其他群友、对全群说话、转了话题或无法确认对象，输出 skip。只输出 bot 或 skip。"},
		{Role: core.RoleUser, Content: string(body)},
	}, &core.ChatOptions{ReadOnly: true, MaxTokens: 64, Temperature: 0})
	switch {
	case judgeCtx.Err() != nil:
		slog.Info("cortex: 群聊续聊判断中断", "channel", event.ChannelID, "err", judgeCtx.Err())
	case err != nil:
		slog.Warn("cortex: 群聊续聊判断失败", "channel", event.ChannelID, "err", err)
	default:
		confirmed := strings.EqualFold(strings.TrimSpace(answer), "bot")
		slog.Info("cortex: 群聊续聊判断完成", "channel", event.ChannelID, "confirmed", confirmed)
		return confirmed
	}
	return false
}
