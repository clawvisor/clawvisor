package handlers

import (
	"sync"
	"testing"
	"time"
)

func TestDedupCache_PutAndGet(t *testing.T) {
	c := newDedupCache(5 * time.Second)
	defer c.Stop()

	key := buildDedupKey("agent1", "svc", "act", map[string]any{"k": "v"})
	c.Put(key, "cached-value")

	got, ok := c.Get(key)
	if !ok {
		t.Fatal("expected cache hit")
	}
	if got != "cached-value" {
		t.Errorf("got %v, want %q", got, "cached-value")
	}
}

func TestDedupCache_Miss(t *testing.T) {
	c := newDedupCache(5 * time.Second)
	defer c.Stop()

	key := buildDedupKey("agent1", "svc", "act")
	if _, ok := c.Get(key); ok {
		t.Fatal("expected cache miss")
	}
}

func TestDedupCache_Expiry(t *testing.T) {
	c := newDedupCache(10 * time.Millisecond)
	defer c.Stop()

	key := buildDedupKey("agent1", "svc", "act")
	c.Put(key, "value")

	time.Sleep(20 * time.Millisecond)

	if _, ok := c.Get(key); ok {
		t.Fatal("expected cache miss after expiry")
	}
}

func TestDedupCache_DifferentKeys(t *testing.T) {
	c := newDedupCache(5 * time.Second)
	defer c.Stop()

	k1 := buildDedupKey("agent1", "svc", "act", map[string]any{"k": "v1"})
	k2 := buildDedupKey("agent1", "svc", "act", map[string]any{"k": "v2"})
	k3 := buildDedupKey("agent2", "svc", "act", map[string]any{"k": "v1"})

	c.Put(k1, "r1")

	if _, ok := c.Get(k2); ok {
		t.Error("different params should not match")
	}
	if _, ok := c.Get(k3); ok {
		t.Error("different agent should not match")
	}
}

func TestDedupCache_Cleanup(t *testing.T) {
	c := newDedupCache(10 * time.Millisecond)
	defer c.Stop()

	key := buildDedupKey("agent1", "svc", "act")
	c.Put(key, "value")

	time.Sleep(20 * time.Millisecond)
	c.cleanup()

	c.mu.Lock()
	count := len(c.entries)
	c.mu.Unlock()
	if count != 0 {
		t.Errorf("expected 0 entries after cleanup, got %d", count)
	}
}

func TestDedupCache_ThreadSafety(t *testing.T) {
	c := newDedupCache(5 * time.Second)
	defer c.Stop()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := buildDedupKey("agent1", "svc", "act")
			c.Put(key, "value")
			c.Get(key)
		}()
	}
	wg.Wait()
}
