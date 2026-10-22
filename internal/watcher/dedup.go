package watcher

import (
	"sync"
	"time"
)

// Deduplicator suppresses duplicate WorkloadEvents within a configurable TTL window.
type Deduplicator struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]time.Time
	nowFunc func() time.Time
}

// NewDeduplicator creates a TTL-based event deduplicator.
func NewDeduplicator(ttl time.Duration) *Deduplicator {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &Deduplicator{
		ttl:     ttl,
		entries: make(map[string]time.Time),
		nowFunc: time.Now,
	}
}

// ShouldProcess returns true if key has not been seen within the TTL window,
// and records the key with a fresh expiration timestamp.
func (d *Deduplicator) ShouldProcess(key string) bool {
	now := d.nowFunc()

	d.mu.Lock()
	defer d.mu.Unlock()

	if len(d.entries) > 2048 {
		d.pruneLocked(now)
	}

	if exp, exists := d.entries[key]; exists && now.Before(exp) {
		return false
	}

	d.entries[key] = now.Add(d.ttl)
	return true
}

func (d *Deduplicator) pruneLocked(now time.Time) {
	for k, exp := range d.entries {
		if !now.Before(exp) {
			delete(d.entries, k)
		}
	}
}
