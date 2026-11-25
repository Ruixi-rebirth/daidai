package qq

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"daidai/internal/config"
	"daidai/internal/core"
	"daidai/internal/entity"
	"daidai/internal/media"
	"daidai/internal/runtime"
)

func (q *QQChannel) dispatchEvent(ctx context.Context, payload map[string]any) bool {
	if ctx.Err() != nil {
		return false
	}
	message, ok := parseQQInboundMessage(payload)
	if !ok {
		return true
	}
	d := message.Data
	eventType := message.EventType
	convID := message.ConversationID
	userID := message.UserID
	senderName := message.SenderName
	content := message.Content
	isGroup := message.IsGroup
	var mentionedUsers []core.GroupParticipant
	if isGroup {
		for _, mention := range qqMentionedGroupMembers(d) {
			mentionedUsers = append(mentionedUsers, core.GroupParticipant{ID: mention.ID, Name: mention.Name})
		}
	}
	msgID := message.MessageID
	botMentioned := message.BotMentioned
	var (
		images        []*core.Attachment
		omittedImages int
		files         []*core.Attachment
		omittedFiles  int
	)
	if qqMessageAuthorIsBot(d) || (isGroup && q.isKnownGroupBot(convID, userID)) {
		if isGroup {
			q.cancelPendingGroupInterjection(convID)
		}
		slog.Debug("qq: 忽略机器人消息，避免机器人之间互相触发", "eventType", eventType, "channelID", convID, "msgID", msgID)
		return true
	}
	if msgID != "" && q.dedup != nil && q.dedup.Contains(msgID) {
		slog.Debug("qq: 拦截到重复投递的事件消息", "msgID", msgID)
		return true
	}
	if msgID == "" {
		slog.Warn("qq: 忽略缺少原消息 ID 的事件，无法使用被动回复凭证", "eventType", eventType, "channelID", convID)
		return true
	}

	inboundMedia := q.extractInboundMedia(ctx, d, msgID, content)
	content = inboundMedia.Content
	images, omittedImages = inboundMedia.Images, inboundMedia.OmittedImages
	files, omittedFiles = inboundMedia.Files, inboundMedia.OmittedFiles
	currentImageCount, currentOmittedImages := len(images), omittedImages
	currentFileCount, currentOmittedFiles := len(files), omittedFiles

	if ctx.Err() != nil {
		return false
	}

	replyCtx, quotedBot := q.resolveInboundQuote(d, isGroup, convID, msgID)

	if convID != "" && userID == "" {
		slog.Warn("qq: 忽略缺少发送者 ID 的消息事件", "eventType", eventType, "msgID", msgID, "channelID", convID)
		return true
	}

	var pendingFiles []*core.Attachment
	var pendingImages []*core.Attachment
	var omittedPendingFiles int
	var omittedPendingImages int
	var attachmentScope string
	if q.inbox != nil && convID != "" && userID != "" {
		attachmentScope = media.AttachmentScope("qq", isGroup, convID, userID)
		pendingFiles, omittedPendingFiles = q.inbox.PendingKind(attachmentScope, core.AttachmentFile)
		pendingImages, omittedPendingImages = q.inbox.PendingKind(attachmentScope, core.AttachmentImage)
	}
	if convID != "" && userID != "" && (content != "" || len(images) > 0 || len(files) > 0 || len(pendingFiles) > 0 || len(pendingImages) > 0 || omittedPendingFiles > 0 || omittedPendingImages > 0) {
		role := entity.RoleGuest
		if q.immune != nil {
			role = q.immune.ResolveRole(userID)
		}
		if !isGroup && role == entity.RoleGuest {
			q.handleUnprivilegedPrivateMessage(userID, senderName, msgID, content, currentImageCount, currentOmittedImages, currentFileCount, currentOmittedFiles)
			return true
		}
		directedElsewhere := isGroup && (replyCtx != nil || qqDirectedAtOtherMember(d, content))
		namedBot := config.BotName() != "" && strings.Contains(content, config.BotName())
		botAddressed := botMentioned || quotedBot || (isGroup && !directedElsewhere && namedBot && !strings.HasPrefix(strings.TrimSpace(content), "/"))
		groupContinuation := false
		if isGroup && !botAddressed {
			groupContinuation = q.evaluateGroupContinuation(convID, userID, content, d, replyCtx, quotedBot)
		}
		if isGroup && runtime.IsAdapterSlashCommand("qq", content) {
			q.handleGroupBotRegistryCommand(convID, msgID, userID, role, content, d)
			return true
		}
		trimmedContent := strings.TrimSpace(content)
		hasIncomingImages := len(images)+omittedImages > 0
		hasIncomingFiles := len(files)+omittedFiles > 0
		if (trimmedContent != "" || hasIncomingImages || hasIncomingFiles) && !strings.HasPrefix(trimmedContent, "/") && trimmedContent != "继续" && q.msgCache != nil && !entity.IsSensitiveAuthCommand(content) {
			chatType := "private"
			if isGroup {
				chatType = "group"
			}
			contentKind := qqQuoteText
			if hasIncomingImages && hasIncomingFiles || (hasIncomingImages || hasIncomingFiles) && trimmedContent != "" {
				contentKind = qqQuoteMixed
			} else if hasIncomingImages {
				contentKind = qqQuoteImage
			} else if hasIncomingFiles {
				contentKind = qqQuoteFile
			}
			q.msgCache.SetInboundTypedAsync(msgID, content, chatType, convID, contentKind, len([]rune(content)) <= core.MaxUserMessageRunes)
		}
		if q.inbox != nil {
			if !isGroup || botAddressed {
				if len(files) > 0 || len(images) > 0 {
					q.inbox.RememberPending(attachmentScope, append(append([]*core.Attachment(nil), files...), images...))
				}
			} else if len(files) > 0 {
				q.inbox.RememberPending(attachmentScope, files)
			}
		}
		if isGroup && !botAddressed && !groupContinuation {
			historyAttachmentNote := attachmentHistoryNote(currentImageCount, currentOmittedImages, currentFileCount, currentOmittedFiles)
			// Ordinary group messages are saved to formal group history. Attachments
			// stay pending and slash commands never execute without an @ mention.
			canInterject := !directedElsewhere &&
				len(images) == 0 && omittedImages == 0 && len(files) == 0 && omittedFiles == 0 && !strings.HasPrefix(content, "/") && content != "继续"
			observedEvent := &entity.SensoryEvent{
				Platform:              "qq",
				ChannelID:             convID,
				IsGroup:               true,
				SenderID:              userID,
				SenderName:            strings.TrimSpace(senderName),
				Role:                  role,
				Content:               strings.TrimSpace(content),
				HistoryAttachmentNote: historyAttachmentNote,
				ReplyContext:          replyCtx,
				RawMessageID:          msgID,
				OccurredAt:            time.Now(),
				GroupMessageObserved:  true,
				MentionedUsers:        mentionedUsers,
			}
			return q.ingestUnaddressedGroupMessage(observedEvent, canInterject)
		}
		if isGroup && botAddressed {
			q.cancelPendingGroupInterjection(convID)
			if botMentioned || quotedBot || (config.BotName() != "" && strings.Contains(content, config.BotName())) {
				q.recordGroupActiveSession(convID, userID)
			}
		}
		if groupContinuation {
			q.cancelPendingGroupInterjection(convID)
		}
		if replyCtx != nil {
			quotedFiles, omittedQuotedFiles := qqQuotedFiles(replyCtx)
			available := core.MaxIncomingFilesPerRequest - len(files)
			if available < 0 {
				available = 0
			}
			if len(quotedFiles) > available {
				omittedFiles += len(quotedFiles) - available
				quotedFiles = quotedFiles[:available]
			}
			files = append(files, quotedFiles...)
			omittedFiles += omittedQuotedFiles
			for current := replyCtx; current != nil; current = current.ReplyContext {
				omittedFiles += current.OmittedFiles
			}
		}
		accepted := q.IngestRaw(&entity.SensoryEvent{
			Platform:              "qq",
			ChannelID:             convID,
			IsGroup:               isGroup,
			SenderID:              userID,
			SenderName:            strings.TrimSpace(senderName),
			Role:                  role,
			Content:               strings.TrimSpace(content),
			HistoryAttachmentNote: attachmentHistoryNote(currentImageCount, currentOmittedImages, currentFileCount, currentOmittedFiles),
			Images:                images,
			OmittedImages:         omittedImages,
			PendingImages:         pendingImages,
			OmittedPendingImages:  omittedPendingImages,
			Files:                 files,
			PendingFiles:          pendingFiles,
			OmittedFiles:          omittedFiles,
			OmittedPendingFiles:   omittedPendingFiles,
			ReplyContext:          replyCtx,
			RawMessageID:          msgID,
			OccurredAt:            time.Now(),
			GroupMessageObserved:  groupContinuation,
			GroupContinuation:     groupContinuation,
			MentionedUsers:        mentionedUsers,
		})
		if !accepted {
			return false
		}
		if q.dedup != nil {
			q.dedup.Remember(msgID)
		}
	}
	return true
}
