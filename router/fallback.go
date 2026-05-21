package router

import (
	"errors"
	"sync"
	"time"
)

// defaultCooldown is used when the upstream sends no Retry-After header.
// 10 s is enough to absorb a burst without burning too much time — real NIM
// windows are typically 1–10 s for the free tier.
const defaultCooldown = 10 * time.Second

// keyState tracks per-key 429 cooldowns and the per-provider round-robin cursor.
type keyState struct {
	mu       sync.Mutex
	ratedAt  map[string]time.Time     // key value → time of last 429
	cooldown map[string]time.Duration // key value → cooldown for that 429 event
	nextIdx  map[string]int           // provider_id → index of next key to try first
}

func newKeyState() *keyState {
	return &keyState{
		ratedAt:  make(map[string]time.Time),
		cooldown: make(map[string]time.Duration),
		nextIdx:  make(map[string]int),
	}
}

func (ks *keyState) isRateLimited(key string) bool {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	t, ok := ks.ratedAt[key]
	if !ok {
		return false
	}
	d := ks.cooldown[key]
	if d <= 0 {
		d = defaultCooldown
	}
	return time.Since(t) < d
}

// markRateLimited records a 429 for key.  retryAfter is the duration from the
// upstream Retry-After header; pass 0 to use the defaultCooldown.
func (ks *keyState) markRateLimited(key string, retryAfter time.Duration) {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	ks.ratedAt[key] = time.Now()
	if retryAfter > 0 {
		ks.cooldown[key] = retryAfter
	} else {
		ks.cooldown[key] = defaultCooldown
	}
}

// startIdx returns the round-robin start index for providerID, bounded to [0, n).
func (ks *keyState) startIdx(providerID string, n int) int {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if n <= 0 {
		return 0
	}
	return ks.nextIdx[providerID] % n
}

// advanceIdx records that usedIdx was the key we just used, so the next request
// for this provider starts one position past it.
func (ks *keyState) advanceIdx(providerID string, usedIdx, n int) {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if n > 0 {
		ks.nextIdx[providerID] = (usedIdx + 1) % n
	}
}

// FallbackResult holds the outcome of a successful provider attempt.
type FallbackResult struct {
	RoutedModel   string
	Provider      string
	InputTokens   int
	OutputTokens  int
	FallbackCount int
}

// ErrAllExhausted is returned when every provider and model fallback failed.
var ErrAllExhausted = errors.New("all providers exhausted")
