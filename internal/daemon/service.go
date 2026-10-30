package daemon

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/viminizer/germes/internal/api"
)

// GetDaemon describes this daemon.
func (d *Daemon) GetDaemon(ctx context.Context) (api.Daemon, error) {
	return api.Daemon{Version: d.opts.Version, PID: os.Getpid(), StartedAt: d.startedAt, Busy: d.sessions.ListBusy()}, nil
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

// DeleteSession deletes a session, after stopping its turn in an old daemon.
func (d *Daemon) DeleteSession(ctx context.Context, id string) error {
	d.old.stopSession(ctx, api.StopRequest{ID: id})
	return d.sessions.DeleteSession(ctx, id)
}

// SendMessage sends text to a session.
func (d *Daemon) SendMessage(ctx context.Context, r api.MessageRequest) (api.MessageResult, error) {
	return d.sessions.SendMessage(ctx, r)
}

// StopSession stops the current step of a session, here or in an old daemon
// that still runs it.
func (d *Daemon) StopSession(ctx context.Context, r api.StopRequest) error {
	d.old.stopSession(ctx, r)
	return d.sessions.StopSession(ctx, r)
}

// ListEvents lists stored events.
func (d *Daemon) ListEvents(ctx context.Context, f api.EventFilter) (api.Page[api.Event], error) {
	return d.sessions.ListEvents(ctx, f)
}

// Watch streams events.
func (d *Daemon) Watch(ctx context.Context, f api.WatchFilter) (<-chan api.Event, error) {
	return d.sessions.Watch(ctx, f)
}

// CreateJob creates and starts a job.
func (d *Daemon) CreateJob(ctx context.Context, r api.JobRequest) (api.Job, error) {
	return d.jobs.CreateJob(ctx, r)
}

// ListJobs lists jobs.
func (d *Daemon) ListJobs(ctx context.Context, f api.JobFilter) (api.Page[api.Job], error) {
	return d.jobs.ListJobs(ctx, f)
}

// GetJob reads one job by id or name.
func (d *Daemon) GetJob(ctx context.Context, idOrName string) (api.Job, error) {
	return d.jobs.GetJob(ctx, idOrName)
}

// StartJob starts a stopped job again.
func (d *Daemon) StartJob(ctx context.Context, id string) error {
	return d.jobs.StartJob(ctx, id)
}

// StopJob stops a job and its running round.
func (d *Daemon) StopJob(ctx context.Context, id string) error {
	return d.jobs.StopJob(ctx, id)
}

// DeleteJob stops and deletes a job.
func (d *Daemon) DeleteJob(ctx context.Context, id string) error {
	return d.jobs.DeleteJob(ctx, id)
}

// ListTools lists the Germes tools a CLI engine gets for a session through
// MCP; the overseer gets the session and job tools too.
func (d *Daemon) ListTools(ctx context.Context, sessionID string) ([]api.ToolInfo, error) {
	if _, err := d.sessions.GetSession(ctx, sessionID); err != nil {
		return nil, err
	}
	if sessionID == api.OverseerID {
		return d.opts.Tools.ListTools(ToolsetMCPOverseer), nil
	}
	return d.opts.Tools.ListTools(ToolsetMCP), nil
}

// GetInsights says how Germes was used since a time.
func (d *Daemon) GetInsights(ctx context.Context, since time.Time) (api.Insights, error) {
	if d.opts.Insights == nil {
		return api.Insights{}, errors.New("no insights in this daemon")
	}
	return d.opts.Insights(ctx, since)
}

// RunTool runs one tool in a session's folder (or r.Dir).
func (d *Daemon) RunTool(ctx context.Context, r api.ToolRequest) (api.ToolResult, error) {
	return d.opts.Tools.RunTool(ctx, r)
}

// CheckTool says whether a tool call may run (Claude's own tools ask it
// through `germes hook check`).
func (d *Daemon) CheckTool(ctx context.Context, r api.ToolRequest) (api.ToolVerdict, error) {
	return d.opts.Tools.CheckTool(ctx, r)
}

// ListCheckpoints lists the snapshots of a session's folder.
func (d *Daemon) ListCheckpoints(ctx context.Context, sessionID string) ([]api.Checkpoint, error) {
	return d.sessions.ListCheckpoints(ctx, sessionID)
}

// RestoreCheckpoint puts a session's folder back as it was in a snapshot.
func (d *Daemon) RestoreCheckpoint(ctx context.Context, sessionID, id string) error {
	return d.sessions.RestoreCheckpoint(ctx, sessionID, id)
}
