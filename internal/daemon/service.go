package daemon

import (
	"context"
	"os"

	"github.com/viminizer/germes/internal/api"
)

// GetDaemon describes this daemon.
func (d *Daemon) GetDaemon(ctx context.Context) (api.Daemon, error) {
	return api.Daemon{Version: d.version, PID: os.Getpid(), StartedAt: d.startedAt}, nil
}

// StopDaemon asks Run to shut down.
func (d *Daemon) StopDaemon(ctx context.Context) error {
	d.stopOnce.Do(func() { close(d.stop) })
	return nil
}

// ListSessions lists sessions.
func (d *Daemon) ListSessions(ctx context.Context, f api.SessionFilter) (api.Page[api.Session], error) {
	return d.sessions.ListSessions(ctx, f)
}

// GetSession reads one session.
func (d *Daemon) GetSession(ctx context.Context, id string) (api.Session, error) {
	return d.sessions.GetSession(ctx, id)
}

// UpdateSession renames a session.
func (d *Daemon) UpdateSession(ctx context.Context, u api.SessionUpdate) error {
	return d.sessions.UpdateSession(ctx, u)
}

// DeleteSession deletes a session.
func (d *Daemon) DeleteSession(ctx context.Context, id string) error {
	return d.sessions.DeleteSession(ctx, id)
}

// SendMessage sends text to a session.
func (d *Daemon) SendMessage(ctx context.Context, r api.MessageRequest) (api.MessageResult, error) {
	return d.sessions.SendMessage(ctx, r)
}

// StopSession stops the current step of a session.
func (d *Daemon) StopSession(ctx context.Context, id string) error {
	return d.sessions.StopSession(ctx, id)
}

// ListEvents lists stored events.
func (d *Daemon) ListEvents(ctx context.Context, f api.EventFilter) (api.Page[api.Event], error) {
	return d.sessions.ListEvents(ctx, f)
}

// Watch streams events.
func (d *Daemon) Watch(ctx context.Context, f api.WatchFilter) (<-chan api.Event, error) {
	return d.sessions.Watch(ctx, f)
}
