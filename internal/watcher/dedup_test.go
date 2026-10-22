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

func TestDeduplicatorEnforcesMaxCapacityAndEvictsOldest(t *testing.T) {
	now := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	dedup := NewDeduplicatorWithMax(5*time.Minute, 3)
	dedup.nowFunc = func() time.Time { return now }

	for _, k := range []string{"k1", "k2", "k3"} {
		if !dedup.ShouldProcess(k) {
			t.Fatalf("expected %s to be processed", k)
		}
		now = now.Add(1 * time.Second)
	}

	// Adding k4 when capacity is 3 and nothing is expired must evict the oldest ("k1").
	if !dedup.ShouldProcess("k4") {
		t.Fatal("expected k4 to be processed")
	}
	if len(dedup.entries) != 3 {
		t.Fatalf("len(entries) = %d, want capped at 3", len(dedup.entries))
	}
	if dedup.ShouldProcess("k2") || dedup.ShouldProcess("k3") || dedup.ShouldProcess("k4") {
		t.Fatal("expected k2, k3, k4 to remain in cache")
	}
	if !dedup.ShouldProcess("k1") {
		t.Fatal("expected oldest entry k1 to have been evicted when k4 was inserted")
	}
}
