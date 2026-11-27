package qq

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"daidai/internal/entity"
)

func testPNGBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestQQInlineImages_RetainsDeliveredMaterialsAndCleansFailedOnes(t *testing.T) {
	pngData := testPNGBytes(t)

	runCase := func(t *testing.T, failSend bool) []string {
		t.Helper()
		var deleted []string
		client := NewClient("app", "secret")
		client.accessToken = "token"
		client.httpClient.Transport = qqTestTransport(func(req *http.Request) (*http.Response, error) {
			header := make(http.Header)
			status := http.StatusOK
			body := `{}`
			switch {
			case strings.HasSuffix(req.URL.Path, "/v2/media-images/prepare"):
				body = `{"upload_id":"up-1","presigned_url":"https://upload.example.test/img"}`
			case req.Method == http.MethodPut:
				header.Set("ETag", `"etag-1"`)
			case strings.HasSuffix(req.URL.Path, "/v2/media-images/complete"):
				body = `{"image_id":"mat-1","url":"qqbot-media://image/v1/mat-1"}`
			case req.Method == http.MethodDelete && strings.Contains(req.URL.Path, "/v2/media-images/"):
				deleted = append(deleted, req.URL.Path)
			case strings.Contains(req.URL.Path, "/messages"):
				if failSend {
					status = http.StatusInternalServerError
					body = `{"code":500,"message":"send failed"}`
				} else {
					body = `{"id":"sent-1"}`
				}
			}
			return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body))}, nil
		})

		ch := &QQChannel{client: client}
		err := ch.Transmit(context.Background(), &entity.Utterance{
			Platform:       "qq",
			IsGroup:        true,
			ChannelID:      "group-1",
			QuoteMsgID:     "msg-1",
			MentionUserIDs: []string{"user-1"},
			Images:         [][]byte{pngData},
		})
		if failSend && err == nil {
			t.Fatal("expected send failure")
		}
		if !failSend && err != nil {
			t.Fatalf("unexpected send failure: %v", err)
		}
		return deleted
	}

	if deleted := runCase(t, false); len(deleted) != 0 {
		t.Fatalf("delivered Markdown images must NOT be deleted immediately, deleted=%v", deleted)
	}
	if deleted := runCase(t, true); len(deleted) != 1 || !strings.Contains(deleted[0], "/mat-1/delete") {
		t.Fatalf("failed send must clean up uploaded Markdown image, deleted=%v", deleted)
	}
}

func TestDeleteReplyMarkdownImages_ReclaimsOldestAndKeepsRecent50(t *testing.T) {
	type item struct {
		ID       string `json:"image_id"`
		FileName string `json:"file_name"`
	}
	var all []item
	// Create 60 reply images (in reverse order to ensure sorting by file_name works) + 1 non-reply image.
	for i := 59; i >= 0; i-- {
		all = append(all, item{
			ID:       fmt.Sprintf("id-%02d", i),
			FileName: fmt.Sprintf("reply-%08x-abcd1234.png", 1000+i),
		})
	}
	all = append(all, item{ID: "custom-1", FileName: "banner.png"})

	var deleted []string
	client := NewClient("app", "secret")
	client.accessToken = "token"
	client.httpClient.Transport = qqTestTransport(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/v2/media-images") {
			raw, _ := json.Marshal(map[string]any{"images": all})
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(raw))}, nil
		}
		if req.Method == http.MethodDelete {
			parts := strings.Split(strings.Trim(req.URL.Path, "/"), "/")
			if len(parts) >= 2 {
				deleted = append(deleted, parts[len(parts)-2])
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		}
		return &http.Response{StatusCode: http.StatusNotFound, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})

	if err := client.DeleteReplyMarkdownImages(context.Background(), nil); err != nil {
		t.Fatalf("DeleteReplyMarkdownImages failed: %v", err)
	}
	if len(deleted) != 10 {
		t.Fatalf("expected 10 oldest reply images deleted (keeping 50), got %d: %v", len(deleted), deleted)
	}
	want := []string{"id-00", "id-01", "id-02", "id-03", "id-04", "id-05", "id-06", "id-07", "id-08", "id-09"}
	if !slices.Equal(deleted, want) {
		t.Fatalf("deleted=%v, want %v", deleted, want)
	}
}
