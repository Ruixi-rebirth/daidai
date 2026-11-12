package media

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"daidai/internal/core"
)

const maxDecodedImagePixels = core.MaxImagePixels

var imageDecodeSlots = make(chan struct{}, 2)

// NormalizeImageData decodes an image by content and returns a bounded PNG.
// FFmpeg handles formats outside Go's standard decoder registry.
func NormalizeImageData(ctx context.Context, data []byte) ([]byte, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if len(data) == 0 {
		return nil, "", fmt.Errorf("image data is empty")
	}
	if int64(len(data)) > core.MaxImageBytes {
		return nil, "", fmt.Errorf("image data exceeds %d byte limit", core.MaxImageBytes)
	}
	select {
	case imageDecodeSlots <- struct{}{}:
		defer func() { <-imageDecodeSlots }()
	case <-ctx.Done():
		return nil, "", ctx.Err()
	}

	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	var decodeErr error
	if err == nil {
		if err := validateDimensions(config); err != nil {
			return nil, format, err
		}
		decoded, decodedFormat, decodeErr := image.Decode(bytes.NewReader(data))
		if decodeErr == nil {
			if decodedFormat != format {
				return nil, format, fmt.Errorf("image format changed while decoding: %s to %s", format, decodedFormat)
			}
			return encodePNG(decoded, format)
		}
	}

	converted, convertErr := convertWithFFmpeg(ctx, data)
	if convertErr != nil {
		if err != nil {
			return nil, ContentType(data), fmt.Errorf("detect image format: %v; ffmpeg fallback: %w", err, convertErr)
		}
		return nil, format, fmt.Errorf("decode %s image: %v; ffmpeg fallback: %w", format, decodeErr, convertErr)
	}
	decoded, _, err := image.Decode(bytes.NewReader(converted))
	if err != nil {
		return nil, format, fmt.Errorf("decode ffmpeg PNG output: %w", err)
	}
	if format == "" {
		format = strings.TrimPrefix(ContentType(data), "image/")
	}
	return encodePNG(decoded, format+" (ffmpeg)")
}

func validateDimensions(config image.Config) error {
	return core.ValidateImageDimensions(config.Width, config.Height)
}

func encodePNG(decoded image.Image, format string) ([]byte, string, error) {
	if err := validateDimensions(image.Config{Width: decoded.Bounds().Dx(), Height: decoded.Bounds().Dy()}); err != nil {
		return nil, format, err
	}
	var normalized bytes.Buffer
	if err := png.Encode(&normalized, decoded); err != nil {
		return nil, format, fmt.Errorf("encode normalized PNG: %w", err)
	}
	if int64(normalized.Len()) > core.MaxImageBytes {
		return nil, format, fmt.Errorf("normalized PNG exceeds %d byte limit", core.MaxImageBytes)
	}
	return normalized.Bytes(), format, nil
}

func convertWithFFmpeg(parent context.Context, data []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	// Inputs can come from user uploads. Only stdin, stdout, and embedded data
	// are needed; disabling file/network protocols prevents format handlers such
	// as SVG from opening external resources.
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin", "-max_pixels", fmt.Sprint(maxDecodedImagePixels), "-protocol_whitelist", "pipe,data", "-i", "pipe:0", "-frames:v", "1", "-f", "image2pipe", "-vcodec", "png", "pipe:1")
	cmd.Stdin = bytes.NewReader(data)
	output := core.NewLimitedBuffer(int(core.MaxImageBytes))
	stderr := core.NewLimitedBuffer(32 << 10)
	cmd.Stdout = output
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if output.Truncated() {
			return nil, fmt.Errorf("ffmpeg output exceeds %d byte limit", core.MaxImageBytes)
		}
		detail := strings.TrimSpace(stderr.String())
		if stderr.Truncated() {
			detail += " [stderr truncated]"
		}
		if detail != "" {
			return nil, fmt.Errorf("%s (%w)", detail, err)
		}
		return nil, err
	}
	if output.Truncated() {
		return nil, fmt.Errorf("ffmpeg output exceeds %d byte limit", core.MaxImageBytes)
	}
	converted := output.Bytes()
	if len(converted) < 8 || !bytes.HasPrefix(converted, []byte("\x89PNG\r\n\x1a\n")) {
		return nil, fmt.Errorf("ffmpeg output is not a valid PNG")
	}
	return converted, nil
}

// ContentType detects a media type from bytes without trusting a filename.
func ContentType(data []byte) string {
	return strings.ToLower(strings.TrimSpace(strings.SplitN(http.DetectContentType(data), ";", 2)[0]))
}
