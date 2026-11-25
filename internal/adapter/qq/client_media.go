package qq

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

func (c *Client) UploadMedia(ctx context.Context, targetType, targetID string, fileType int, data []byte) (string, error) {
	if len(data) == 0 {
		return "", fmt.Errorf("upload media failed: media data is empty")
	}
	limit := maxQQMediaBytes
	if fileType == 2 {
		limit = maxQQVideoBytes
	}
	if len(data) > limit {
		return "", fmt.Errorf("upload media failed: media exceeds %d byte limit", limit)
	}
	url := fmt.Sprintf("%s/v2/%s/%s/files", apiBaseURL, targetType, targetID)
	payload := map[string]any{
		"file_type":    fileType,
		"file_data":    base64.StdEncoding.EncodeToString(data),
		"srv_send_msg": false,
	}

	respBytes, err := c.doPostJSON(ctx, url, payload)
	if err != nil {
		return "", fmt.Errorf("upload media failed: %w", err)
	}

	var res struct {
		FileInfo string `json:"file_info"`
		FileUUID string `json:"file_uuid"`
	}
	if err := json.Unmarshal(respBytes, &res); err != nil {
		return "", fmt.Errorf("failed to parse media upload response: %w", err)
	}
	if res.FileInfo == "" {
		return "", fmt.Errorf("upload media returned empty file_info: %s", summarizeAPIResponse(respBytes))
	}

	return res.FileInfo, nil
}

// UploadMarkdownImage uploads PNG data to QQ's material library and returns
// the platform-scoped Markdown reference and material ID for later cleanup.
func (c *Client) UploadMarkdownImage(ctx context.Context, data []byte) (string, string, error) {
	const maxMarkdownImageBytes = 10_000_000
	if len(data) == 0 || len(data) > maxMarkdownImageBytes {
		return "", "", fmt.Errorf("markdown image size must be between 1 and %d bytes", maxMarkdownImageBytes)
	}
	if !bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) {
		return "", "", fmt.Errorf("markdown image must be PNG")
	}
	md5Sum := md5.Sum(data)
	sha1Sum := sha1.Sum(data)
	sha256Sum := sha256.Sum256(data)
	fileName := "reply-" + hex.EncodeToString(sha256Sum[:8]) + ".png"
	prepared, err := c.doPostJSON(ctx, apiBaseURL+"/v2/media-images/prepare", map[string]any{
		"file_name": fileName, "file_size": len(data), "content_type": "image/png",
		"md5": hex.EncodeToString(md5Sum[:]), "sha1": hex.EncodeToString(sha1Sum[:]),
	})
	if err != nil {
		return "", "", fmt.Errorf("prepare QQ Markdown image upload: %w", err)
	}
	var upload struct {
		UploadID     string `json:"upload_id"`
		PresignedURL string `json:"presigned_url"`
	}
	if err := json.Unmarshal(prepared, &upload); err != nil {
		return "", "", fmt.Errorf("parse QQ Markdown image upload: %w", err)
	}
	if upload.UploadID == "" || upload.PresignedURL == "" {
		return "", "", fmt.Errorf("QQ Markdown image upload response is missing upload_id or presigned_url")
	}
	putReq, err := http.NewRequestWithContext(ctx, http.MethodPut, upload.PresignedURL, bytes.NewReader(data))
	if err != nil {
		return "", "", err
	}
	putReq.Header.Set("Content-Type", "image/png")
	putResp, err := c.httpClient.Do(putReq)
	if err != nil {
		return "", "", fmt.Errorf("upload QQ Markdown image bytes: %w", err)
	}
	_, readErr := readBoundedAPIResponse(putResp.Body)
	_ = putResp.Body.Close()
	if readErr != nil {
		return "", "", readErr
	}
	if putResp.StatusCode < 200 || putResp.StatusCode >= 300 {
		return "", "", fmt.Errorf("QQ Markdown image storage returned status %d", putResp.StatusCode)
	}
	etag := strings.Trim(putResp.Header.Get("ETag"), "\" ")
	if etag == "" {
		return "", "", fmt.Errorf("QQ Markdown image storage response did not include ETag")
	}
	completed, err := c.doPostJSON(ctx, apiBaseURL+"/v2/media-images/complete", map[string]string{
		"upload_id": upload.UploadID, "md5": etag,
	})
	if err != nil {
		return "", "", fmt.Errorf("complete QQ Markdown image upload: %w", err)
	}
	var material struct {
		ImageID string `json:"image_id"`
		URL     string `json:"url"`
	}
	if err := json.Unmarshal(completed, &material); err != nil {
		return "", "", fmt.Errorf("parse completed QQ Markdown image: %w", err)
	}
	if material.ImageID == "" || !strings.HasPrefix(material.URL, "qqbot-media://image/v1/") {
		return "", "", fmt.Errorf("QQ returned an invalid Markdown image material")
	}
	return material.ImageID, material.URL, nil
}

// DeleteMarkdownImage releases a temporary material after its message is sent.
func (c *Client) DeleteMarkdownImage(ctx context.Context, imageID string) error {
	if imageID == "" {
		return nil
	}
	_, err := c.doJSON(ctx, http.MethodDelete, apiBaseURL+"/v2/media-images/"+url.PathEscape(imageID)+"/delete", nil)
	return err
}

// DeleteReplyMarkdownImages reclaims only reply images created by this adapter.
// QQ keeps Markdown image materials in a bot-wide 100-image library; sent
// messages retain their copied image after the temporary material is deleted.
func (c *Client) DeleteReplyMarkdownImages(ctx context.Context, preserveIDs []string) error {
	cursor := ""
	var failures []error
	var candidates []string
	preserve := make(map[string]struct{}, len(preserveIDs))
	for _, id := range preserveIDs {
		preserve[id] = struct{}{}
	}
	for {
		listURL := apiBaseURL + "/v2/media-images?cursor=" + url.QueryEscape(cursor)
		data, err := c.doJSON(ctx, http.MethodGet, listURL, nil)
		if err != nil {
			return errors.Join(append(failures, fmt.Errorf("list QQ Markdown images: %w", err))...)
		}
		var page struct {
			Images []struct {
				ID       string `json:"image_id"`
				FileName string `json:"file_name"`
			} `json:"images"`
			NextCursor string `json:"next_cursor"`
			Data       *struct {
				Images []struct {
					ID       string `json:"image_id"`
					FileName string `json:"file_name"`
				} `json:"images"`
				NextCursor string `json:"next_cursor"`
			} `json:"data"`
		}
		if err := json.Unmarshal(data, &page); err != nil {
			return errors.Join(append(failures, fmt.Errorf("decode QQ Markdown image list: %w", err))...)
		}
		images, nextCursor := page.Images, page.NextCursor
		if page.Data != nil {
			images, nextCursor = page.Data.Images, page.Data.NextCursor
		}
		for _, image := range images {
			if !isReplyMarkdownImageName(image.FileName) || image.ID == "" {
				continue
			}
			if _, keep := preserve[image.ID]; keep {
				continue
			}
			candidates = append(candidates, image.ID)
		}
		if nextCursor == "" || nextCursor == cursor {
			break
		}
		cursor = nextCursor
	}
	for _, id := range candidates {
		if err := c.DeleteMarkdownImage(ctx, id); err != nil {
			failures = append(failures, fmt.Errorf("delete Daidai reply image: %w", err))
		}
	}
	return errors.Join(failures...)
}

func isReplyMarkdownImageName(name string) bool {
	if len(name) != len("reply-")+16+len(".png") || !strings.HasPrefix(name, "reply-") || !strings.HasSuffix(name, ".png") {
		return false
	}
	_, err := strconv.ParseUint(name[len("reply-"):len(name)-len(".png")], 16, 64)
	return err == nil
}

// SendGroupMessage 发送 QQ 群聊 Markdown 消息。
