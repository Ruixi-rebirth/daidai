package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"log/slog"
	"mime/multipart"
	"strconv"
	"strings"

	"daidai/internal/core"
	"daidai/internal/entity"
)

func (c *Channel) Transmit(ctx context.Context, u *entity.Utterance) error {
	if u == nil {
		return nil
	}
	if strings.TrimSpace(u.ChannelID) == "" {
		return fmt.Errorf("telegram destination is empty")
	}
	reply, _ := strconv.ParseInt(u.QuoteMsgID, 10, 64)
	if u.GroupInterjection && len(u.MentionUserIDs) == 0 {
		reply = 0
	}
	images := u.Images
	sources := append([]string(nil), u.ImageSources...)
	if len(images) == 0 && len(u.CardData) > 0 {
		images = [][]byte{u.CardData}
		sources = []string{u.FallbackText}
	}
	for len(sources) < len(images) {
		sources = append(sources, "")
	}
	sources = sources[:len(images)]
	// Validate before delivering any part of the response.
	for _, data := range images {
		if len(data) == 0 || len(data) > core.MaxImageBytes {
			return fmt.Errorf("telegram image is empty or exceeds size limit")
		}
		config, _, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return fmt.Errorf("telegram image format is invalid")
		}
		if err = core.ValidateImageDimensions(config.Width, config.Height); err != nil {
			return err
		}
	}
	text := u.Text
	caption := ""
	if len(images) > 0 && len([]rune(text)) <= 500 {
		caption, text = text, ""
	}
	delivered := ""
	sent := false
	deliveredImages := 0
	partial := func(err error, remainingText string) error {
		completeSources, pendingSources := imageDeliveryText(sources, deliveredImages)
		return &entity.PartialDeliveryError{Err: err,
			DeliveredText:   joinDeliveryText(completeSources, delivered),
			UndeliveredText: joinDeliveryText(pendingSources, remainingText)}
	}
	for start := 0; start < len(images); start += 10 {
		end := min(start+10, len(images))
		batchCaption := ""
		if start == 0 {
			batchCaption = caption
		}
		results, err := c.sendImages(ctx, u.ChannelID, reply, images[start:end], batchCaption)
		if err != nil {
			var receiptErr *entity.PartialDeliveryError
			if sent || errors.As(err, &receiptErr) {
				remaining := text
				if !sent {
					remaining = joinDeliveryText(caption, text)
				}
				return partial(err, remaining)
			}
			return err
		}
		sent = true
		deliveredImages = end
		if batchCaption != "" {
			delivered = batchCaption
		}
		for i, result := range results {
			source := sources[start+i]
			r := &quoteRecord{Content: source, Kind: "text_card", Complete: source != "", SenderName: c.bot.FirstName}
			if source == "" {
				r.Content = "[机器人发送的图片]"
				if i == 0 && batchCaption != "" {
					r.Content = batchCaption
				}
				r.Kind = "media"
				r.Images, r.Files = attachments(&result, true)
			}
			if err := c.storeQuote(ctx, u.ChannelID, result.ID, r); err != nil {
				slog.Warn("telegram: image source persistence failed", "err", err)
			}
		}
	}
	formattedChunks := splitFormatted(formatMarkdown(text), 1800)
	chunks := make([]string, len(formattedChunks))
	for i, chunk := range formattedChunks {
		chunks[i] = chunk.Text
	}
	for i, chunk := range chunks {
		if strings.TrimSpace(chunk) == "" {
			continue
		}
		if err := c.sendFormattedText(ctx, u.ChannelID, formattedChunks[i], reply, chunk); err != nil {
			if sent {
				delivered = joinDeliveryText(delivered, strings.Join(chunks[:i], "\n"))
				return partial(err, strings.Join(chunks[i:], "\n"))
			}
			return err
		}
		sent = true
	}
	return nil
}

// A paginated card may repeat its full source on every page. Only count that
// source as delivered after every page carrying it has been accepted.
func imageDeliveryText(sources []string, delivered int) (complete, pending string) {
	return entity.ImageDeliveryText(sources, delivered)
}

func joinDeliveryText(parts ...string) string {
	var result []string
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			result = append(result, part)
		}
	}
	return strings.Join(result, "\n\n")
}

// Telegram albums carry 2–10 photos. A remaining single photo uses sendPhoto.
func (c *Channel) sendImages(ctx context.Context, chat string, reply int64, images [][]byte, caption string) (messages []message, sendErr error) {
	attempted := false
	defer func() {
		if sendErr != nil && !attempted {
			sendErr = &entity.UnsentError{Err: sendErr}
		}
	}()
	kind := "photo"
	for _, data := range images {
		config, _, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("telegram image format is invalid")
		}
		if config.Width+config.Height > 10000 || config.Width > 20*config.Height || config.Height > 20*config.Width {
			kind = "document"
		}
	}
	var buffer bytes.Buffer
	w := multipart.NewWriter(&buffer)
	dest, err := destination(chat)
	if err != nil {
		return nil, err
	}
	for name, value := range dest {
		if err := w.WriteField(name, fmt.Sprint(value)); err != nil {
			return nil, err
		}
	}
	if reply > 0 {
		p, _ := json.Marshal(map[string]any{"message_id": reply, "allow_sending_without_reply": true})
		if err := w.WriteField("reply_parameters", string(p)); err != nil {
			return nil, err
		}
	}
	var media []map[string]any
	formattedCaption := formatMarkdown(caption)
	for i, data := range images {
		name := fmt.Sprintf("photo%d", i)
		if len(images) == 1 {
			name = kind
		}
		part, err := w.CreateFormFile(name, "image.png")
		if err != nil {
			return nil, err
		}
		if _, err = part.Write(data); err != nil {
			return nil, err
		}
		item := map[string]any{"type": kind, "media": "attach://" + name}
		if i == 0 && caption != "" {
			item["caption"] = formattedCaption.Text
			if len(formattedCaption.Entities) > 0 {
				item["caption_entities"] = formattedCaption.Entities
			}
		}
		media = append(media, item)
	}
	method := "sendPhoto"
	if kind == "document" {
		method = "sendDocument"
	}
	if len(images) > 1 {
		method = "sendMediaGroup"
		encoded, _ := json.Marshal(media)
		if err := w.WriteField("media", string(encoded)); err != nil {
			return nil, err
		}
	} else if caption != "" {
		if err := w.WriteField("caption", formattedCaption.Text); err != nil {
			return nil, err
		}
		if len(formattedCaption.Entities) > 0 {
			encoded, _ := json.Marshal(formattedCaption.Entities)
			if err := w.WriteField("caption_entities", string(encoded)); err != nil {
				return nil, err
			}
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	if len(images) == 1 {
		var result message
		attempted = true
		if err := c.call(ctx, method, &buffer, w.FormDataContentType(), &result); err != nil {
			return nil, err
		}
		return []message{result}, nil
	}
	var results []message
	attempted = true
	if err := c.call(ctx, method, &buffer, w.FormDataContentType(), &results); err != nil {
		return nil, err
	}
	if len(results) != len(images) {
		return nil, &entity.PartialDeliveryError{Err: fmt.Errorf("telegram returned an incomplete album receipt")}
	}
	return results, nil
}

func destination(address string) (map[string]any, error) {
	chat, topic, hasTopic := strings.Cut(address, "/")
	id, err := strconv.ParseInt(chat, 10, 64)
	if err != nil || id == 0 {
		return nil, fmt.Errorf("invalid Telegram chat ID")
	}
	result := map[string]any{"chat_id": id}
	if hasTopic {
		thread, err := strconv.ParseInt(topic, 10, 64)
		if err != nil || thread <= 0 {
			return nil, fmt.Errorf("invalid Telegram topic ID")
		}
		result["message_thread_id"] = thread
	}
	return result, nil
}
