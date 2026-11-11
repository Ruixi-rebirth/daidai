package core

import (
	"bytes"
	"fmt"
	"io"
	"sync"
)

// ReadBounded reads at most maxBytes and returns an error if the input exceeds
// that limit.
func ReadBounded(r io.Reader, maxBytes int64) ([]byte, error) {
	if maxBytes < 0 {
		return nil, fmt.Errorf("maximum read size cannot be negative")
	}
	data, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("response exceeds %d byte limit", maxBytes)
	}
	return data, nil
}

// LimitedBuffer captures at most maxBytes while continuing to accept writes,
// so subprocess pipes keep draining after the capture limit is reached.
type LimitedBuffer struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	maxBytes  int
	truncated bool
}

func NewLimitedBuffer(maxBytes int) *LimitedBuffer {
	if maxBytes < 0 {
		maxBytes = 0
	}
	return &LimitedBuffer{maxBytes: maxBytes}
}

func (b *LimitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	remaining := b.maxBytes - b.buffer.Len()
	if remaining > len(p) {
		remaining = len(p)
	}
	if remaining > 0 {
		_, _ = b.buffer.Write(p[:remaining])
	}
	if remaining < len(p) {
		b.truncated = true
	}
	return len(p), nil
}

func (b *LimitedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buffer.Bytes()...)
}

func (b *LimitedBuffer) String() string {
	return string(b.Bytes())
}

func (b *LimitedBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}
