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

// Expander expands @ references in a message into the text they point to,
// and loads a skill the user called by name.
type Expander interface {
	Expand(ctx context.Context, msg, sessionID, dir string) string
	LoadSkill(ctx context.Context, name, sessionID, dir string) (out string, isError bool)
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
	// Checkpoints is where folders are snapshot before each turn, for
	// rollback; "" = no snapshots.
	Checkpoints string
	Titles      Titler // nil = a session's title stays its first message cut short
}

// NewManager creates a manager.
func NewManager(store Store, runner Runner, opts Options) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{store: store, runner: runner, ctx: ctx, cancel: cancel, opts: opts,
		workers: map[string]*worker{}, hub: hub{watchers: map[*watcher]struct{}{}}}
}

// Start marks turns that no process runs any more as interrupted: turns cut
// when the daemon stopped, or when an old daemon died finishing them, and
// turns paused for an engine limit (the wait lived only in memory, issue
// #27). Turns this manager or an old daemon runs are left alone. Safe to
// call again.
func (m *Manager) Start(ctx context.Context) error {
	if err := m.createOverseer(ctx); err != nil {
		return err
	}
	var cut []api.Session
	for _, status := range []api.Status{api.StatusWorking, api.StatusPaused} {
		page, err := m.store.ListSessions(ctx, api.SessionFilter{Status: status, Limit: 500})
		if err != nil {
			return err
		}
		cut = append(cut, page.Items...)
	}
	for _, s := range cut {
		m.mu.Lock()
		_, running := m.workers[s.ID]
		m.mu.Unlock()
		if running || (m.opts.Elsewhere != nil && m.opts.Elsewhere.IsBusy(ctx, s.ID)) {
			continue
		}
		w, err := m.newWorker(ctx, s)
		if err != nil {
			slog.Error("recover session", "session", s.ID, "err", err)
			continue
		}
		w.emit(api.Event{Kind: api.KindError, Text: api.TextInterrupted})
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

// createOverseer creates the overseer's session in the home folder, once.
func (m *Manager) createOverseer(ctx context.Context) error {
	_, err := m.store.GetSession(ctx, api.OverseerID)
	if !api.IsNotFound(err) {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	now := time.Now()
	return m.store.CreateSession(ctx, api.Session{ID: api.OverseerID, Title: "overseer", Project: home, Dir: home,
		Status: api.StatusIdle, ResumeIDs: map[string]string{}, CreatedAt: now, UpdatedAt: now})
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
	if err := w.enqueue(text, r.Plan, r.Skill); err != nil {
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

// UpdateSession renames a session, or moves it into or out of its own git
// worktree (from the next turn).
func (m *Manager) UpdateSession(ctx context.Context, u api.SessionUpdate) error {
	title := strings.TrimSpace(u.Title)
	if title == "" && !u.LeaveWorktree && !u.EnterWorktree {
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
		if err := m.leaveWorktree(ctx, &s); err != nil {
			return err
		}
	}
	if u.EnterWorktree {
		if s.Branch != "" {
			return api.NewError(api.CodeInvalid, "session %s already works in its worktree %s", s.ID, s.Dir)
		}
		if err := m.createWorktree(ctx, &s); err != nil {
			return api.NewError(api.CodeInvalid, "no worktree: %v", err)
		}
	}
	s.UpdatedAt = time.Now()
	return m.store.UpdateSession(ctx, s)
}

// leaveWorktree removes s's worktree and moves s back to the project folder.
// A job's rounds share one worktree: all of them leave it, or none, so no
// session is left in a deleted folder. The caller saves s.
func (m *Manager) leaveWorktree(ctx context.Context, s *api.Session) error {
	if s.Branch == "" {
		return api.NewError(api.CodeInvalid, "session %s is not in a worktree", s.ID)
	}
	others, err := m.listSharing(ctx, *s)
	if err != nil {
		return err
	}
	for _, o := range others {
		if m.isBusy(ctx, o.ID) {
			return api.NewError(api.CodeInvalid, "session %s still works in worktree %s: wait until its turn ends", o.ID, s.Dir)
		}
	}
	if err := m.deleteWorktree(ctx, *s); err != nil {
		return err
	}
	s.Dir, s.Branch = s.Project, ""
	for _, o := range others {
		o.Dir, o.Branch, o.UpdatedAt = s.Project, "", time.Now()
		if err := m.store.UpdateSession(ctx, o); err != nil {
			return err
		}
	}
	return nil
}

// deleteWorktree removes a session's worktree, or explains why it must stay.
func (m *Manager) deleteWorktree(ctx context.Context, s api.Session) error {
	err := worktree.Delete(ctx, s.Project, worktree.Worktree{Dir: s.Dir, Branch: s.Branch})
	if errors.Is(err, worktree.ErrHasWork) {
		return api.NewError(api.CodeInvalid, "worktree %s (branch %s) has uncommitted changes or commits not merged into the project: merge or discard them first", s.Dir, s.Branch)
	}
	return err
}

// DeleteSession stops and deletes a session. The overseer stays.
func (m *Manager) DeleteSession(ctx context.Context, id string) error {
	if id == api.OverseerID {
		return api.NewError(api.CodeInvalid, "the overseer cannot be deleted")
	}
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
	// On a read error the worktree is kept.
	if s.Branch != "" {
		if others, err := m.listSharing(ctx, s); err == nil && len(others) == 0 {
			if err := m.deleteWorktree(ctx, s); err != nil {
				return err
			}
		}
	}
	return m.store.DeleteSession(ctx, id)
}

// listSharing lists the other sessions that work in s.Dir (a job's rounds).
// ponytail: the project's newest 500 sessions; page through them if a job
// ever runs more rounds than that.
func (m *Manager) listSharing(ctx context.Context, s api.Session) ([]api.Session, error) {
	page, err := m.store.ListSessions(ctx, api.SessionFilter{Project: s.Project, Limit: 500})
	if err != nil {
		return nil, err
	}
	var others []api.Session
	for _, o := range page.Items {
		if o.ID != s.ID && o.Dir == s.Dir {
			others = append(others, o)
		}
	}
	return others, nil
}

// isBusy reports whether a session runs a turn or has messages waiting,
// here or in an old daemon that still finishes it.
func (m *Manager) isBusy(ctx context.Context, id string) bool {
	m.mu.Lock()
	w, ok := m.workers[id]
	m.mu.Unlock()
	return ok && w.isBusy() || m.opts.Elsewhere != nil && m.opts.Elsewhere.IsBusy(ctx, id)
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
		s := api.Session{ID: api.NewID(), Title: firstTitle(r, text), Project: src.Project, Dir: src.Dir, Branch: src.Branch,
			Status: api.StatusIdle, ResumeIDs: map[string]string{}, CreatedAt: now, UpdatedAt: now}
		return s, "", m.store.CreateSession(ctx, s)
	}
	dir := r.Dir
	if dir == "" {
		return api.Session{}, "", api.NewError(api.CodeInvalid, "no session id and no folder")
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return api.Session{}, "", api.NewError(api.CodeInvalid, "folder %q does not exist", dir)
	}
	s := api.Session{ID: api.NewID(), Title: firstTitle(r, text), Project: GetProjectRoot(dir), Dir: dir,
		Status: api.StatusIdle, ResumeIDs: map[string]string{}, CreatedAt: now, UpdatedAt: now}
	warning := ""
	if r.Worktree {
		if err := m.createWorktree(ctx, &s); err != nil {
			warning = "no worktree for this session (" + err.Error() + "), so it works in the project folder"
		}
	}
	if warning == "" && s.Branch == "" && m.isOthersFolder(ctx, s) {
		warning = "another session is working in this folder, so changes may clash (start with --worktree to avoid it)"
	}
	if err := m.store.CreateSession(ctx, s); err != nil {
		return api.Session{}, "", err
	}
	return s, warning, nil
}

// isOthersFolder reports whether another session is working in s.Dir. A
// read error says no: it only skips a warning.
func (m *Manager) isOthersFolder(ctx context.Context, s api.Session) bool {
	busy, err := m.store.ListSessions(ctx, api.SessionFilter{Project: s.Project, Status: api.StatusWorking, Limit: 50})
	if err != nil {
		return false
	}
	for _, o := range busy.Items {
		if o.Dir == s.Dir {
			return true
		}
	}
	return false
}

// createWorktree gives the session its own git worktree and branch. The
// error says why not, in words for the user (details go to the log).
func (m *Manager) createWorktree(ctx context.Context, s *api.Session) error {
	if m.opts.Worktrees == "" || !worktree.IsRepo(ctx, s.Project) {
		return errors.New("the folder is not a git repo")
	}
	wt, err := worktree.Create(ctx, m.opts.Worktrees, s.Project, s.ID)
	if errors.Is(err, worktree.ErrNoCommits) {
		return err
	}
	if err != nil {
		slog.Warn("create worktree", "session", s.ID, "err", err)
		return errors.New("git failed; details in the daemon log")
	}
	s.Dir, s.Branch = wt.Dir, wt.Branch
	return nil
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
