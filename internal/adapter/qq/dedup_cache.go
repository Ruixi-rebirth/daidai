package qq

import (
	"sync"
	"time"
)

type dedupCache struct {
	mu   sync.Mutex
	seen map[string]int64
	max  int
}

func newDedupCache(max int) *dedupCache {
	if max <= 0 {
		max = 2000
	}
	return &dedupCache{
		seen: make(map[string]int64),
		max:  max,
	}
}

func (d *dedupCache) CheckAndSet(msgID string) bool {
	if msgID == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	if _, exists := d.seen[msgID]; exists {
		return true
	}
	d.rememberLocked(msgID)
	return false
}

func (d *dedupCache) Contains(msgID string) bool {
	if msgID == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_, exists := d.seen[msgID]
	return exists
}

func (d *dedupCache) Remember(msgID string) {
	if msgID == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rememberLocked(msgID)
}

func (d *dedupCache) rememberLocked(msgID string) {
	now := time.Now().Unix()

	if len(d.seen) >= d.max {
		oldestKey := ""
		var oldestTS int64
		for k, ts := range d.seen {
			if now-ts > 300 {
				delete(d.seen, k)
			} else if oldestKey == "" || ts < oldestTS {
				oldestKey = k
				oldestTS = ts
			}
		}
		if len(d.seen) >= d.max && oldestKey != "" {
			delete(d.seen, oldestKey)
		}
	}
	d.seen[msgID] = now
}
