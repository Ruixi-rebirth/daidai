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
	if res.Text != "已发送呆呆的【开心】图片" {
		t.Errorf("unexpected text: %s", res.Text)
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
	origHome := os.Getenv("HOME")
	t.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

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
