package engine

import (
	"context"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"
)

const (
	reviewEvery   = 10 // tool calls without saving memory or a skill that make a background review due
	reviewTimeout = 5 * time.Minute
)

// Reviews learns from sessions in the background (Hermes' learning loop):
// it counts each session's tool calls since it last saved memory or a skill,
// says when a review is due, and runs the reviews until its context ends.
// Both engines share one, so a session keeps its count when it changes engine.
type Reviews struct {
	ctx context.Context
	wg  sync.WaitGroup
	mu  sync.Mutex
	n   map[string]int
}

// NewReviews creates Reviews whose reviews stop when ctx ends.
func NewReviews(ctx context.Context) *Reviews {
	return &Reviews{ctx: ctx, n: map[string]int{}}
}

// Update records one tool call; saved means it saved memory or a skill.
func (r *Reviews) Update(sessionID string, saved bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if saved {
		delete(r.n, sessionID)
		return
	}
	r.n[sessionID]++
}

// IsDue reports whether a review is due, and starts counting again if so.
func (r *Reviews) IsDue(sessionID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.n[sessionID] < reviewEvery {
		return false
	}
	delete(r.n, sessionID)
	return true
}

// Run runs review in the background. Its ctx ends after a few minutes, or
// when the Reviews' context ends. A panic is logged.
func (r *Reviews) Run(sessionID string, review func(ctx context.Context)) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer func() {
			if p := recover(); p != nil {
				slog.Error("review panicked", "session", sessionID, "panic", p, "stack", string(debug.Stack()))
			}
		}()
		ctx, cancel := context.WithTimeout(r.ctx, reviewTimeout)
		defer cancel()
		review(ctx)
	}()
}

// Wait blocks until the running reviews end.
func (r *Reviews) Wait() { r.wg.Wait() }
