package session

import (
	"context"
	"log/slog"
	"runtime/debug"
	"time"
)

// Titler names a session from a message: the router, which asks the engine
// the turn runs on (Hermes: agent/title_generator.py).
type Titler interface {
	CreateTitle(ctx context.Context, message string) (string, error)
}

const (
	// titleTimeout bounds the call that names a session.
	titleTimeout = time.Minute
	// greetingTitle is what a bare greeting is named: a placeholder, so
	// the next message names the session again, like Hermes.
	greetingTitle = "Friendly greeting"
)

// nameSession gives a session a short title a model writes, in the
// background, when its title is still the first message cut short or the
// greeting placeholder (Hermes' two-step title). A title changed meanwhile,
// like a rename, is kept.
func (w *worker) nameSession(message string) {
	m, id, current := w.m, w.sess.ID, w.sess.Title
	if m.opts.Titles == nil || (current != titleOf(message) && current != greetingTitle) {
		return
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("naming the session panicked", "session", id, "panic", r, "stack", string(debug.Stack()))
			}
		}()
		ctx, cancel := context.WithTimeout(m.ctx, titleTimeout)
		defer cancel()
		title, err := m.opts.Titles.CreateTitle(ctx, message)
		if err != nil {
			slog.Info("session keeps its title", "session", id, "err", err)
			return
		}
		// A title that came is saved even when the daemon is stopping.
		ctx = context.Background()
		m.saveMu.Lock()
		defer m.saveMu.Unlock()
		s, err := m.store.GetSession(ctx, id)
		if err != nil || s.Title != current {
			return // gone, or renamed meanwhile
		}
		s.Title = title
		if err := m.store.UpdateSession(ctx, s); err != nil {
			slog.Error("save session title", "session", id, "err", err)
		}
	}()
}
