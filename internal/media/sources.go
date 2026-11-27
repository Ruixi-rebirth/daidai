package media

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// Sources resolves opaque platform file references without exposing credentials
// to the model, message history, or the generic HTTP downloader.
type Sources struct {
	mu      sync.RWMutex
	loaders map[string]func(context.Context, string, int64) ([]byte, error)
}

func (s *Sources) Register(scheme string, load func(context.Context, string, int64) ([]byte, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loaders == nil {
		s.loaders = make(map[string]func(context.Context, string, int64) ([]byte, error))
	}
	s.loaders[scheme] = load
}

func (s *Sources) HasScheme(scheme string) bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.loaders[scheme] != nil
}

func (s *Sources) Fetch(ctx context.Context, ref string, limit int64) ([]byte, bool, error) {
	if s == nil {
		return nil, false, nil
	}
	scheme, _, ok := strings.Cut(ref, ":")
	if !ok {
		return nil, false, nil
	}
	s.mu.RLock()
	load := s.loaders[scheme]
	s.mu.RUnlock()
	if load == nil {
		return nil, false, nil
	}
	data, err := load(ctx, ref, limit)
	if err == nil && int64(len(data)) > limit {
		return nil, true, fmt.Errorf("attachment exceeds size limit")
	}
	return data, true, err
}
