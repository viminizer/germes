// Package session runs conversations: it takes messages in, runs turns one
// at a time, records every event, and streams events to watchers.
package session

import (
	"context"
	"crypto/rand"
	"fmt"
	textpkg "github.com/viminizer/germes/internal/text"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/viminizer/germes/internal/api"
	"github.com/viminizer/germes/internal/engine"
)

// idleTime is how long a worker stays in memory with nothing to do.
const idleTime = 5 * time.Minute

// Store is what the manager needs from the database.
type Store interface {
	CreateSession(ctx context.Context, s api.Session) error
	GetSession(ctx context.Context, id string) (api.Session, error)
	UpdateSession(ctx context.Context, s api.Session) error
	DeleteSession(ctx context.Context, id string) error
	ListSessions(ctx context.Context, f api.SessionFilter) (api.Page[api.Session], error)
	CreateEvent(ctx context.Context, e api.Event) error
	ListEvents(ctx context.Context, f api.EventFilter) (api.Page[api.Event], error)
	GetLastSeq(ctx context.Context, sessionID string) (int64, error)
}

// Runner runs one turn. The router (or one engine) satisfies it.
type Runner interface {
	Run(ctx context.Context, t engine.Turn) (<-chan engine.Event, error)
}

// Manager owns the active sessions.
type Manager struct {
	store  Store
	runner Runner
	ctx    context.Context // cancelled by Stop
	cancel context.CancelFunc

	mu      sync.Mutex
	workers map[string]*worker
	saveMu  sync.Mutex // one read-modify-write of a session row at a time
	hub     hub
	wg      sync.WaitGroup
}

// NewManager creates a manager.
func NewManager(store Store, runner Runner) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{store: store, runner: runner, ctx: ctx, cancel: cancel,
		workers: map[string]*worker{}, hub: hub{watchers: map[*watcher]struct{}{}}}
}

// Start marks turns that were running when the daemon stopped as interrupted.
func (m *Manager) Start(ctx context.Context) error {
	page, err := m.store.ListSessions(ctx, api.SessionFilter{Status: api.StatusWorking, Limit: 500})
	if err != nil {
		return err
	}
	for _, s := range page.Items {
		w, err := m.newWorker(ctx, s)
		if err != nil {
			slog.Error("recover session", "session", s.ID, "err", err)
			continue
		}
		w.emit(api.Event{Kind: api.KindError, Text: "interrupted: the daemon stopped during this turn"})
		w.setStatus(api.StatusIdle)
		w.emit(api.Event{Kind: api.KindTurnDone})
	}
	return nil
}

// Stop cancels every running turn and waits for the workers to finish.
func (m *Manager) Stop(ctx context.Context) error {
	m.cancel()
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SendMessage queues text for a session. No SessionID: a new session is
// created in r.Dir.
func (m *Manager) SendMessage(ctx context.Context, r api.MessageRequest) (api.MessageResult, error) {
	text := strings.TrimSpace(r.Text)
	if text == "" {
		return api.MessageResult{}, api.NewError(api.CodeInvalid, "empty message")
	}
	id := r.SessionID
	if id == "" {
		s, err := m.createSession(ctx, r.Dir, text)
		if err != nil {
			return api.MessageResult{}, err
		}
		id = s.ID
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.workers[id]
	if !ok {
		s, err := m.store.GetSession(ctx, id)
		if err != nil {
			return api.MessageResult{}, err
		}
		if w, err = m.newWorker(ctx, s); err != nil {
			return api.MessageResult{}, err
		}
		m.workers[id] = w
		m.wg.Add(1)
		go w.run()
	}
	if err := w.enqueue(text); err != nil {
		return api.MessageResult{}, err
	}
	return api.MessageResult{SessionID: id}, nil
}

// StopSession stops the current step of a session. The queue is dropped (Esc)
// or, with SendQueue, runs right away as the next turn (Esc Esc).
func (m *Manager) StopSession(ctx context.Context, r api.StopRequest) error {
	m.mu.Lock()
	w, ok := m.workers[r.ID]
	m.mu.Unlock()
	if ok {
		w.stopTurn(!r.SendQueue)
	}
	return nil
}

// ListSessions lists sessions.
func (m *Manager) ListSessions(ctx context.Context, f api.SessionFilter) (api.Page[api.Session], error) {
	return m.store.ListSessions(ctx, f)
}

// GetSession reads one session, with the messages waiting in its queue.
func (m *Manager) GetSession(ctx context.Context, id string) (api.Session, error) {
	s, err := m.store.GetSession(ctx, id)
	if err != nil {
		return s, err
	}
	m.mu.Lock()
	w, ok := m.workers[id]
	m.mu.Unlock()
	if ok {
		s.Queued = w.queued()
	}
	return s, nil
}

// UpdateSession renames a session.
func (m *Manager) UpdateSession(ctx context.Context, u api.SessionUpdate) error {
	title := strings.TrimSpace(u.Title)
	if title == "" {
		return api.NewError(api.CodeInvalid, "empty title")
	}
	m.saveMu.Lock()
	defer m.saveMu.Unlock()
	s, err := m.store.GetSession(ctx, u.ID)
	if err != nil {
		return err
	}
	s.Title = title
	s.UpdatedAt = time.Now()
	return m.store.UpdateSession(ctx, s)
}

// DeleteSession stops and deletes a session.
func (m *Manager) DeleteSession(ctx context.Context, id string) error {
	m.mu.Lock()
	w, ok := m.workers[id]
	m.mu.Unlock()
	if ok {
		w.stopTurn(true)
	}
	return m.store.DeleteSession(ctx, id)
}

// ListEvents lists stored events.
func (m *Manager) ListEvents(ctx context.Context, f api.EventFilter) (api.Page[api.Event], error) {
	return m.store.ListEvents(ctx, f)
}

func (m *Manager) createSession(ctx context.Context, dir, text string) (api.Session, error) {
	if dir == "" {
		return api.Session{}, api.NewError(api.CodeInvalid, "no session id and no folder")
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return api.Session{}, api.NewError(api.CodeInvalid, "folder %q does not exist", dir)
	}
	now := time.Now()
	s := api.Session{ID: newID(), Title: titleOf(text), Project: GetProjectRoot(dir), Dir: dir,
		Status: api.StatusIdle, ResumeIDs: map[string]string{}, CreatedAt: now, UpdatedAt: now}
	if err := m.store.CreateSession(ctx, s); err != nil {
		return api.Session{}, err
	}
	return s, nil
}

// GetProjectRoot walks up from dir to the folder holding .git. No git: dir.
func GetProjectRoot(dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		if filepath.Dir(d) == d {
			return dir
		}
	}
}

// titleOf is the first line of the first message, cut to 40 characters.
func titleOf(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	return textpkg.Shorten(line, 40)
}

func newID() string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 6)
	rand.Read(b)
	for i := range b {
		b[i] = letters[int(b[i])%len(letters)]
	}
	return string(b)
}

func (m *Manager) newWorker(ctx context.Context, s api.Session) (*worker, error) {
	seq, err := m.store.GetLastSeq(ctx, s.ID)
	if err != nil {
		return nil, fmt.Errorf("load session %s: %w", s.ID, err)
	}
	return &worker{m: m, sess: s, seq: seq, wake: make(chan struct{}, 1)}, nil
}
