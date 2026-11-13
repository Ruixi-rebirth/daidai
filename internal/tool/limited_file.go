package tool

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"daidai/internal/core"
	"daidai/internal/media"
	"golang.org/x/sys/unix"
)

const maxGeneratedImageBytes = core.MaxImageBytes

func normalizeImageData(ctx context.Context, data []byte) ([]byte, string, error) {
	return media.NormalizeImageData(ctx, data)
}

func imageContentType(data []byte) string {
	return media.ContentType(data)
}

func readFileLimited(path string, maxBytes int64) ([]byte, error) {
	if maxBytes < 0 {
		return nil, fmt.Errorf("maximum file size cannot be negative")
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("failed to open file descriptor for %q", path)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("refusing to read non-regular file %q", path)
	}

	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("file exceeded %d byte limit", maxBytes)
	}
	return data, nil
}

// readGeneratedImage prefers the requested output path, then recovers a raster
// image the generator may have saved under a different name in its isolated workspace.
func readGeneratedImage(ctx context.Context, outputPath, workspace string, excludedPaths ...string) ([]byte, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if data, err := readFileLimited(outputPath, maxGeneratedImageBytes); err == nil {
		return data, outputPath, nil
	} else if !os.IsNotExist(err) {
		return nil, "", err
	}

	excluded := make(map[string]struct{}, len(excludedPaths)+1)
	for _, path := range append(excludedPaths, outputPath) {
		absPath, err := filepath.Abs(path)
		if err == nil {
			excluded[absPath] = struct{}{}
		}
	}

	var candidates []string
	err := filepath.WalkDir(workspace, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != workspace && strings.Count(strings.TrimPrefix(path, workspace), string(filepath.Separator)) >= 2 {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		absPath, err := filepath.Abs(path)
		if err != nil {
			return nil
		}
		if _, skip := excluded[absPath]; skip {
			return nil
		}
		candidates = append(candidates, path)
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	for _, path := range candidates {
		data, err := readFileLimited(path, maxGeneratedImageBytes)
		if err != nil || !strings.HasPrefix(imageContentType(data), "image/") {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		return data, path, nil
	}
	return nil, "", os.ErrNotExist
}
