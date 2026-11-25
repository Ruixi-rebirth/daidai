package memory

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"daidai/internal/core"
)

var (
	defaultEmbedderMu      sync.RWMutex
	defaultEmbedder        core.EmbeddingProvider
	defaultEmbedderVersion uint64
	embedCacheMu           sync.RWMutex
	embedCache             = make(map[embedCacheKey][]float32, 256)
)

type embedCacheKey struct {
	version  uint64
	identity string
	text     string
}

// SetDefaultEmbedder sets the process-wide embedding provider and invalidates cached vectors.
func SetDefaultEmbedder(emb core.EmbeddingProvider) {
	defaultEmbedderMu.Lock()
	defaultEmbedder = emb
	defaultEmbedderVersion++
	defaultEmbedderMu.Unlock()

	embedCacheMu.Lock()
	embedCache = make(map[embedCacheKey][]float32, 256)
	embedCacheMu.Unlock()
}

// GetDefaultEmbedder returns the process-wide embedding provider.
func GetDefaultEmbedder() core.EmbeddingProvider {
	emb, _ := defaultEmbedderSnapshot()
	return emb
}

func defaultEmbedderSnapshot() (core.EmbeddingProvider, uint64) {
	defaultEmbedderMu.RLock()
	defer defaultEmbedderMu.RUnlock()
	return defaultEmbedder, defaultEmbedderVersion
}

// EmbedText encodes text with the configured provider; unavailable or failed providers return nil.
func EmbedText(ctx context.Context, text string) []float32 {
	vec, _ := EmbedTextWithIdentity(ctx, text)
	return vec
}

func EmbedTextWithIdentity(ctx context.Context, text string) ([]float32, string) {
	emb, version := defaultEmbedderSnapshot()
	return embedCachedWithProvider(ctx, emb, version, text)
}

// EmbedTextWithProvider encodes text using an explicit embedding provider with shared bounded caching.
func EmbedTextWithProvider(ctx context.Context, emb core.EmbeddingProvider, text string) ([]float32, string) {
	if emb == nil {
		return EmbedTextWithIdentity(ctx, text)
	}
	_, version := defaultEmbedderSnapshot()
	return embedCachedWithProvider(ctx, emb, version, text)
}

func embedCachedWithProvider(ctx context.Context, emb core.EmbeddingProvider, version uint64, text string) ([]float32, string) {
	clean := strings.TrimSpace(text)
	if emb == nil || clean == "" {
		return nil, ""
	}
	identity := embeddingIdentity(emb)
	key := embedCacheKey{version: version, identity: identity, text: clean}

	embedCacheMu.RLock()
	if cached, ok := embedCache[key]; ok {
		embedCacheMu.RUnlock()
		return append([]float32(nil), cached...), identity
	}
	embedCacheMu.RUnlock()

	vec, actualIdentity, err := embedWithIdentity(ctx, emb, clean)
	if err != nil || len(vec) == 0 {
		return nil, ""
	}
	if actualIdentity == "" {
		actualIdentity = identity
	}

	embedCacheMu.Lock()
	if len(embedCache) >= 512 {
		for k := range embedCache {
			delete(embedCache, k)
			break
		}
	}
	if actualIdentity == identity {
		embedCache[key] = append([]float32(nil), vec...)
	}
	embedCacheMu.Unlock()
	return append([]float32(nil), vec...), actualIdentity
}

func embeddingIdentity(emb core.EmbeddingProvider) string {
	if emb == nil {
		return ""
	}
	if identified, ok := emb.(core.IdentifiedEmbeddingProvider); ok {
		if identity := strings.TrimSpace(identified.EmbeddingIdentity()); identity != "" {
			return identity
		}
	}
	return fmt.Sprintf("%T", emb)
}

func embedWithIdentity(ctx context.Context, emb core.EmbeddingProvider, text string) ([]float32, string, error) {
	if identified, ok := emb.(core.IdentifiedEmbeddingProvider); ok {
		return identified.EmbedIdentified(ctx, text)
	}
	vec, err := emb.Embed(ctx, text)
	return vec, embeddingIdentity(emb), err
}
