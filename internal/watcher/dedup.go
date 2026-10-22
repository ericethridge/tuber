package watcher

import (
	"sync"
	"time"
)

const (
	// DefaultMaxDedupEntries bounds the in-memory deduplication map size to prevent OOM under event storms.
	DefaultMaxDedupEntries = 10000
	pruneThreshold         = 2048
)

// Deduplicator suppresses duplicate WorkloadEvents within a configurable TTL window
// with a hard upper bound on in-memory entries.
type Deduplicator struct {
	mu         sync.Mutex
	ttl        time.Duration
	maxEntries int
	entries    map[string]time.Time
	nowFunc    func() time.Time
}

// NewDeduplicator creates a TTL-based event deduplicator with DefaultMaxDedupEntries capacity.
func NewDeduplicator(ttl time.Duration) *Deduplicator {
	return NewDeduplicatorWithMax(ttl, DefaultMaxDedupEntries)
}

// NewDeduplicatorWithMax creates a TTL-based event deduplicator with an explicit maximum entry capacity.
func NewDeduplicatorWithMax(ttl time.Duration, maxEntries int) *Deduplicator {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	if maxEntries <= 0 {
		maxEntries = DefaultMaxDedupEntries
	}
	return &Deduplicator{
		ttl:        ttl,
		maxEntries: maxEntries,
		entries:    make(map[string]time.Time),
		nowFunc:    time.Now,
	}
}

// ShouldProcess returns true if key has not been seen within the TTL window,
// and records the key with a fresh expiration timestamp.
func (d *Deduplicator) ShouldProcess(key string) bool {
	now := d.nowFunc()

	d.mu.Lock()
	defer d.mu.Unlock()

	if exp, exists := d.entries[key]; exists {
		if now.Before(exp) {
			return false
		}
		delete(d.entries, key)
	}

	if len(d.entries) >= pruneThreshold || len(d.entries) >= d.maxEntries {
		d.pruneLocked(now)
	}
	for len(d.entries) >= d.maxEntries {
		d.evictOldestLocked()
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

func (d *Deduplicator) evictOldestLocked() {
	var oldestKey string
	var oldestExp time.Time
	first := true
	for k, exp := range d.entries {
		if first || exp.Before(oldestExp) {
			oldestKey = k
			oldestExp = exp
			first = false
		}
	}
	if !first {
		delete(d.entries, oldestKey)
	}
}
