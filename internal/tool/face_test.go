package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"daidai/assets"
	"daidai/internal/entity"
)

func TestFaceTool_HitCache(t *testing.T) {
	tmpDir := t.TempDir()
	dummyBytes, err := assets.SmallFaces.ReadFile(filepath.Join("daidai_faces_small", "开心.png"))
	if err != nil {
		t.Fatalf("failed to read bundled test face: %v", err)
	}
	testFacePath := filepath.Join(tmpDir, "开心.png")
	if err := os.WriteFile(testFacePath, dummyBytes, 0644); err != nil {
		t.Fatalf("failed to write test face: %v", err)
	}

	tool := NewFaceToolWithDir(nil, tmpDir)
	rawArgs, _ := json.Marshal(map[string]any{"emotion": "开心"})

	res, err := tool.Execute(context.Background(), rawArgs)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	expectedBytes, _, err := normalizeImageData(context.Background(), dummyBytes)
	if err != nil {
		t.Fatalf("normalizeImageData failed: %v", err)
	}
	if !bytes.Equal(res.ImageData, expectedBytes) {
		t.Errorf("expected image data to match normalized bytes (got %d bytes, want %d bytes)", len(res.ImageData), len(expectedBytes))
	}
	if !res.ImageOnly || !res.FixedPresentation {
		t.Errorf("expected prepared face image with ImageOnly and FixedPresentation")
	}
}

func TestFaceTool_MissingDelegates(t *testing.T) {
	tmpDir := t.TempDir()
	tool := NewFaceToolWithDir(nil, tmpDir)
	rawArgs, _ := json.Marshal(map[string]any{"emotion": "不存在的表情"})

	res, err := tool.Execute(context.Background(), rawArgs)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	if len(res.ImageData) > 0 {
		t.Errorf("expected no image data when cache missed and no delegates")
	}
}

func TestFaceTool_NotLoggedIn_FailFast(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	tmpDir := t.TempDir()
	delegates := entity.NewDelegates(&dummyWorker{})
	tool := NewFaceToolWithDir(delegates, tmpDir)

	rawArgs, _ := json.Marshal(map[string]any{"emotion": "难过"})
	res, err := tool.Execute(context.Background(), rawArgs)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	if !strings.Contains(res.Text, "尚未登录") || !strings.Contains(res.Text, "/agy login") {
		t.Fatalf("expected fail-fast prompt about login, got: %s", res.Text)
	}
}

func TestFaceTool_GuestCacheMissReturnsTextWithoutFallback(t *testing.T) {
	tmpDir := t.TempDir()
	tool := NewFaceToolWithDir(nil, tmpDir)
	rawArgs, _ := json.Marshal(map[string]any{"emotion": "哈气"})
	ctx := entity.WithSensoryEvent(context.Background(), &entity.SensoryEvent{
		Platform: "qq",
		IsGroup:  true,
		Role:     entity.RoleGuest,
	})

	res, err := tool.Execute(ctx, rawArgs)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	if len(res.ImageData) > 0 || res.ImageOnly {
		t.Fatalf("expected no fallback image for guest cache miss, got %d bytes (ImageOnly=%t)", len(res.ImageData), res.ImageOnly)
	}
	if !strings.Contains(res.Text, "哈气") || !strings.Contains(res.Text, "暂无") {
		t.Fatalf("expected clear cache-miss explanation, got: %q", res.Text)
	}
}

func TestFaceTool_DefinitionForEventRestrictsGuestToCachedEnum(t *testing.T) {
	tmpDir := t.TempDir()
	dummyBytes, err := assets.SmallFaces.ReadFile(filepath.Join("daidai_faces_small", "开心.png"))
	if err != nil {
		t.Fatalf("failed to read bundled test face: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "全身.png"), dummyBytes, 0644); err != nil {
		t.Fatalf("failed to write custom face: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, ".hidden.png"), dummyBytes, 0644); err != nil {
		t.Fatalf("failed to write hidden file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "notes.txt"), []byte("ignore"), 0644); err != nil {
		t.Fatalf("failed to write non-image file: %v", err)
	}

	tool := NewFaceToolWithDir(nil, tmpDir)

	guestDef, ok := tool.DefinitionForEvent(&entity.SensoryEvent{Platform: "qq", IsGroup: true, Role: entity.RoleGuest})
	if !ok {
		t.Fatal("expected guest face definition to be included")
	}
	props := guestDef.Function.Parameters["properties"].(map[string]any)
	emotionProp := props["emotion"].(map[string]any)
	enumVals, ok := emotionProp["enum"].([]string)
	if !ok || len(enumVals) == 0 {
		t.Fatalf("expected non-empty enum for guest, got %#v", emotionProp["enum"])
	}
	hasFullBody, hasHappy := false, false
	for _, v := range enumVals {
		if v == "全身" {
			hasFullBody = true
		}
		if v == "开心" {
			hasHappy = true
		}
		if v == ".hidden" || v == "notes" || v == "哈气" {
			t.Fatalf("unexpected entry %q in guest enum: %v", v, enumVals)
		}
	}
	if !hasFullBody || !hasHappy {
		t.Fatalf("expected both custom 全身 and bundled 开心 in enum, got %v", enumVals)
	}

	for _, role := range []entity.UserRole{entity.RolePartner, entity.RoleCollaborator} {
		def, ok := tool.DefinitionForEvent(&entity.SensoryEvent{Platform: "qq", IsGroup: true, Role: role})
		if !ok {
			t.Fatalf("expected %s face definition to be included", role)
		}
		p := def.Function.Parameters["properties"].(map[string]any)["emotion"].(map[string]any)
		if _, restricted := p["enum"]; restricted {
			t.Fatalf("expected %s emotion schema to stay unrestricted, got enum=%v", role, p["enum"])
		}
	}
}
