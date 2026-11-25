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
	images := [][]byte{nil}
	sources := []string{"已丢弃的图片正文"}
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

func TestPartialTelegramReplyRetainsLinkDestination(t *testing.T) {
	c := testChannel(t)
	calls := 0
	c.http.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return response(`{"ok":true,"result":{"message_id":1}}`), nil
		}
		return response(`{"ok":false,"error_code":400}`), nil
	})
	first := strings.Repeat("前", 1800)
	const original = "https://example.com/article"
	err := c.Transmit(context.Background(), &entity.Utterance{ChannelID: "42", Text: first + "\n\n[原文](" + original + ")"})
	var partial *entity.PartialDeliveryError
	if !errors.As(err, &partial) || strings.Contains(partial.DeliveredText, original) || !strings.Contains(partial.UndeliveredText, original) {
		t.Fatalf("link destination did not survive partial send: %+v", partial)
	}
}

func TestTelegramImageDeliveryRequiresDistinctMessageIDs(t *testing.T) {
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct {
		name    string
		images  int
		receipt string
	}{
		{"photo missing ID", 1, `{}`},
		{"album missing ID", 2, `[{"message_id":1},{}]`},
		{"album duplicate ID", 2, `[{"message_id":1},{"message_id":1}]`},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			c := testChannel(t)
			c.http.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
				return response(`{"ok":true,"result":` + scenario.receipt + `}`), nil
			})
			u := &entity.Utterance{ChannelID: "42"}
			for range scenario.images {
				u.Images = append(u.Images, data.Bytes())
				u.ImageSources = append(u.ImageSources, "卡片正文")
			}
			err := c.Transmit(context.Background(), u)
			var partial *entity.PartialDeliveryError
			if !errors.As(err, &partial) || partial.DeliveredText != "" || partial.UndeliveredText != "卡片正文" {
				t.Fatalf("invalid receipt was accepted: %v", err)
			}
		})
	}
}

func TestTelegramTextWithoutReceiptPreservesOnlyConfirmedUnsentTail(t *testing.T) {
	c := testChannel(t)
	calls := 0
	c.http.Transport = transportFunc(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return response(`{"ok":true,"result":{"message_id":1}}`), nil
		}
		return response(`{"ok":true,"result":{}}`), nil
	})
	first := strings.Repeat("前", 1800)
	uncertain := strings.Repeat("中", 1800)
	err := c.Transmit(context.Background(), &entity.Utterance{ChannelID: "42", Text: first + uncertain + "尾"})
	var partial *entity.PartialDeliveryError
	if !errors.As(err, &partial) || calls != 2 || partial.DeliveredText != first || partial.UndeliveredText != "尾" {
		t.Fatalf("incorrect receipt accounting: calls=%d, err=%v", calls, err)
	}
}

func TestTelegramSendVideoDeliversMP4AndCaption(t *testing.T) {
	c := testChannel(t)
	var calledPath string
	c.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		calledPath = r.URL.Path
		return response(`{"ok":true,"result":{"message_id":99}}`), nil
	})
	err := c.Transmit(context.Background(), &entity.Utterance{
		ChannelID: "42",
		Text:      "动画渲染好了",
		Videos:    [][]byte{[]byte("fake-mp4-bytes")},
	})
	if err != nil {
		t.Fatalf("Transmit video failed: %v", err)
	}
	if !strings.HasSuffix(calledPath, "/sendVideo") {
		t.Fatalf("expected /sendVideo call, got %q", calledPath)
	}
}
