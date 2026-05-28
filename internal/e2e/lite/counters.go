package lite

import "sync"

// Counters tracks the numeric series the scenario expectations assert on.
// Methods are safe under concurrent access — the agent loop is single-
// threaded today, but the lite-proxy may emit audit rows from another
// goroutine in future.
type Counters struct {
	mu   sync.Mutex
	data map[string]int
}

func NewCounters() *Counters {
	return &Counters{data: map[string]int{}}
}

func (c *Counters) Inc(series string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data[series]++
}

func (c *Counters) Get(series string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.data[series]
}

func (c *Counters) Snapshot() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int, len(c.data))
	for k, v := range c.data {
		out[k] = v
	}
	return out
}
