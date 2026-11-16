package telegram

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"daidai/internal/core"
)

func attachments(m *message, quoted bool) (images, files []*core.Attachment) {
	add := func(f *file, image bool, name string) {
		if f == nil || f.ID == "" {
			return
		}
		if f.Name != "" {
			name = f.Name
		}
		kind := core.AttachmentFile
		if image {
			kind = core.AttachmentImage
		}
		a := &core.Attachment{Kind: kind, URL: "telegram-file:" + f.ID, Name: name, MimeType: f.MIME, SourceMessageID: strconv.FormatInt(m.ID, 10), Quoted: quoted}
		if image {
			images = append(images, a)
		} else {
			files = append(files, a)
		}
	}
	if len(m.Photo) > 0 {
		add(&m.Photo[len(m.Photo)-1], true, "photo.jpg")
	}
	add(m.Document, m.Document != nil && strings.HasPrefix(m.Document.MIME, "image/"), "document")
	add(m.Audio, false, "audio")
	add(m.Voice, false, "voice.ogg")
	add(m.Video, false, "video.mp4")
	return
}

// FetchFile resolves a Telegram file only when the shared attachment workflow
// requests its bytes. Token-bearing URLs never leave this adapter.
func (c *Channel) FetchFile(ctx context.Context, ref string, limit int64) ([]byte, error) {
	id, ok := strings.CutPrefix(ref, "telegram-file:")
	if !ok || id == "" || limit <= 0 {
		return nil, fmt.Errorf("invalid Telegram file reference")
	}
	var result struct {
		Path string `json:"file_path"`
		Size int64  `json:"file_size"`
	}
	if err := c.jsonCall(ctx, "getFile", map[string]any{"file_id": id}, &result); err != nil {
		return nil, err
	}
	if result.Size > limit {
		return nil, fmt.Errorf("telegram file exceeds %d byte limit", limit)
	}
	if result.Path == "" || path.Clean(result.Path) != result.Path || strings.HasPrefix(result.Path, "/") || strings.HasPrefix(result.Path, "../") || strings.ContainsAny(result.Path, "\\?#") {
		return nil, fmt.Errorf("invalid Telegram file path")
	}
	segments := strings.Split(result.Path, "/")
	for i := range segments {
		segments[i] = url.PathEscape(segments[i])
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.telegram.org/file/bot"+c.cfg.Token+"/"+strings.Join(segments, "/"), nil)
	if err != nil {
		return nil, fmt.Errorf("telegram file request construction failed")
	}
	response, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("telegram file download failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("telegram file download returned status %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("telegram file download interrupted")
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("telegram file exceeds %d byte limit", limit)
	}
	return data, nil
}
