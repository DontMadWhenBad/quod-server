package server

import (
	"math"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Limits bounds what one client can do. Zero values take defaults.
type Limits struct {
	// UnauthPerMin and UnauthBurst limit requests without a valid key, per
	// client IP. Once exhausted, credentials are not even checked.
	UnauthPerMin float64
	UnauthBurst  int
	// KeyPerMin and KeyBurst limit requests per principal.
	KeyPerMin float64
	KeyBurst  int
	// MaxStreamsPerKey caps concurrently open event streams per principal.
	MaxStreamsPerKey int
	// MaxEntries caps each limiter table; past it, unknown clients share
	// one bucket so memory stays bounded.
	MaxEntries int
}

func (l Limits) withDefaults() Limits {
	if l.UnauthPerMin <= 0 {
		l.UnauthPerMin = 20
	}
	if l.UnauthBurst <= 0 {
		l.UnauthBurst = 10
	}
	if l.KeyPerMin <= 0 {
		l.KeyPerMin = 120
	}
	if l.KeyBurst <= 0 {
		l.KeyBurst = 30
	}
	if l.MaxStreamsPerKey <= 0 {
		l.MaxStreamsPerKey = 8
	}
	if l.MaxEntries <= 0 {
		l.MaxEntries = 10000
	}
	return l
}

// limiterTable holds one token bucket per key, evicting idle ones.
type limiterTable struct {
	mu        sync.Mutex
	limit     rate.Limit
	burst     int
	max       int
	m         map[string]*bucket
	overflow  *rate.Limiter // shared once the table is full
	lastSweep time.Time
	now       func() time.Time
}

type bucket struct {
	lim  *rate.Limiter
	last time.Time
}

const idleAfter = 10 * time.Minute

func newLimiterTable(perMin float64, burst, max int) *limiterTable {
	lim := rate.Limit(perMin / 60)
	return &limiterTable{
		limit: lim, burst: burst, max: max,
		m:        map[string]*bucket{},
		overflow: rate.NewLimiter(lim, burst),
		now:      time.Now,
	}
}

func (t *limiterTable) get(key string, now time.Time) *rate.Limiter {
	if now.Sub(t.lastSweep) > time.Minute {
		for k, b := range t.m {
			if now.Sub(b.last) > idleAfter {
				delete(t.m, k)
			}
		}
		t.lastSweep = now
	}
	b, ok := t.m[key]
	if !ok {
		if len(t.m) >= t.max {
			return t.overflow
		}
		b = &bucket{lim: rate.NewLimiter(t.limit, t.burst)}
		t.m[key] = b
	}
	b.last = now
	return b.lim
}

// allow consumes a token for key; if none is available it returns the
// wait until one is.
func (t *limiterTable) allow(key string) (bool, time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	lim := t.get(key, now)
	if lim.AllowN(now, 1) {
		return true, 0
	}
	return false, waitFor(lim, now)
}

// exhausted reports (without consuming) whether key has no token left.
func (t *limiterTable) exhausted(key string) (bool, time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	lim := t.get(key, now)
	if lim.TokensAt(now) >= 1 {
		return false, 0
	}
	return true, waitFor(lim, now)
}

func waitFor(lim *rate.Limiter, now time.Time) time.Duration {
	missing := 1 - lim.TokensAt(now)
	if lim.Limit() <= 0 {
		return time.Minute
	}
	return time.Duration(math.Ceil(missing/float64(lim.Limit())*1000)) * time.Millisecond
}

// size is the number of tracked keys (tests).
func (t *limiterTable) size() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.m)
}

// counter caps concurrent uses per key (open event streams).
type counter struct {
	mu  sync.Mutex
	max int
	n   map[string]int
}

func newCounter(max int) *counter { return &counter{max: max, n: map[string]int{}} }

func (c *counter) acquire(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n[key] >= c.max {
		return false
	}
	c.n[key]++
	return true
}

func (c *counter) release(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n[key]--; c.n[key] <= 0 {
		delete(c.n, key)
	}
}
