// Package events is an in-process activity feed: recent node events and
// request records for the dashboard and the status API.
package events

import (
	"sync"
	"time"
)

// Level of an event.
const (
	Info  = "info"
	Warn  = "warn"
	Error = "error"
)

// Event is a notable node occurrence (session opened, key created, ...).
type Event struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"`
	Kind    string    `json:"kind"`
	Message string    `json:"message"`
}

// Request summarizes one routed inference request.
type Request struct {
	Time         time.Time     `json:"time"`
	Key          string        `json:"key"`
	Model        string        `json:"model"`
	Kind         string        `json:"kind"`
	Session      string        `json:"session,omitempty"`
	Provider     string        `json:"provider,omitempty"`
	Status       int           `json:"status"`
	Latency      time.Duration `json:"latencyMs"`
	TTFT         time.Duration `json:"ttftMs"`
	InputTokens  int           `json:"inputTokens"`
	OutputTokens int           `json:"outputTokens"`
	Stream       bool          `json:"stream"`
	Error        string        `json:"error,omitempty"`
}

// Bus fans events out to subscribers and keeps short histories.
type Bus struct {
	mu       sync.Mutex
	subs     map[int]chan any
	next     int
	events   ring[Event]
	requests ring[Request]
	totals   Totals
}

// Totals are lifetime counters since the node started.
type Totals struct {
	Requests     int64 `json:"requests"`
	Errors       int64 `json:"errors"`
	InputTokens  int64 `json:"inputTokens"`
	OutputTokens int64 `json:"outputTokens"`
}

func New() *Bus {
	return &Bus{subs: map[int]chan any{}, events: newRing[Event](200), requests: newRing[Request](200)}
}

// Emit records and broadcasts an event.
func (b *Bus) Emit(level, kind, msg string) {
	e := Event{Time: time.Now(), Level: level, Kind: kind, Message: msg}
	b.mu.Lock()
	b.events.push(e)
	b.broadcast(e)
	b.mu.Unlock()
}

// Record stores a finished request.
func (b *Bus) Record(r Request) {
	if r.Time.IsZero() {
		r.Time = time.Now()
	}
	b.mu.Lock()
	b.requests.push(r)
	b.totals.Requests++
	if r.Status >= 400 || r.Error != "" {
		b.totals.Errors++
	}
	b.totals.InputTokens += int64(r.InputTokens)
	b.totals.OutputTokens += int64(r.OutputTokens)
	b.broadcast(r)
	b.mu.Unlock()
}

func (b *Bus) broadcast(v any) {
	for _, ch := range b.subs {
		select {
		case ch <- v:
		default: // slow subscriber: drop rather than block the node
		}
	}
}

// Subscribe returns a channel of Event and Request values.
func (b *Bus) Subscribe() (<-chan any, func()) {
	ch := make(chan any, 256)
	b.mu.Lock()
	id := b.next
	b.next++
	b.subs[id] = ch
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, id)
		b.mu.Unlock()
	}
}

// RecentEvents returns up to n events, newest last.
func (b *Bus) RecentEvents(n int) []Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.events.last(n)
}

// RecentRequests returns up to n requests, newest last.
func (b *Bus) RecentRequests(n int) []Request {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.requests.last(n)
}

// Totals returns lifetime counters.
func (b *Bus) Totals() Totals {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.totals
}

type ring[T any] struct {
	buf  []T
	head int
	full bool
}

func newRing[T any](n int) ring[T] { return ring[T]{buf: make([]T, n)} }

func (r *ring[T]) push(v T) {
	r.buf[r.head] = v
	r.head = (r.head + 1) % len(r.buf)
	if r.head == 0 {
		r.full = true
	}
}

func (r *ring[T]) last(n int) []T {
	size := r.head
	if r.full {
		size = len(r.buf)
	}
	if n > size || n <= 0 {
		n = size
	}
	out := make([]T, 0, n)
	for i := size - n; i < size; i++ {
		idx := i
		if r.full {
			idx = (r.head + i) % len(r.buf)
		}
		out = append(out, r.buf[idx])
	}
	return out
}
