package media

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"daidai/internal/core"
)

const (
	// MaxRawVideoInputBytes limits raw generated video bytes accepted before MP4 normalization.
	MaxRawVideoInputBytes = 25 << 20
	// MaxVideoDurationSeconds caps outbound normalized video duration.
	MaxVideoDurationSeconds = 60
	// MaxVideoDimension bounds both width and height after scaling.
	MaxVideoDimension = 1280
)

var videoEncodeSlots = make(chan struct{}, 1)

// VideoInfo summarizes probed dimensions and container/stream metadata.
type VideoInfo struct {
	Width    int
	Height   int
	HasAudio bool
}

// NormalizeVideoData validates raw video bytes and transcodes them into a bounded
// H.264/AAC faststart MP4 suitable for QQ and Telegram delivery.
func NormalizeVideoData(ctx context.Context, data []byte) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("video data is empty")
	}
	if int64(len(data)) > MaxRawVideoInputBytes {
		return nil, fmt.Errorf("video data exceeds %d byte limit", MaxRawVideoInputBytes)
	}

	select {
	case videoEncodeSlots <- struct{}{}:
		defer func() { <-videoEncodeSlots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	tmpDir, err := os.MkdirTemp("", "daidai-vid-*")
	if err != nil {
		return nil, fmt.Errorf("create video temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()
	if err := os.Chmod(tmpDir, 0700); err != nil {
		return nil, fmt.Errorf("secure video temp dir: %w", err)
	}

	inPath := filepath.Join(tmpDir, "input.bin")
	if err := os.WriteFile(inPath, data, 0600); err != nil {
		return nil, fmt.Errorf("write input video: %w", err)
	}

	if _, err := probeVideoFile(ctx, inPath); err != nil {
		return nil, err
	}

	outPath := filepath.Join(tmpDir, "normalized.mp4")
	for _, crf := range []int{23, 28} {
		if err := transcodeMP4(ctx, inPath, outPath, crf); err != nil {
			return nil, err
		}
		normalized, err := os.ReadFile(outPath)
		if err != nil {
			return nil, fmt.Errorf("read normalized video: %w", err)
		}
		if len(normalized) == 0 {
			return nil, fmt.Errorf("normalized MP4 is empty")
		}
		if int64(len(normalized)) <= core.MaxVideoBytes {
			return normalized, nil
		}
	}
	return nil, fmt.Errorf("normalized MP4 exceeds %d byte limit", core.MaxVideoBytes)
}

func probeVideoFile(parent context.Context, inputPath string) (VideoInfo, error) {
	probeCtx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()

	cmd := core.CommandProcessGroup(probeCtx, "ffprobe",
		"-v", "error",
		"-show_entries", "stream=codec_type,width,height",
		"-of", "json",
		inputPath,
	)
	output := core.NewLimitedBuffer(256 << 10)
	stderr := core.NewLimitedBuffer(64 << 10)
	cmd.Stdout = output
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return VideoInfo{}, fmt.Errorf("probe video failed: %s", detail)
	}

	var parsed struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(output.Bytes(), &parsed); err != nil {
		return VideoInfo{}, fmt.Errorf("parse ffprobe output: %w", err)
	}

	var info VideoInfo
	for _, stream := range parsed.Streams {
		switch stream.CodecType {
		case "video":
			if info.Width == 0 && stream.Width > 0 && stream.Height > 0 {
				info.Width = stream.Width
				info.Height = stream.Height
			}
		case "audio":
			info.HasAudio = true
		}
	}
	if info.Width <= 0 || info.Height <= 0 {
		return VideoInfo{}, fmt.Errorf("file does not contain a valid video stream")
	}
	return info, nil
}

func transcodeMP4(parent context.Context, inputPath, outputPath string, crf int) error {
	encodeCtx, cancel := context.WithTimeout(parent, 90*time.Second)
	defer cancel()

	vf := fmt.Sprintf("scale='min(%d,iw)':'min(%d,ih)':force_original_aspect_ratio=decrease,pad=ceil(iw/2)*2:ceil(ih/2)*2", MaxVideoDimension, MaxVideoDimension)
	args := []string{
		"-y",
		"-hide_banner",
		"-loglevel", "error",
		"-nostdin",
		"-i", inputPath,
		"-t", strconv.Itoa(MaxVideoDurationSeconds),
		"-map", "0:v:0",
		"-map", "0:a:0?",
		"-vf", vf,
		"-c:v", "libx264",
		"-pix_fmt", "yuv420p",
		"-preset", "veryfast",
		"-crf", strconv.Itoa(crf),
		"-c:a", "aac",
		"-b:a", "128k",
		"-movflags", "+faststart",
		"-f", "mp4",
		outputPath,
	}

	cmd := core.CommandProcessGroup(encodeCtx, "ffmpeg", args...)
	stderr := core.NewLimitedBuffer(64 << 10)
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if _, lookErr := exec.LookPath("ffmpeg"); lookErr != nil {
			return fmt.Errorf("ffmpeg is unavailable: %w", lookErr)
		}
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("ffmpeg video transcode failed: %s", detail)
	}
	return nil
}
