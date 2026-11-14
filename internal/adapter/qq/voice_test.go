package qq

import (
	"context"
	"testing"
	"time"

	"daidai/internal/entity"
)

func TestQQChannel_VoiceMessageWithASR(t *testing.T) {
	channel := &QQChannel{
		dedup:  newDedupCache(100),
		immune: entity.NewImmuneSystem("USER_VOICE_123"),
	}

	var capturedEvent *entity.SensoryEvent
	_ = channel.Start(context.Background(), func(event *entity.SensoryEvent) {
		capturedEvent = event
	})

	payload := map[string]any{
		"t": "C2C_MESSAGE_CREATE",
		"d": map[string]any{
			"id":      "msg_voice_001",
			"content": "",
			"author": map[string]any{
				"user_openid": "USER_VOICE_123",
				"username":    "测试用户",
			},
			"attachments": []any{
				map[string]any{
					"content_type":   "voice",
					"url":            "https://multimedia.nt.qq.com.cn/download?file=voice.slk",
					"voice_wav_url":  "https://multimedia.nt.qq.com.cn/download?file=voice.wav",
					"filename":       "voice.slk",
					"asr_refer_text": "呆呆今天星期几",
				},
			},
		},
	}

	channel.dispatchEvent(payload)

	// 等待防抖窗口触发 (测试环境合并)
	time.Sleep(2200 * time.Millisecond)

	if capturedEvent == nil {
		t.Fatalf("expected sensory event dispatched for voice message")
	}
	if capturedEvent.Content != "呆呆今天星期几" {
		t.Fatalf("expected content '呆呆今天星期几', got %q", capturedEvent.Content)
	}
	if len(capturedEvent.Files) != 0 {
		t.Fatalf("expected 0 files, got %d (voice message must not be treated as generic file)", len(capturedEvent.Files))
	}
	if len(capturedEvent.Images) != 0 {
		t.Fatalf("expected 0 images, got %d", len(capturedEvent.Images))
	}
	if capturedEvent.HistoryAttachmentNote != "" {
		t.Fatalf("expected empty HistoryAttachmentNote, got %q", capturedEvent.HistoryAttachmentNote)
	}
}

func TestQQChannel_VoiceMessageWithoutASRFallback(t *testing.T) {
	channel := &QQChannel{
		dedup:  newDedupCache(100),
		immune: entity.NewImmuneSystem("USER_VOICE_456"),
	}

	var capturedEvent *entity.SensoryEvent
	_ = channel.Start(context.Background(), func(event *entity.SensoryEvent) {
		capturedEvent = event
	})

	payload := map[string]any{
		"t": "C2C_MESSAGE_CREATE",
		"d": map[string]any{
			"id":      "msg_voice_002",
			"content": "",
			"author": map[string]any{
				"user_openid": "USER_VOICE_456",
				"username":    "测试用户2",
			},
			"attachments": []any{
				map[string]any{
					"content_type":   "voice",
					"url":            "",
					"voice_wav_url":  "",
					"filename":       "audio.amr",
					"asr_refer_text": "",
				},
			},
		},
	}

	channel.dispatchEvent(payload)

	time.Sleep(2200 * time.Millisecond)

	if capturedEvent == nil {
		t.Fatalf("expected sensory event dispatched")
	}
	if capturedEvent.Content != "【收到一段语音消息，但未能识别出文字内容】" {
		t.Fatalf("expected fallback voice content, got %q", capturedEvent.Content)
	}
	if len(capturedEvent.Files) != 0 {
		t.Fatalf("expected 0 files, got %d", len(capturedEvent.Files))
	}
}
