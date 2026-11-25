package media

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeVideoData_Validation(t *testing.T) {
	if _, err := NormalizeVideoData(context.Background(), nil); err == nil {
		t.Fatal("expected error for empty video data")
	}
	oversized := make([]byte, MaxRawVideoInputBytes+1)
	if _, err := NormalizeVideoData(context.Background(), oversized); err == nil {
		t.Fatal("expected error for oversized video data")
	}
	if _, err := exec.LookPath("ffprobe"); err == nil {
		if _, err := NormalizeVideoData(context.Background(), []byte("not-a-video")); err == nil {
			t.Fatal("expected invalid video payload to fail probe")
		}
	}
}

func TestNormalizeVideoData_TranscodesValidVideo(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed in PATH")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed in PATH")
	}

	tmpDir := t.TempDir()
	srcPath := filepath.Join(tmpDir, "sample.mp4")
	cmd := exec.Command("ffmpeg", "-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=c=blue:s=321x241:d=1",
		"-c:v", "libx264", "-vf", "pad=ceil(iw/2)*2:ceil(ih/2)*2", srcPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create synthetic mp4 failed: %v (%s)", err, string(out))
	}
	raw, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}

	normalized, err := NormalizeVideoData(context.Background(), raw)
	if err != nil {
		t.Fatalf("NormalizeVideoData failed: %v", err)
	}
	if len(normalized) == 0 {
		t.Fatal("expected non-empty normalized MP4")
	}
	// Verify MP4 ftyp box header near start.
	if len(normalized) < 12 || !strings.Contains(string(normalized[:16]), "ftyp") {
		t.Fatalf("expected MP4 ftyp header, got %q", string(normalized[:min(16, len(normalized))]))
	}
}
