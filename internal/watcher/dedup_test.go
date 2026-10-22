package watcher

import (
	"testing"
	"time"
)

func TestDeduplicatorSuppressesWithinTTLandExpires(t *testing.T) {
	now := time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)
	dedup := NewDeduplicator(2 * time.Minute)
	dedup.nowFunc = func() time.Time { return now }

	key := "prod:Deployment:uid-1:Scaled:2:scale:2->4"

	if !dedup.ShouldProcess(key) {
		t.Fatal("expected first occurrence of key to be processed")
	}
	if dedup.ShouldProcess(key) {
		t.Fatal("expected duplicate occurrence within TTL to be suppressed")
	}

	// Advance time by 1 minute (still within 2m TTL).
	now = now.Add(1 * time.Minute)
	if dedup.ShouldProcess(key) {
		t.Fatal("expected key at +1m to still be suppressed")
	}

	// Advance time past 2m TTL.
	now = now.Add(61 * time.Second)
	if !dedup.ShouldProcess(key) {
		t.Fatal("expected key after TTL expiration to be processed again")
	}
}
