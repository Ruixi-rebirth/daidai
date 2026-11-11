package tool

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"daidai/internal/core"
	"golang.org/x/sys/unix"
)

const maxGeneratedImageBytes = core.MaxImageBytes

func validImageData(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(strings.SplitN(http.DetectContentType(data), ";", 2)[0])) {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return true
	default:
		return false
	}
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
