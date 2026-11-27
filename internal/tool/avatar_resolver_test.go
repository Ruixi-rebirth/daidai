package tool

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"daidai/internal/core"
	"daidai/internal/db"
	"daidai/internal/entity"
	"daidai/internal/media"
)

func tinyPNGBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestAvatarResolver_ResolvesSelfMentionQuoteAndHistory(t *testing.T) {
	ctx := context.Background()
	dbMgr := db.NewManager(filepath.Join(t.TempDir(), "db"))
	defer func() { _ = dbMgr.Close() }()

	if _, err := dbMgr.LogMessage(ctx, "qq", "group", "g1", "OPENID_YUE", "月", "大家好", false, "", ""); err != nil {
		t.Fatal(err)
	}

	sources := &media.Sources{}
	sources.Register("qq-avatar", func(context.Context, string, int64) ([]byte, error) {
		return tinyPNGBytes(t), nil
	})
	resolver := NewAvatarResolver(dbMgr, sources)

	event := &entity.SensoryEvent{
		Platform:   "qq",
		IsGroup:    true,
		ChannelID:  "g1",
		SenderID:   "OPENID_SELF",
		SenderName: "陈念秋",
		MentionedUsers: []core.GroupParticipant{
			{ID: "OPENID_MENTIONED", Name: "小明"},
		},
		ReplyContext: &entity.SensoryEvent{
			SenderID:   "OPENID_QUOTED",
			SenderName: "小红",
		},
	}

	if !resolver.SupportsPlatform("qq") {
		t.Fatal("expected qq platform to be supported")
	}
	if resolver.SupportsPlatform("telegram") {
		t.Fatal("expected unregistered telegram platform not to be supported")
	}

	name, ref, err := resolver.Resolve(ctx, event, "self")
	if err != nil || name != "陈念秋" || ref != "qq-avatar:OPENID_SELF" {
		t.Fatalf("self resolve = (%q, %q, %v)", name, ref, err)
	}

	name, ref, err = resolver.Resolve(ctx, event, "小明")
	if err != nil || name != "小明" || ref != "qq-avatar:OPENID_MENTIONED" {
		t.Fatalf("mentioned resolve = (%q, %q, %v)", name, ref, err)
	}

	name, ref, err = resolver.Resolve(ctx, event, "小红")
	if err != nil || name != "小红" || ref != "qq-avatar:OPENID_QUOTED" {
		t.Fatalf("quoted resolve = (%q, %q, %v)", name, ref, err)
	}

	name, ref, err = resolver.Resolve(ctx, event, "月")
	if err != nil || name != "月" || ref != "qq-avatar:OPENID_YUE" {
		t.Fatalf("history resolve = (%q, %q, %v)", name, ref, err)
	}

	// Pronoun targeting quoted message author.
	name, ref, err = resolver.Resolve(ctx, event, "他")
	if err != nil || name != "小红" || ref != "qq-avatar:OPENID_QUOTED" {
		t.Fatalf("quoted pronoun resolve = (%q, %q, %v)", name, ref, err)
	}

	// Duplicate nickname in history returns disambiguation error unless @-mentioned.
	if _, err := dbMgr.LogMessage(ctx, "qq", "group", "g1", "OPENID_DUP_1", "阿强", "你好", false, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := dbMgr.LogMessage(ctx, "qq", "group", "g1", "OPENID_DUP_2", "阿强", "我也叫阿强", false, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolver.Resolve(ctx, event, "阿强"); err == nil || !strings.Contains(err.Error(), "2 位") {
		t.Fatalf("expected duplicate nickname error, got %v", err)
	}
	eventWithMention := *event
	eventWithMention.MentionedUsers = []core.GroupParticipant{{ID: "OPENID_DUP_1", Name: "阿强"}}
	name, ref, err = resolver.Resolve(ctx, &eventWithMention, "阿强")
	if err != nil || name != "阿强" || ref != "qq-avatar:OPENID_DUP_1" {
		t.Fatalf("explicit mention should disambiguate duplicate nickname, got (%q, %q, %v)", name, ref, err)
	}
}

func TestImageTool_PrepareReferenceImages_QuoteAndAvatar(t *testing.T) {
	ctx := context.Background()
	dbMgr := db.NewManager(filepath.Join(t.TempDir(), "db"))
	defer func() { _ = dbMgr.Close() }()
	if _, err := dbMgr.LogMessage(ctx, "qq", "group", "g1", "OPENID_YUE", "月", "在的", false, "", ""); err != nil {
		t.Fatal(err)
	}

	samplePNG := tinyPNGBytes(t)
	sources := &media.Sources{}
	sources.Register("qq-avatar", func(_ context.Context, ref string, _ int64) ([]byte, error) {
		if ref != "qq-avatar:OPENID_YUE" {
			t.Fatalf("unexpected avatar ref: %s", ref)
		}
		return samplePNG, nil
	})
	sources.Register("fixture", func(_ context.Context, ref string, _ int64) ([]byte, error) {
		if ref != "fixture:cat" {
			t.Fatalf("unexpected image ref: %s", ref)
		}
		return samplePNG, nil
	})

	imgTool := NewImageTool(nil)
	imgTool.SetMediaSources(sources, NewAvatarResolver(dbMgr, sources))

	workDir := t.TempDir()
	event := &entity.SensoryEvent{
		Platform:  "qq",
		IsGroup:   true,
		ChannelID: "g1",
		SenderID:  "OPENID_GUEST",
		ReplyContext: &entity.SensoryEvent{
			Images: []*core.Attachment{{URL: "fixture:cat"}},
		},
	}

	refs, err := imgTool.prepareReferenceImages(ctx, event, workDir, imageArgs{
		Prompt:           "把猫换成月的头像",
		ReferenceSources: []string{entity.QuotedImageSource},
		AvatarUsers:      []string{"月"},
	})
	if err != nil {
		t.Fatalf("prepareReferenceImages failed: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("expected 2 reference images, got %d: %+v", len(refs), refs)
	}
	for _, ref := range refs {
		if _, err := os.Stat(ref.Path); err != nil {
			t.Fatalf("reference file missing at %s: %v", ref.Path, err)
		}
	}

	goal, err := buildImageGenGoal("把猫换成月的头像", filepath.Join(workDir, "out.png"), refs)
	if err != nil {
		t.Fatalf("buildImageGenGoal failed: %v", err)
	}
	if !strings.Contains(goal, "引用消息参考图") || !strings.Contains(goal, "用户「月」的头像参考图") {
		t.Fatalf("expected both reference labels in goal, got:\n%s", goal)
	}
}
