package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"strings"
	"testing"

	"daidai/internal/entity"
)

func TestPartialAlbumPreservesUndeliveredCardSource(t *testing.T) {
	c := testChannel(t)
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	var images [][]byte
	var sources []string
	for range 11 {
		images = append(images, data.Bytes())
		sources = append(sources, "完整分页正文")
	}
	calls := 0
	c.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 2 {
			return response(`{"ok":false,"error_code":400}`), nil
		}
		var receipt []map[string]any
		for i := range 10 {
			receipt = append(receipt, map[string]any{"message_id": i + 1})
		}
		encoded, _ := json.Marshal(receipt)
		return response(fmt.Sprintf(`{"ok":true,"result":%s}`, encoded)), nil
	})
	err := c.Transmit(context.Background(), &entity.Utterance{ChannelID: "42", Images: images, ImageSources: sources})
	var partial *entity.PartialDeliveryError
	if !errors.As(err, &partial) {
		t.Fatalf("missing partial delivery result: %v", err)
	}
	if partial.DeliveredText != "" {
		t.Fatal("incomplete paginated card advertised as fully delivered")
	}
	if partial.UndeliveredText != "完整分页正文" {
		t.Fatalf("card continuation lost or duplicated: %q", partial.UndeliveredText)
	}
	if calls != 2 {
		t.Fatal("unexpected fallback or retry")
	}
}

func TestPartialTextKeepsAlreadyDeliveredCardInHistory(t *testing.T) {
	c := testChannel(t)
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	c.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/sendPhoto") {
			return response(`{"ok":true,"result":{"message_id":1}}`), nil
		}
		return response(`{"ok":false,"error_code":400}`), nil
	})
	text := strings.Repeat("尚未发送的回答", 100)
	err := c.Transmit(context.Background(), &entity.Utterance{ChannelID: "42", CardData: data.Bytes(), FallbackText: "卡片原文", Text: text})
	var partial *entity.PartialDeliveryError
	if !errors.As(err, &partial) || partial.DeliveredText != "卡片原文" || partial.UndeliveredText != text {
		t.Fatalf("delivery semantics incorrect: %+v %v", partial, err)
	}
}

func TestImageDeliveryTextHandlesMixedMedia(t *testing.T) {
	done, pending := imageDeliveryText([]string{"已送达卡片", "", "待发送卡片", "待发送卡片"}, 2)
	if done != "已送达卡片" || pending != "待发送卡片" {
		t.Fatalf("incorrect mixed result: %q %q", done, pending)
	}
	_, pending = imageDeliveryText([]string{"", ""}, 1)
	if !strings.Contains(pending, "1 张图片") {
		t.Fatal("missing real image failure notice")
	}
}
