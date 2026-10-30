package engine

import (
	"sync"
	"time"
)

const (
	reviewEvery = 10 // tool calls without saving memory or a skill that make a background review due
	// ReviewTimeout is how long a background review may run.
	ReviewTimeout = 5 * time.Minute
)

// Steps counts each session's tool calls since it last saved memory or a
// skill, so an engine knows when to review the session in the background
// and learn from it (Hermes' learning loop). The zero value is ready to use.
type Steps struct {
	mu sync.Mutex
	n  map[string]int
}

// Update records one tool call; saved means it saved memory or a skill.
func (s *Steps) Update(sessionID string, saved bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.n == nil {
		s.n = map[string]int{}
	}
	if saved {
		delete(s.n, sessionID)
		return
	}
	s.n[sessionID]++
}

// IsDue reports whether a review is due, and starts counting again if so.
func (s *Steps) IsDue(sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.n[sessionID] < reviewEvery {
		return false
	}
	delete(s.n, sessionID)
	return true
}
