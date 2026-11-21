package tool

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"daidai/internal/core"
	"daidai/internal/engine/vision"
	"daidai/internal/entity"
	"daidai/internal/media"
)

func TestImageAnalysisReportsOmissionsFromSelectionAndDownload(t *testing.T) {
	var picture bytes.Buffer
	if err := png.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	sources := &media.Sources{}
	sources.Register("fixture", func(_ context.Context, url string, _ int64) ([]byte, error) {
		if url == "fixture:unavailable" {
			return nil, errors.New("image expired")
		}
		return picture.Bytes(), nil
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"看到一张图"}}]}`))
	}))
	defer server.Close()

	valid := &core.Attachment{URL: "fixture:valid"}
	event := &entity.SensoryEvent{
		Images:        []*core.Attachment{valid, valid, {}, {URL: "fixture:unavailable"}},
		OmittedImages: 1,
	}
	engine := vision.NewEngine(vision.Config{APIKey: "fixture", BaseURL: server.URL, Sources: sources})
	result, err := NewImageAnalysisTool(engine).Execute(entity.WithSensoryEvent(context.Background(), event), []byte(`{"source":"current_message"}`))
	if err != nil || result == nil || !strings.Contains(result.Text, "另有 3 张图片未分析") {
		t.Fatalf("expected 3 omitted images, result=%+v err=%v", result, err)
	}
	if len(event.ProcessedAttachments) != 1 || event.ProcessedAttachments[0] != valid {
		t.Fatalf("unexpected processed attachments: %+v", event.ProcessedAttachments)
	}
}
