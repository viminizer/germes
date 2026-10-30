package session

import (
	"context"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"
	"unicode"

	"github.com/viminizer/germes/internal/api"
	textpkg "github.com/viminizer/germes/internal/text"
)

// Titler names a session from a message: the router, which asks the engine
// the turn runs on (Hermes: agent/title_generator.py).
type Titler interface {
	CreateTitle(ctx context.Context, message string) (string, error)
}

const (
	// titleTimeout bounds the call that names a session.
	titleTimeout = time.Minute
	// maxTitle is the longest title a client may set (MessageRequest.Title).
	maxTitle = 80
	// maxNamings is how many messages may name a session, like Hermes: a
	// chat that stays small talk stops asking. Counted while the session's
	// worker runs: one woken up again (idle, daemon restart) may ask again.
	maxNamings = 3
)

// firstTitle is a new session's title: the one asked for (a job round),
// else the message cut short until a model names it.
func firstTitle(r api.MessageRequest, text string) string {
	if t := strings.Join(strings.Fields(r.Title), " "); t != "" { // one line
		return textpkg.Shorten(t, maxTitle)
	}
	return titleOf(text)
}

// isGreeting says whether a title is what the title prompt gives a bare
// greeting ("Friendly greeting", also "in chat", any case): a placeholder
// the next message replaces, like Hermes.
func isGreeting(title string) bool {
	t := strings.ToLower(strings.TrimFunc(title, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }))
	return t == "friendly greeting" || t == "friendly greeting in chat"
}

// nameSession gives a session a short title a model writes, in the
// background, when its title is still the first message cut short or the
// greeting placeholder, for its first maxNamings messages (Hermes'
// two-step title). A title changed meanwhile, like a rename, is kept.
func (w *worker) nameSession(message string) {
	m, id, current := w.m, w.sess.ID, w.sess.Title
	if m.opts.Titles == nil || (current != titleOf(message) && !isGreeting(current)) || w.namings >= maxNamings {
		return
	}
	w.namings++
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
			slog.Warn("session keeps its title", "session", id, "err", err)
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
