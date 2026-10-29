// Package session runs conversations: it takes messages in, runs turns one
// at a time, records every event, and streams events to watchers.
package session

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	textpkg "github.com/viminizer/germes/internal/text"
	"github.com/viminizer/germes/internal/worktree"
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

	opts Options
}

// Expander expands @ references in a message into the text they point to.
type Expander interface {
	Expand(ctx context.Context, msg, sessionID, dir string) string
}

// Options configure the manager.
type Options struct {
	Worktrees string   // where session worktrees go; "" = no worktrees
	Refs      Expander // nil = @ references stay plain text
}

// NewManager creates a manager.
func NewManager(store Store, runner Runner, opts Options) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{store: store, runner: runner, ctx: ctx, cancel: cancel, opts: opts,
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
	id, warning := r.SessionID, ""
	if id == "" {
		s, warn, err := m.createSession(ctx, r.Dir, text, r.NoWorktree)
		if err != nil {
			return api.MessageResult{}, err
		}
		id, warning = s.ID, warn
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
	return api.MessageResult{SessionID: id, Warning: warning}, nil
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
	if title == "" && !u.LeaveWorktree {
		return api.NewError(api.CodeInvalid, "nothing to change")
	}
	m.saveMu.Lock()
	defer m.saveMu.Unlock()
	s, err := m.store.GetSession(ctx, u.ID)
	if err != nil {
		return err
	}
	if title != "" {
		s.Title = title
	}
	if u.LeaveWorktree {
		if s.Branch == "" {
			return api.NewError(api.CodeInvalid, "session %s is not in a worktree", s.ID)
		}
		if err := m.deleteWorktree(ctx, s); err != nil {
			return err
		}
		s.Dir, s.Branch = s.Project, ""
	}
	s.UpdatedAt = time.Now()
	return m.store.UpdateSession(ctx, s)
}

// deleteWorktree removes a session's worktree, or explains why it must stay.
func (m *Manager) deleteWorktree(ctx context.Context, s api.Session) error {
	err := worktree.Delete(ctx, s.Project, worktree.Worktree{Dir: s.Dir, Branch: s.Branch})
	if errors.Is(err, worktree.ErrHasWork) {
		return api.NewError(api.CodeInvalid, "worktree %s (branch %s) has uncommitted changes or commits not merged into the project: merge or discard them first", s.Dir, s.Branch)
	}
	return err
}

// DeleteSession stops and deletes a session.
func (m *Manager) DeleteSession(ctx context.Context, id string) error {
	m.mu.Lock()
	w, ok := m.workers[id]
	m.mu.Unlock()
	if ok {
		w.stopTurn(true)
	}
	s, err := m.store.GetSession(ctx, id)
	if err != nil {
		return err
	}
	// A session whose worktree holds work is never deleted with it.
	if s.Branch != "" {
		if err := m.deleteWorktree(ctx, s); err != nil {
			return err
		}
	}
	return m.store.DeleteSession(ctx, id)
}

// ListEvents lists stored events.
func (m *Manager) ListEvents(ctx context.Context, f api.EventFilter) (api.Page[api.Event], error) {
	return m.store.ListEvents(ctx, f)
}

// createSession saves a new session. In a git project it gets its own
// worktree, so sessions never clash; if that fails it works in dir, with a
// warning for the user.
func (m *Manager) createSession(ctx context.Context, dir, text string, noWorktree bool) (api.Session, string, error) {
	if dir == "" {
		return api.Session{}, "", api.NewError(api.CodeInvalid, "no session id and no folder")
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return api.Session{}, "", api.NewError(api.CodeInvalid, "folder %q does not exist", dir)
	}
	now := time.Now()
	s := api.Session{ID: newID(), Title: titleOf(text), Project: GetProjectRoot(dir), Dir: dir,
		Status: api.StatusIdle, ResumeIDs: map[string]string{}, CreatedAt: now, UpdatedAt: now}
	warning := ""
	switch {
	case m.opts.Worktrees != "" && !noWorktree && worktree.IsRepo(ctx, s.Project):
		wt, err := worktree.Create(ctx, m.opts.Worktrees, s.Project, s.ID)
		if err != nil {
			slog.Warn("create worktree", "session", s.ID, "err", err)
			warning = "no worktree for this session (" + gitReason(err) + "), so it works in the project folder"
		} else {
			s.Dir, s.Branch = wt.Dir, wt.Branch
		}
	case !worktree.IsRepo(ctx, s.Project):
		busy, err := m.store.ListSessions(ctx, api.SessionFilter{Project: s.Project, Status: api.StatusWorking, Limit: 1})
		if err == nil && len(busy.Items) > 0 {
			warning = "another session is working in this folder and it is not a git repo, so changes may clash"
		}
	}
	if err := m.store.CreateSession(ctx, s); err != nil {
		return api.Session{}, "", err
	}
	return s, warning, nil
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

// gitReason is the short part of a git error worth showing a user.
func gitReason(err error) string {
	msg := err.Error()
	if strings.Contains(msg, "invalid reference: HEAD") {
		return "the project has no commits yet"
	}
	if i := strings.LastIndex(msg, "fatal: "); i >= 0 {
		msg = msg[i+len("fatal: "):]
	}
	return strings.TrimSuffix(strings.TrimSpace(strings.Split(msg, ": exit status")[0]), ".")
}
