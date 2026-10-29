// Package session runs conversations: it takes messages in, runs turns one
// at a time, records every event, and streams events to watchers.
package session

import (
	"context"
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
	"github.com/viminizer/germes/internal/stream"
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

// Elsewhere reports whether another process still runs a session: an old
// daemon finishing its turns after an update.
type Elsewhere interface {
	IsBusy(ctx context.Context, sessionID string) bool
}

// Options configure the manager.
type Options struct {
	Worktrees string    // where session worktrees go; "" = no worktrees
	Refs      Expander  // nil = @ references stay plain text
	Elsewhere Elsewhere // nil = only this process runs sessions
}

// NewManager creates a manager.
func NewManager(store Store, runner Runner, opts Options) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{store: store, runner: runner, ctx: ctx, cancel: cancel, opts: opts,
		workers: map[string]*worker{}, hub: hub{watchers: map[*watcher]struct{}{}}}
}

// Start marks turns that were running when the daemon stopped as
// interrupted. Turns an old daemon still runs are left alone.
func (m *Manager) Start(ctx context.Context) error {
	page, err := m.store.ListSessions(ctx, api.SessionFilter{Status: api.StatusWorking, Limit: 500})
	if err != nil {
		return err
	}
	for _, s := range page.Items {
		if m.opts.Elsewhere != nil && m.opts.Elsewhere.IsBusy(ctx, s.ID) {
			continue
		}
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
	return stream.Wait(ctx, &m.wg)
}

// ListBusy lists the sessions with a turn running or messages waiting.
func (m *Manager) ListBusy() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []string
	for id, w := range m.workers {
		if w.isBusy() {
			ids = append(ids, id)
		}
	}
	return ids
}

// SendEvent passes a live event from another process (an old daemon after
// an update) on to this manager's watchers. That process saved it already.
func (m *Manager) SendEvent(ev api.Event) { m.hub.publish(ev) }

// SendMessage queues text for a session. No SessionID: a new session is
// created in r.Dir.
func (m *Manager) SendMessage(ctx context.Context, r api.MessageRequest) (api.MessageResult, error) {
	text := strings.TrimSpace(r.Text)
	if text == "" {
		return api.MessageResult{}, api.NewError(api.CodeInvalid, "empty message")
	}
	id, warning := r.SessionID, ""
	if id == "" {
		s, warn, err := m.createSession(ctx, r, text)
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
	// A session whose worktree holds work is never deleted with it. A
	// worktree other sessions still use (a job's rounds) stays.
	if s.Branch != "" && !m.isShared(ctx, s) {
		if err := m.deleteWorktree(ctx, s); err != nil {
			return err
		}
	}
	return m.store.DeleteSession(ctx, id)
}

// isShared reports whether another session works in s.Dir. On a read error
// it says yes, so the worktree is kept.
func (m *Manager) isShared(ctx context.Context, s api.Session) bool {
	page, err := m.store.ListSessions(ctx, api.SessionFilter{Project: s.Project, Limit: 500})
	if err != nil {
		return true
	}
	for _, o := range page.Items {
		if o.ID != s.ID && o.Dir == s.Dir {
			return true
		}
	}
	return false
}

// ListEvents lists stored events.
func (m *Manager) ListEvents(ctx context.Context, f api.EventFilter) (api.Page[api.Event], error) {
	return m.store.ListEvents(ctx, f)
}

// createSession saves a new session. In a git project it gets its own
// worktree, so sessions never clash; if that fails it works in r.Dir, with a
// warning for the user. With r.SameDirAs it works where that session works.
func (m *Manager) createSession(ctx context.Context, r api.MessageRequest, text string) (api.Session, string, error) {
	now := time.Now()
	if r.SameDirAs != "" {
		src, err := m.store.GetSession(ctx, r.SameDirAs)
		if err != nil {
			return api.Session{}, "", err
		}
		if _, err := os.Stat(src.Dir); err != nil {
			return api.Session{}, "", api.NewError(api.CodeInvalid, "folder %q of session %s is gone", src.Dir, src.ID)
		}
		s := api.Session{ID: api.NewID(), Title: titleOf(text), Project: src.Project, Dir: src.Dir, Branch: src.Branch,
			Status: api.StatusIdle, ResumeIDs: map[string]string{}, CreatedAt: now, UpdatedAt: now}
		return s, "", m.store.CreateSession(ctx, s)
	}
	dir, noWorktree := r.Dir, r.NoWorktree
	if dir == "" {
		return api.Session{}, "", api.NewError(api.CodeInvalid, "no session id and no folder")
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return api.Session{}, "", api.NewError(api.CodeInvalid, "folder %q does not exist", dir)
	}
	s := api.Session{ID: api.NewID(), Title: titleOf(text), Project: GetProjectRoot(dir), Dir: dir,
		Status: api.StatusIdle, ResumeIDs: map[string]string{}, CreatedAt: now, UpdatedAt: now}
	warning := ""
	switch {
	case m.opts.Worktrees != "" && !noWorktree && worktree.IsRepo(ctx, s.Project):
		wt, err := worktree.Create(ctx, m.opts.Worktrees, s.Project, s.ID)
		if err != nil {
			slog.Warn("create worktree", "session", s.ID, "err", err)
			reason := "git failed; details in the daemon log"
			if errors.Is(err, worktree.ErrNoCommits) {
				reason = err.Error()
			}
			warning = "no worktree for this session (" + reason + "), so it works in the project folder"
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

func (m *Manager) newWorker(ctx context.Context, s api.Session) (*worker, error) {
	seq, err := m.store.GetLastSeq(ctx, s.ID)
	if err != nil {
		return nil, fmt.Errorf("load session %s: %w", s.ID, err)
	}
	return &worker{m: m, sess: s, seq: seq, wake: make(chan struct{}, 1)}, nil
}
