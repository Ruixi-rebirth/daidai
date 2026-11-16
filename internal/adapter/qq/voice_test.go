package qq

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"daidai/internal/entity"
)

func TestQQChannel_VoiceMessageWithASR(t *testing.T) {
	channel := &QQChannel{
		dedup:  newDedupCache(100),
		immune: entity.NewImmuneSystem("USER_VOICE_123"),
	}

	events := startTestQQChannel(t, channel)
	channel.voiceModeMu.Lock()
	channel.voiceMode = VoiceModeFast
	channel.voiceModeMu.Unlock()

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

	channel.dispatchEvent(context.Background(), payload)

	capturedEvent := awaitTestQQEvent(t, events)
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

	events := startTestQQChannel(t, channel)

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

	channel.dispatchEvent(context.Background(), payload)

	capturedEvent := awaitTestQQEvent(t, events)
	if capturedEvent.Content != "【收到一段语音消息，但未能识别出文字内容】" {
		t.Fatalf("expected fallback voice content, got %q", capturedEvent.Content)
	}
	if len(capturedEvent.Files) != 0 {
		t.Fatalf("expected 0 files, got %d", len(capturedEvent.Files))
	}
}

func TestIgnoredVoiceMessagesDoNotStartTranscription(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	for _, reason := range []string{"duplicate", "bot", "unknown_event", "cancelled"} {
		t.Run(reason, func(t *testing.T) {
			logs.Reset()
			channel := &QQChannel{dedup: newDedupCache(100)}
			author := map[string]any{"user_openid": "sender", "bot": reason == "bot"}
			payload := map[string]any{
				"t": "C2C_MESSAGE_CREATE",
				"d": map[string]any{
					"id": "ignored-voice", "author": author,
					"attachments": []any{map[string]any{
						"content_type": "voice", "url": "file:///untrusted-voice.wav",
					}},
				},
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch reason {
			case "duplicate":
				channel.dedup.Remember("ignored-voice")
			case "unknown_event":
				payload["t"] = "IGNORED_EVENT"
			case "cancelled":
				cancel()
			}
			accepted := channel.dispatchEvent(ctx, payload)
			if accepted != (reason != "cancelled") {
				t.Fatalf("unexpected sequence acceptance: %v", accepted)
			}
			// An attempted download of this URL fails validation and logs a warning.
			if logs.Len() != 0 {
				t.Fatalf("ignored message started attachment processing: %s", logs.String())
			}
		})
	}
}
