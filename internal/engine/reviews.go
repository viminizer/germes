package engine

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
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
	ctx    context.Context
	home   string                       // Germes' home: memories/ and skills/ are here
	notify func(sessionID, text string) // tells the session what a review saved
	wg     sync.WaitGroup
	one    sync.Mutex // one review at a time, so each sees only its own saves
	mu     sync.Mutex
	n      map[string]int
	queued map[string]bool // sessions with a review waiting for its turn
}

// NewReviews creates Reviews whose reviews stop when ctx ends. After a
// review that changed memory or skills under home, notify gets one line
// saying what (issue #18); nil notify says nothing.
func NewReviews(ctx context.Context, home string, notify func(sessionID, text string)) *Reviews {
	return &Reviews{ctx: ctx, home: home, notify: notify, n: map[string]int{}, queued: map[string]bool{}}
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

// Run runs review in the background, after any review still running: two
// at once would each count the other's saves as their own. A session has
// at most one review waiting; another one is skipped, and the waiting one
// covers the same ground. Its ctx ends a few minutes after it starts, or
// when the Reviews' context ends. A panic is logged.
func (r *Reviews) Run(sessionID string, review func(ctx context.Context)) {
	r.mu.Lock()
	if r.queued[sessionID] {
		r.mu.Unlock()
		slog.Info("review skipped: one for this session is waiting", "session", sessionID)
		return
	}
	r.queued[sessionID] = true
	r.mu.Unlock()
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer func() {
			if p := recover(); p != nil {
				slog.Error("review panicked", "session", sessionID, "panic", p, "stack", string(debug.Stack()))
			}
		}()
		r.one.Lock()
		defer r.one.Unlock()
		r.mu.Lock()
		delete(r.queued, sessionID) // running now: a later one may wait
		r.mu.Unlock()
		if r.ctx.Err() != nil {
			return // the daemon stopped while this review waited
		}
		ctx, cancel := context.WithTimeout(r.ctx, reviewTimeout)
		defer cancel()
		before := r.learned()
		review(ctx)
		if saved := changed(before, r.learned()); len(saved) > 0 && r.notify != nil {
			r.notify(sessionID, "saved by the review: "+strings.Join(saved, ", "))
		}
	}()
}

// learned is the size and time of every memory and skill file. Both engines
// save through files (claude edits them, the Germes loop's tools write them),
// so comparing before and after a review shows what it saved.
// ponytail: a turn that saves memory while a review runs counts as this
// review's too; tell them apart if that ever confuses.
func (r *Reviews) learned() map[string]string {
	files := map[string]string{}
	if r.home == "" {
		return files
	}
	for _, dir := range []string{"memories", "skills"} {
		// A folder or file that cannot be read is left out: it was not
		// saved, so there is nothing to tell.
		filepath.WalkDir(filepath.Join(r.home, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if info, err := d.Info(); err == nil {
				rel, _ := filepath.Rel(r.home, path)
				files[rel] = fmt.Sprintf("%d %d", info.ModTime().UnixNano(), info.Size())
			}
			return nil
		})
	}
	return files
}

// changed names what differs between two learned snapshots: "memory (agent
// notes)", "memory (about you)", "skill <name>".
func changed(before, after map[string]string) []string {
	var names []string
	add := func(rel string) {
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) > 2 && parts[1] == ".archive" {
			parts = append(parts[:1], parts[2:]...) // the curator moved it there
		}
		name := "skill " + parts[min(1, len(parts)-1)]
		switch {
		case parts[0] == "memories" && parts[len(parts)-1] == "USER.md":
			name = "memory (about you)"
		case parts[0] == "memories":
			name = "memory (agent notes)"
		}
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	for rel, v := range after {
		if before[rel] != v {
			add(rel)
		}
	}
	for rel := range before {
		if _, ok := after[rel]; !ok {
			add(rel) // deleted (or moved to the archive)
		}
	}
	slices.Sort(names)
	return names
}

// Wait blocks until the running reviews end.
func (r *Reviews) Wait() { r.wg.Wait() }
