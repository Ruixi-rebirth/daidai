package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestFaceTool_HitCache(t *testing.T) {
	tmpDir := t.TempDir()
	dummyBytes := []byte("fake-png-data")
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

	if string(res.ImageData) != string(dummyBytes) {
		t.Errorf("expected image data to match dummy bytes, got %s", string(res.ImageData))
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
