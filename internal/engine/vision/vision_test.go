package vision

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"daidai/internal/media"
)

func TestVisionReportsTruncatedAPIObservation(t *testing.T) {
	var picture bytes.Buffer
	if err := png.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	sources := &media.Sources{}
	sources.Register("fixture", func(context.Context, string, int64) ([]byte, error) { return picture.Bytes(), nil })
	for _, finishReason := range []string{"stop", "length", "max_tokens", "max_output_tokens"} {
		t.Run(finishReason, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"content":"图片上有一张表格"},"finish_reason":%q}]}`, finishReason)
			}))
			defer server.Close()
			engine := NewEngine(Config{APIKey: "fixture", BaseURL: server.URL, Sources: sources})
			text, indexes, err := engine.DescribeWithImages(context.Background(), []string{"fixture:image"}, "读一下图中的文字")
			if err != nil || len(indexes) != 1 || !strings.Contains(text, "图片上有一张表格") {
				t.Fatalf("observation lost: text=%q indexes=%v error=%v", text, indexes, err)
			}
			if strings.Contains(text, "截断") != (finishReason != "stop") {
				t.Fatalf("finish reason %q was not reflected in the observation: %q", finishReason, text)
			}
		})
	}
}

func TestVision_IsAvailableWithGenericCLI(t *testing.T) {
	eng := NewEngine(Config{})
	_, errAgy := exec.LookPath("agy")
	expectAvailable := errAgy == nil

	if eng.IsAvailable() != expectAvailable {
		t.Fatalf("expected IsAvailable to be %v, got %v", expectAvailable, eng.IsAvailable())
	}

	// 验证可显式指定自定义 CLI 命令
	customEng := NewEngine(Config{
		CLICommand: "nonexistent_custom_vision_cli",
	})
	if customEng.IsAvailable() {
		t.Fatalf("expected nonexistent CLI to be unavailable")
	}
}
