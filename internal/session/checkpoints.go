package session

import (
	"context"
	"log/slog"

	"github.com/viminizer/germes/internal/api"
	"github.com/viminizer/germes/internal/checkpoint"
	"github.com/viminizer/germes/internal/worktree"
)

// maxCheckpoints is how many snapshots a listing shows.
const maxCheckpoints = 20

// snapshot saves the session's folder before a turn, when it is a git
// project (not the overseer's home). A failure is logged: the turn runs on.
func (w *worker) snapshot(ctx context.Context, text string) {
	store := w.m.opts.Checkpoints
	if store == "" || w.sess.ID == api.OverseerID || !worktree.IsRepo(ctx, w.sess.Dir) {
		return
	}
	if _, err := checkpoint.Create(ctx, store, w.sess.Dir, "before: "+titleOf(text)); err != nil {
		slog.Warn("snapshot before turn", "session", w.sess.ID, "err", err)
	}
}

// ListCheckpoints lists the snapshots of a session's folder, newest first.
func (m *Manager) ListCheckpoints(ctx context.Context, sessionID string) ([]api.Checkpoint, error) {
	s, err := m.store.GetSession(ctx, sessionID)
	if err != nil || m.opts.Checkpoints == "" {
		return nil, err
	}
	list, err := checkpoint.List(ctx, m.opts.Checkpoints, s.Dir, maxCheckpoints)
	if err != nil {
		return nil, err
	}
	out := make([]api.Checkpoint, len(list))
	for i, c := range list {
		out[i] = api.Checkpoint{ID: c.ID, Label: c.Label, Time: c.Time}
	}
	return out, nil
}

// RestoreCheckpoint puts a session's folder back as it was in one of its
// snapshots. Refused while the session works: the turn would fight it.
func (m *Manager) RestoreCheckpoint(ctx context.Context, sessionID, id string) error {
	m.mu.Lock()
	w, ok := m.workers[sessionID]
	busy := ok && w.isBusy()
	m.mu.Unlock()
	if busy {
		return api.NewError(api.CodeInvalid, "session %s is working; wait until its turn ends (or stop it with Esc Esc)", sessionID)
	}
	list, err := m.ListCheckpoints(ctx, sessionID)
	if err != nil {
		return err
	}
	for _, c := range list {
		if c.ID == id {
			s, err := m.store.GetSession(ctx, sessionID)
			if err != nil {
				return err
			}
			return checkpoint.Restore(ctx, m.opts.Checkpoints, s.Dir, id)
		}
	}
	return api.NewError(api.CodeNotFound, "no snapshot %q for session %s", id, sessionID)
}
