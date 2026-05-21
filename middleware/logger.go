package middleware

import (
	"encoding/json"
	"log/slog"
	"sync"
)

// LogEvent is published after every proxied request completes.
type LogEvent struct {
	TS             int64  `json:"ts"`
	ClientKey      string `json:"key_id"`
	RequestedModel string `json:"model"`
	RoutedModel    string `json:"routed_model"`
	Provider       string `json:"provider"`
	InputTokens    int    `json:"input_tokens"`
	OutputTokens   int    `json:"output_tokens"`
	LatencyMS      int64  `json:"latency_ms"`
	Status         int    `json:"status"`
	FallbackCount  int    `json:"fallback_count"`
}

// Broadcaster is an in-process pub/sub for LogEvents.
// Subscribers receive events on buffered channels.
type Broadcaster struct {
	mu   sync.Mutex
	subs map[chan []byte]struct{}
}

// NewBroadcaster returns an initialised Broadcaster.
func NewBroadcaster() *Broadcaster {
	return &Broadcaster{subs: make(map[chan []byte]struct{})}
}

// Subscribe returns a buffered channel that receives JSON-encoded LogEvents.
// Call Unsubscribe when the consumer is done.
func (b *Broadcaster) Subscribe() chan []byte {
	ch := make(chan []byte, 64)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

// Unsubscribe removes the channel from the subscriber list and closes it.
func (b *Broadcaster) Unsubscribe(ch chan []byte) {
	b.mu.Lock()
	delete(b.subs, ch)
	b.mu.Unlock()
	close(ch)
}

// Publish sends a LogEvent to all current subscribers.
// Slow subscribers are skipped (non-blocking send) to avoid blocking the router.
func (b *Broadcaster) Publish(ev LogEvent) {
	data, err := json.Marshal(ev)
	if err != nil {
		slog.Error("broadcaster: marshal event", "err", err)
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- data:
		default:
			// Subscriber too slow — drop event rather than block.
		}
	}
}
