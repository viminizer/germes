// Package job runs jobs: a prompt that runs again and again, each round in a
// new session, until the agent says it is done, the job is stopped, or its
// rounds keep failing.
package job

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/viminizer/germes/internal/api"
	"github.com/viminizer/germes/internal/stream"
)

// maxFailures is how many rounds in a row may fail before the job does.
const maxFailures = 3

// doneMark ends the agent's reply when nothing is left to do.
const doneMark = "[DONE]"

// Store is what the scheduler needs from the database.
type Store interface {
	CreateJob(ctx context.Context, j api.Job) error
	GetJob(ctx context.Context, idOrName string) (api.Job, error)
	UpdateJob(ctx context.Context, j api.Job) error
	DeleteJob(ctx context.Context, id string) error
	ListJobs(ctx context.Context, f api.JobFilter) (api.Page[api.Job], error)
}

// Sessions runs the rounds. The session manager satisfies it.
type Sessions interface {
	SendMessage(ctx context.Context, r api.MessageRequest) (api.MessageResult, error)
	GetSession(ctx context.Context, id string) (api.Session, error)
	UpdateSession(ctx context.Context, u api.SessionUpdate) error
	StopSession(ctx context.Context, r api.StopRequest) error
	ListEvents(ctx context.Context, f api.EventFilter) (api.Page[api.Event], error)
	Watch(ctx context.Context, f api.WatchFilter) (<-chan api.Event, error)
	SendEvent(ev api.Event)
}

// Tools runs a workflow's commands, like an agent's tool call (the block
// list applies), in the session's folder or r.Dir.
type Tools interface {
	RunTool(ctx context.Context, r api.ToolRequest) (api.ToolResult, error)
}

// Options configure the scheduler.
type Options struct {
	Tools     Tools
	Workflows string // the user's workflow folder; a job's <dir>/.germes/workflows comes first
}

// Scheduler runs every running job in its own goroutine.
type Scheduler struct {
	store    Store
	sessions Sessions
	opts     Options
	wg       sync.WaitGroup

	mu      sync.Mutex // one read-modify-write of a job row at a time; guards the fields below
	running map[string]*runner
	ctx     context.Context // set by Start, cancelled by Stop
	cancel  context.CancelFunc
}

// runner is one job's goroutine.
type runner struct{ cancel context.CancelFunc }

// round is how one round ended.
type round struct {
	done    bool   // the agent replied [DONE], or the workflow reached done (a repeating job: nothing to do now)
	failure string // why it failed; "" = it worked
	fatal   bool   // the job cannot run any more (its folder or workflow is gone)
	blocked string // what blocks the work (a workflow that stopped making progress); the job waits
}

// NewScheduler creates a scheduler.
func NewScheduler(store Store, sessions Sessions, opts Options) *Scheduler {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // nothing runs before Start
	return &Scheduler{store: store, sessions: sessions, opts: opts, ctx: ctx, cancel: cancel, running: map[string]*runner{}}
}

// ListWorkflowDirs lists the folders a job in dir loads workflows from, first
// match wins: the project's .germes/workflows, then the user's folder.
func ListWorkflowDirs(userDir, dir string) []string {
	return []string{filepath.Join(dir, ".germes", "workflows"), userDir}
}

// Start resumes the jobs that were running when the daemon stopped. A round
// missed while the daemon was off runs once, right away. A round an old
// daemon still runs (after an update) is waited for first. Start may follow
// Stop.
func (s *Scheduler) Start(ctx context.Context) error {
	s.mu.Lock()
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.mu.Unlock()
	page, err := s.store.ListJobs(ctx, api.JobFilter{Status: api.JobRunning, Limit: 500})
	if err != nil {
		return err
	}
	for _, j := range page.Items {
		s.launch(j.ID)
	}
	return nil
}

// Stop ends every job goroutine and waits for them. The jobs stay running
// in the database, so the next start resumes them. Rounds already running
// in sessions go on.
func (s *Scheduler) Stop(ctx context.Context) error {
	s.mu.Lock()
	s.cancel()
	s.running = map[string]*runner{}
	s.mu.Unlock()
	return stream.Wait(ctx, &s.wg)
}

// CreateJob saves a job and starts its first round.
func (s *Scheduler) CreateJob(ctx context.Context, r api.JobRequest) (api.Job, error) {
	prompt := strings.TrimSpace(r.Prompt)
	switch {
	case prompt == "" && r.Workflow == "":
		return api.Job{}, api.NewError(api.CodeInvalid, "empty prompt")
	case r.Every < 0 || r.MaxRuns < 0:
		return api.Job{}, api.NewError(api.CodeInvalid, "every and max runs must not be negative")
	}
	if fi, err := os.Stat(r.Dir); err != nil || !fi.IsDir() {
		return api.Job{}, api.NewError(api.CodeInvalid, "folder %q does not exist", r.Dir)
	}
	if r.Workflow != "" {
		if _, err := LoadWorkflow(ListWorkflowDirs(s.opts.Workflows, r.Dir), r.Workflow); err != nil {
			return api.Job{}, api.NewError(api.CodeInvalid, "workflow %s: %v", r.Workflow, err)
		}
	}
	now := time.Now()
	j := api.Job{ID: api.NewID(), Name: strings.TrimSpace(r.Name), Dir: r.Dir, Prompt: prompt, NoWorktree: r.NoWorktree,
		Repeat: r.Repeat, Every: r.Every, MaxRuns: r.MaxRuns, NextRunAt: now, Status: api.JobRunning, Workflow: r.Workflow, CreatedAt: now, UpdatedAt: now}
	if j.Name == "" {
		j.Name = j.ID
	}
	if err := s.store.CreateJob(ctx, j); err != nil {
		return api.Job{}, err
	}
	s.launch(j.ID)
	return j, nil
}

// ListJobs lists jobs.
func (s *Scheduler) ListJobs(ctx context.Context, f api.JobFilter) (api.Page[api.Job], error) {
	return s.store.ListJobs(ctx, f)
}

// GetJob reads one job by id or name.
func (s *Scheduler) GetJob(ctx context.Context, idOrName string) (api.Job, error) {
	return s.store.GetJob(ctx, idOrName)
}

// StartJob runs a stopped, done or failed job again. A missed round runs
// right away, and so does the next round of a job waiting for work.
func (s *Scheduler) StartJob(ctx context.Context, id string) error {
	j, err := s.update(ctx, id, func(j *api.Job) error {
		if j.MaxRuns > 0 && j.Runs >= j.MaxRuns {
			return api.NewError(api.CodeInvalid, "job %s already ran all %d rounds", j.Name, j.MaxRuns)
		}
		j.Status, j.Failures, j.Error = api.JobRunning, 0, ""
		if j.Idle > 0 {
			j.Idle, j.NextRunAt = 0, time.Now()
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.launch(j.ID)
	return nil
}

// StopJob stops a job and its running round.
func (s *Scheduler) StopJob(ctx context.Context, id string) error {
	j, err := s.update(ctx, id, func(j *api.Job) error {
		if j.Status == api.JobRunning {
			j.Status = api.JobStopped
		}
		j.Finished = j.Runs // the stopped round is dropped, never read later
		return nil
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	if r, ok := s.running[j.ID]; ok {
		r.cancel()
		delete(s.running, j.ID)
	}
	s.mu.Unlock()
	if j.SessionID != "" {
		return s.sessions.StopSession(ctx, api.StopRequest{ID: j.SessionID})
	}
	return nil
}

// DeleteJob stops and deletes a job. Its round sessions stay.
func (s *Scheduler) DeleteJob(ctx context.Context, id string) error {
	j, err := s.store.GetJob(ctx, id)
	if err != nil {
		return err
	}
	if err := s.StopJob(ctx, j.ID); err != nil {
		return err
	}
	return s.store.DeleteJob(ctx, j.ID)
}

// update changes a job row under the lock. change may refuse with an error.
// A job that stops running (done, failed, stopped) sends a notice.
func (s *Scheduler) update(ctx context.Context, id string, change func(*api.Job) error) (api.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, err := s.store.GetJob(ctx, id)
	if err != nil {
		return j, err
	}
	before := j.Status
	if err := change(&j); err != nil {
		return j, err
	}
	j.UpdatedAt = time.Now()
	if err := s.store.UpdateJob(ctx, j); err != nil {
		return j, err
	}
	if before == api.JobRunning && j.Status != api.JobRunning {
		text := "job " + j.Name + " is " + string(j.Status)
		if j.Status == api.JobFailed {
			text += ": " + j.Error
		}
		s.notify(j.SessionID, text)
	}
	return j, nil
}

// launch starts the job's goroutine unless it already runs.
func (s *Scheduler) launch(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.running[id]; ok {
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	r := &runner{cancel: cancel}
	s.running[id] = r
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			if s.running[id] == r { // StopJob and StartJob may have replaced it
				delete(s.running, id)
			}
			s.mu.Unlock()
			cancel()
		}()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("job panicked", "job", id, "panic", r, "stack", string(debug.Stack()))
				s.finish(context.Background(), id, round{failure: fmt.Sprintf("internal error: %v", r), fatal: true})
			}
		}()
		s.run(ctx, id)
	}()
}

// run runs rounds until the job stops being running.
func (s *Scheduler) run(ctx context.Context, id string) {
	if !s.finishRunningRound(ctx, id) {
		return
	}
	for {
		j, ok := s.advance(ctx, id)
		if !ok {
			return
		}
		res := s.runRound(ctx, j)
		if ctx.Err() != nil {
			return // stopped, or the daemon is shutting down
		}
		if !s.finish(ctx, id, res) {
			return
		}
	}
}

// finishRunningRound waits for a round that is still working when the job
// goroutine starts (an old daemon finishes it after an update) and records
// it. It reports whether the job runs on.
func (s *Scheduler) finishRunningRound(ctx context.Context, id string) bool {
	j, err := s.store.GetJob(ctx, id)
	if err != nil || j.Status != api.JobRunning {
		return false
	}
	working := false
	if j.SessionID != "" {
		sess, err := s.sessions.GetSession(ctx, j.SessionID)
		working = err == nil && sess.Status != api.StatusIdle
	}
	var res round
	switch {
	case j.Workflow != "" && j.Step != "":
		// Cut in the middle of a workflow round: let a running turn end,
		// then run the saved step again.
		if working {
			s.waitIdle(ctx, j.SessionID, 0)
		}
		res = s.runWorkflow(ctx, j)
	case working || (j.Workflow == "" && j.SessionID != "" && j.Runs > j.Finished):
		// Still working, or it ended while no scheduler watched (a hand-over
		// cut in before its result was recorded): read its reply. An idle
		// session's saved events are read at once.
		res = s.plainRound(s.waitIdle(ctx, j.SessionID, 0))
	default:
		return true
	}
	return ctx.Err() == nil && s.finish(ctx, id, res)
}

// advance waits for the next round, then counts it and sets the time of the
// one after, before it runs: a crash never runs a round twice.
func (s *Scheduler) advance(ctx context.Context, id string) (api.Job, bool) {
	j, err := s.store.GetJob(ctx, id)
	if err != nil || j.Status != api.JobRunning {
		return j, false
	}
	if j.MaxRuns > 0 && j.Runs >= j.MaxRuns {
		if _, err := s.update(ctx, id, func(j *api.Job) error { j.Status = api.JobDone; return nil }); err != nil {
			slog.Error("job done", "job", id, "err", err)
		}
		return j, false
	}
	select {
	case <-time.After(time.Until(j.NextRunAt)):
	case <-ctx.Done():
		return j, false
	}
	errStopped := errors.New("not running")
	j, err = s.update(ctx, id, func(j *api.Job) error {
		if j.Status != api.JobRunning {
			return errStopped
		}
		j.Runs++
		j.NextRunAt = time.Now().Add(j.Every)
		return nil
	})
	if err != nil {
		if !errors.Is(err, errStopped) {
			slog.Error("job round", "job", id, "err", err)
		}
		return j, false
	}
	return j, true
}

// finish records how a round ended and reports whether the job runs on.
func (s *Scheduler) finish(ctx context.Context, id string, res round) bool {
	j, err := s.update(ctx, id, func(j *api.Job) error {
		j.Finished = j.Runs // recorded: a restart does not read it again
		if j.Status != api.JobRunning {
			return nil
		}
		switch {
		case res.blocked != "" || (res.done && j.Repeat):
			// Blocked, or nothing to do right now: wait longer each time,
			// then look again.
			j.Idle++
			j.Failures, j.Error = 0, ""
			if res.blocked != "" {
				j.Error = "blocked: " + res.blocked
			}
			if wait := idleWait(j.Idle); time.Until(j.NextRunAt) < wait {
				j.NextRunAt = time.Now().Add(wait)
			}
			// A blocker rarely clears by itself: a job without --repeat
			// stops after a few blocked rounds instead of paying for one
			// an hour. (Its Idle only grows on blocked rounds.)
			if res.blocked != "" && !j.Repeat && j.Idle >= maxFailures {
				j.Status = api.JobFailed
			}
		case res.done:
			j.Status, j.Failures, j.Error = api.JobDone, 0, ""
		case res.failure != "":
			j.Failures++
			j.Error = res.failure
			if res.fatal || j.Failures >= maxFailures {
				j.Status = api.JobFailed
			}
		default:
			j.Failures, j.Error, j.Idle = 0, "", 0
		}
		return nil
	})
	if err != nil {
		slog.Error("job round", "job", id, "err", err)
		return false
	}
	if res.blocked != "" && j.Status == api.JobRunning { // a stopped job says so itself
		s.notify(j.SessionID, fmt.Sprintf("job %s is blocked: %s (it tries again at %s)", j.Name, res.blocked, j.NextRunAt.Format("15:04")))
	}
	return j.Status == api.JobRunning
}

// idleWait is how long a repeating job waits after n rounds in a row with
// nothing to do: 1, 2, 4 ... minutes, at most an hour.
func idleWait(n int) time.Duration {
	return min(time.Minute<<min(n-1, 6), time.Hour)
}

// notify tells whoever watches the session (the TUI, later Telegram).
func (s *Scheduler) notify(sessionID, text string) {
	slog.Info("job notice", "session", sessionID, "text", text)
	s.sessions.SendEvent(api.Event{SessionID: sessionID, Time: time.Now(), Kind: api.KindNotice, Text: text})
}

// runRound runs one round: the workflow, or the prompt in a new session in
// the folder of the round before.
func (s *Scheduler) runRound(ctx context.Context, j api.Job) round {
	if j.Workflow != "" {
		return s.runWorkflow(ctx, j)
	}
	id, err := s.sendRound(ctx, j, roundPrompt(j))
	if err != nil {
		return failed(err)
	}
	return s.plainRound(s.waitIdle(ctx, id, 0))
}

// plainRound reads a prompt round's reply.
func (s *Scheduler) plainRound(reply, failure string) round {
	return round{done: strings.Contains(reply, doneMark), failure: failure}
}

// sendRound starts the round's session with text, in the folder of the
// round before, and saves it as the job's latest session.
func (s *Scheduler) sendRound(ctx context.Context, j api.Job, text string) (string, error) {
	res, err := s.sessions.SendMessage(ctx, api.MessageRequest{Dir: j.Dir, Text: text,
		Worktree: !j.NoWorktree, SameDirAs: j.SessionID})
	if err != nil {
		return "", err
	}
	_, err = s.update(ctx, j.ID, func(j *api.Job) error { j.SessionID = res.SessionID; return nil })
	return res.SessionID, err
}

// failed is a round that could not send its message. A folder or session
// that is gone fails the job at once.
func failed(err error) round {
	return round{failure: err.Error(), fatal: api.HasCode(err, api.CodeInvalid, api.CodeNotFound)}
}

// waitIdle watches a session's events after seq after until it is idle
// with nothing queued (the user may steer a round by typing into it). It
// returns the last reply and the last error. A dropped watch resumes after
// the last event seen; a deleted session is a failure.
func (s *Scheduler) waitIdle(ctx context.Context, sessionID string, after int64) (reply, failure string) {
	for ctx.Err() == nil {
		events, err := s.sessions.Watch(ctx, api.WatchFilter{SessionID: sessionID, AfterSeq: after})
		if err != nil {
			return reply, err.Error()
		}
		for ev := range events {
			if ev.Seq != 0 {
				after = ev.Seq
			}
			switch ev.Kind {
			case api.KindText:
				reply = ev.Text
			case api.KindError:
				failure = ev.Text
			case api.KindTurnDone:
				gone, idle := s.checkRound(ctx, sessionID)
				if gone {
					return reply, "the round's session " + sessionID + " was deleted"
				}
				if idle {
					return reply, failure
				}
			}
		}
		if gone, _ := s.checkRound(ctx, sessionID); gone {
			return reply, "the round's session " + sessionID + " was deleted"
		}
	}
	return reply, failure
}

// checkRound reports whether the round's session is gone, or idle with
// nothing queued.
func (s *Scheduler) checkRound(ctx context.Context, sessionID string) (gone, idle bool) {
	sess, err := s.sessions.GetSession(ctx, sessionID)
	if api.IsNotFound(err) {
		return true, false
	}
	return false, err == nil && sess.Status == api.StatusIdle && len(sess.Queued) == 0
}

// roundPrompt tells the agent how a round works. The first line is the
// round session's title.
func roundPrompt(j api.Job) string {
	if j.Repeat {
		return fmt.Sprintf("Job %s, round %d. It repeats until the user stops it. Only when you found nothing to do in this round, end your reply with %s: Germes then waits a while before the next round. After doing work, do not write it.\n\n%s",
			j.Name, j.Runs, doneMark, j.Prompt)
	}
	return fmt.Sprintf(`Job %s, round %d.

Each round starts with fresh context in the same folder. Read PROGRESS.md first if it exists, do the next piece of work toward the goal, then update PROGRESS.md (what is done, what is left). When nothing is left, end your reply with %s.

Goal: %s`, j.Name, j.Runs, doneMark, j.Prompt)
}
