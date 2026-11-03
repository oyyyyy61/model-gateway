// Package limits implements the gateway's local admission control: a token
// bucket for requests-per-minute and a daily token budget per API key.
//
// State is process local. That is enough for a single replica and keeps the
// gateway dependency free; a Redis backed implementation can replace Limiter
// behind the same methods once the gateway runs with multiple replicas. The
// MySQL audit table already records the same usage fields.
package limits

import (
	"sync"
	"time"
)

// Limiter tracks per-key request buckets and daily token usage.
type Limiter struct {
	mu      sync.Mutex
	now     func() time.Time
	buckets map[string]*bucket
	usage   map[string]*tokenCounter
}

type bucket struct {
	tokens float64
	last   time.Time
}

type tokenCounter struct {
	day    string
	tokens int64
}

// New returns an empty limiter using the wall clock.
func New() *Limiter {
	return &Limiter{
		now:     time.Now,
		buckets: make(map[string]*bucket),
		usage:   make(map[string]*tokenCounter),
	}
}

// SetClock replaces the clock. It is only useful for deterministic tests.
func (l *Limiter) SetClock(now func() time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.now = now
}

// AllowRequest consumes one token from the key's per-minute bucket.
//
// rpm <= 0 disables request limiting. The returned duration is the wait until
// one token becomes available and is meant for the Retry-After header.
func (l *Limiter) AllowRequest(key string, rpm int) (bool, time.Duration) {
	if rpm <= 0 {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	current, ok := l.buckets[key]
	if !ok {
		current = &bucket{tokens: float64(rpm), last: now}
		l.buckets[key] = current
	}

	if elapsed := now.Sub(current.last).Seconds(); elapsed > 0 {
		rate := float64(rpm) / 60.0
		current.tokens += elapsed * rate
		if current.tokens > float64(rpm) {
			current.tokens = float64(rpm)
		}
		current.last = now
	}

	if current.tokens >= 1 {
		current.tokens--
		return true, 0
	}

	rate := float64(rpm) / 60.0
	wait := time.Duration((1-current.tokens)/rate*float64(time.Second)) + time.Millisecond
	return false, wait
}

// AddTokens records usage against the key's current UTC day.
func (l *Limiter) AddTokens(key string, tokens int64) {
	if tokens <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	day := l.now().UTC().Format("2006-01-02")
	counter, ok := l.usage[key]
	if !ok || counter.day != day {
		counter = &tokenCounter{day: day}
		l.usage[key] = counter
	}
	counter.tokens += tokens
}

// TokensUsed returns the token usage recorded for the current UTC day.
func (l *Limiter) TokensUsed(key string) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	day := l.now().UTC().Format("2006-01-02")
	counter, ok := l.usage[key]
	if !ok || counter.day != day {
		return 0
	}
	return counter.tokens
}

// QuotaExceeded reports whether the key already reached its daily token
// budget. daily <= 0 disables the quota.
func (l *Limiter) QuotaExceeded(key string, daily int64) (bool, int64) {
	used := l.TokensUsed(key)
	if daily <= 0 {
		return false, used
	}
	return used >= daily, used
}
