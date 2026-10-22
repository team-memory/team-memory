// Package ratelimit is an in-memory token-bucket limiter keyed by an
// arbitrary string (a principal ID, a client IP). Limits are per process:
// with N replicas the effective limit is up to N times higher, which is
// documented in docs/security/limits.md.
package ratelimit

import (
	"math"
	"sync"
	"time"
)

// Limiter holds one bucket per key.
type Limiter struct {
	rate  float64 // tokens per second
	burst float64
	now   func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
	swept   time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// New returns a limiter allowing rate events per second with the given
// burst. A rate <= 0 disables limiting.
func New(rate float64, burst int) *Limiter {
	if burst < 1 {
		burst = 1
	}
	return &Limiter{rate: rate, burst: float64(burst), now: time.Now, buckets: map[string]*bucket{}}
}

// WithClock replaces the clock (tests).
func (l *Limiter) WithClock(now func() time.Time) *Limiter {
	l.now = now
	return l
}

// Allow consumes one token for key. When none is available it returns false
// and how long until one is.
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	if l == nil || l.rate <= 0 {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweep(now)
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = math.Min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
	return false, wait
}

// sweep drops buckets that have refilled completely, bounding memory by the
// number of keys active within one refill period.
func (l *Limiter) sweep(now time.Time) {
	if now.Sub(l.swept) < time.Minute {
		return
	}
	l.swept = now
	full := time.Duration(l.burst / l.rate * float64(time.Second))
	for k, b := range l.buckets {
		if now.Sub(b.last) > full {
			delete(l.buckets, k)
		}
	}
}
