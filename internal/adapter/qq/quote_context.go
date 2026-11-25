package qq

import (
	"log/slog"
	"strings"

	"daidai/internal/config"
	"daidai/internal/entity"
)

func (q *QQChannel) resolveInboundQuote(d map[string]any, isGroup bool, convID, msgID string) (*entity.SensoryEvent, bool) {
	var replyCtx *entity.SensoryEvent
	if ref, ok := d["message_reference"].(map[string]any); ok {
		var refContent string
		if c, ok := ref["content"].(string); ok && c != "" {
			refContent = c
		} else if msgObj, ok := ref["message"].(map[string]any); ok {
			refContent, _ = msgObj["content"].(string)
		}

		refMsgID, _ := ref["message_id"].(string)
		refContent = entity.SanitizeQuotedMessageContent(refContent)

		slog.Info("qq: 处理引用消息", "msgID", msgID, "refMsgID", refMsgID, "hitCache", refContent != "")

		if refContent != "" {
			replyCtx = &entity.SensoryEvent{
				Content:      strings.TrimSpace(refContent),
				RawMessageID: refMsgID,
			}
		}
	}

	// 适配 QQ 官方 v2 协议原生引用消息 (message_type: 103)
	if replyCtx == nil {
		if quote := qqReferencedMessageElement(d); quote != nil {
			replyCtx = qqQuotedMessageContext(quote, 0)
			if replyCtx != nil {
				refIndex, _ := quote["msg_idx"].(string)
				slog.Info("qq: 从 v2 msg_elements 中解析出引用消息", "msgID", msgID, "refMsgIdx", refIndex, "refContentBytes", len(replyCtx.Content), "hasNestedQuote", replyCtx.ReplyContext != nil)
			}
		}
		if replyCtx == nil && qqQuoteElementCount(d) > 1 {
			replyCtx = &entity.SensoryEvent{Content: "[QQ 消息包含多个引用片段，但无法根据引用索引确定本轮目标]"}
			slog.Warn("qq: 引用消息候选不唯一且缺少匹配索引", "msgID", msgID, "quoteCount", qqQuoteElementCount(d))
		}
	}
	quotedBot := false
	if q.msgCache != nil {
		cacheKey := qqReferenceCacheKey(d)
		if cacheKey != "" {
			chatType := "private"
			if isGroup {
				chatType = "group"
			}
			if record, found := q.msgCache.GetRecord(cacheKey, chatType, convID); found {
				if replyCtx == nil {
					replyCtx = &entity.SensoryEvent{RawMessageID: cacheKey}
				}
				if record.Content != "" {
					replyCtx.Content = entity.SanitizeQuotedMessageContent(record.Content)
				} else if record.ContentKind == qqQuoteImage && len(replyCtx.Images) == 0 {
					replyCtx.Content = "[引用的是图片，但 QQ 没有提供可读取的图片附件]"
				} else if record.ContentKind == qqQuoteFile && len(replyCtx.Files) == 0 {
					replyCtx.Content = "[引用的是文件，但 QQ 没有提供可读取的文件附件]"
				} else if record.ContentKind == qqQuoteTextCard {
					replyCtx.Content = "[引用的是图片文字卡片，但没有可用的原文缓存]"
				} else if record.ContentKind == qqQuoteMixed && replyCtx.Content == "" {
					replyCtx.Content = "[引用的是图文消息，但没有可用的文字正文]"
				}
				replyCtx.ContentComplete = record.TextComplete
				replyCtx.ContentKind = record.ContentKind
				if record.Direction == "outbound" {
					quotedBot = true
					if replyCtx.SenderName == "" {
						replyCtx.SenderName = config.BotName()
					}
				}
				slog.Info("qq: 引用命中语义正文索引", "msgID", msgID, "hasRefIndex", strings.HasPrefix(cacheKey, "REFIDX_"), "contentKind", record.ContentKind, "textComplete", record.TextComplete, "refContentBytes", len(replyCtx.Content), "quotedBot", quotedBot)
			}
		}
	}
	if replyCtx != nil && replyCtx.Content == "" && len(replyCtx.Images)+len(replyCtx.Files) == 0 {
		replyCtx.Content = "[引用消息（QQ 未提供可读取的正文）]"
	}

	return replyCtx, quotedBot
}
