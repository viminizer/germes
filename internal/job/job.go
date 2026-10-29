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
	StopSession(ctx context.Context, r api.StopRequest) error
	Watch(ctx context.Context, f api.WatchFilter) (<-chan api.Event, error)
}

// Scheduler runs every running job in its own goroutine.
type Scheduler struct {
	store    Store
	sessions Sessions
	ctx      context.Context // cancelled by Stop
	cancel   context.CancelFunc
	wg       sync.WaitGroup

	mu      sync.Mutex // one read-modify-write of a job row at a time; guards running
	running map[string]*runner
}

// runner is one job's goroutine.
type runner struct{ cancel context.CancelFunc }

// round is how one round ended.
type round struct {
	done    bool   // the agent replied [DONE]
	failure string // why it failed; "" = it worked
	fatal   bool   // the job cannot run any more (its folder is gone)
}

// NewScheduler creates a scheduler.
func NewScheduler(store Store, sessions Sessions) *Scheduler {
	ctx, cancel := context.WithCancel(context.Background())
	return &Scheduler{store: store, sessions: sessions, ctx: ctx, cancel: cancel, running: map[string]*runner{}}
}

// Start resumes the jobs that were running when the daemon stopped. A round
// missed while the daemon was off runs once, right away.
func (s *Scheduler) Start(ctx context.Context) error {
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
// in the database, so the next start resumes them.
func (s *Scheduler) Stop(ctx context.Context) error {
	s.cancel()
	return stream.Wait(ctx, &s.wg)
}

// CreateJob saves a job and starts its first round.
func (s *Scheduler) CreateJob(ctx context.Context, r api.JobRequest) (api.Job, error) {
	prompt := strings.TrimSpace(r.Prompt)
	switch {
	case prompt == "":
		return api.Job{}, api.NewError(api.CodeInvalid, "empty prompt")
	case r.Every < 0 || r.MaxRuns < 0:
		return api.Job{}, api.NewError(api.CodeInvalid, "every and max runs must not be negative")
	}
	if fi, err := os.Stat(r.Dir); err != nil || !fi.IsDir() {
		return api.Job{}, api.NewError(api.CodeInvalid, "folder %q does not exist", r.Dir)
	}
	now := time.Now()
	j := api.Job{ID: api.NewID(), Name: strings.TrimSpace(r.Name), Dir: r.Dir, Prompt: prompt, NoWorktree: r.NoWorktree,
		Every: r.Every, MaxRuns: r.MaxRuns, NextRunAt: now, Status: api.JobRunning, CreatedAt: now, UpdatedAt: now}
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
// right away.
func (s *Scheduler) StartJob(ctx context.Context, id string) error {
	j, err := s.update(ctx, id, func(j *api.Job) error {
		if j.MaxRuns > 0 && j.Runs >= j.MaxRuns {
			return api.NewError(api.CodeInvalid, "job %s already ran all %d rounds", j.Name, j.MaxRuns)
		}
		j.Status, j.Failures, j.Error = api.JobRunning, 0, ""
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
func (s *Scheduler) update(ctx context.Context, id string, change func(*api.Job) error) (api.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, err := s.store.GetJob(ctx, id)
	if err != nil {
		return j, err
	}
	if err := change(&j); err != nil {
		return j, err
	}
	j.UpdatedAt = time.Now()
	return j, s.store.UpdateJob(ctx, j)
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
		if j.Status != api.JobRunning {
			return nil
		}
		switch {
		case res.done:
			j.Status, j.Failures, j.Error = api.JobDone, 0, ""
		case res.failure != "":
			j.Failures++
			j.Error = res.failure
			if res.fatal || j.Failures >= maxFailures {
				j.Status = api.JobFailed
			}
		default:
			j.Failures, j.Error = 0, ""
		}
		return nil
	})
	if err != nil {
		slog.Error("job round", "job", id, "err", err)
		return false
	}
	return j.Status == api.JobRunning
}

// runRound sends the round's prompt to a new session, in the folder of the
// round before, and waits until that session has nothing left to do.
func (s *Scheduler) runRound(ctx context.Context, j api.Job) round {
	res, err := s.sessions.SendMessage(ctx, api.MessageRequest{Dir: j.Dir, Text: roundPrompt(j),
		NoWorktree: j.NoWorktree, SameDirAs: j.SessionID})
	if err != nil {
		var e *api.Error
		fatal := errors.As(err, &e) && (e.Code == api.CodeInvalid || e.Code == api.CodeNotFound)
		return round{failure: err.Error(), fatal: fatal}
	}
	if _, err := s.update(ctx, j.ID, func(j *api.Job) error { j.SessionID = res.SessionID; return nil }); err != nil {
		return round{failure: err.Error()}
	}
	return s.waitRound(ctx, res.SessionID)
}

// waitRound watches a round's session until it is idle with nothing queued
// (the user may steer a round by typing into it). A dropped watch resumes
// after the last event seen. A deleted session fails the round.
func (s *Scheduler) waitRound(ctx context.Context, sessionID string) round {
	var res round
	var after int64
	for ctx.Err() == nil {
		events, err := s.sessions.Watch(ctx, api.WatchFilter{SessionID: sessionID, AfterSeq: after})
		if err != nil {
			return round{failure: err.Error()}
		}
		for ev := range events {
			if ev.Seq != 0 {
				after = ev.Seq
			}
			switch ev.Kind {
			case api.KindText:
				res.done = strings.Contains(ev.Text, doneMark)
			case api.KindError:
				res.failure = ev.Text
			case api.KindTurnDone:
				gone, idle := s.checkRound(ctx, sessionID)
				if gone {
					return round{failure: "the round's session " + sessionID + " was deleted"}
				}
				if idle {
					return res
				}
			}
		}
		if gone, _ := s.checkRound(ctx, sessionID); gone {
			return round{failure: "the round's session " + sessionID + " was deleted"}
		}
	}
	return res
}

// checkRound reports whether the round's session is gone, or idle with
// nothing queued.
func (s *Scheduler) checkRound(ctx context.Context, sessionID string) (gone, idle bool) {
	sess, err := s.sessions.GetSession(ctx, sessionID)
	var e *api.Error
	if errors.As(err, &e) && e.Code == api.CodeNotFound {
		return true, false
	}
	return false, err == nil && sess.Status == api.StatusIdle && len(sess.Queued) == 0
}

// roundPrompt tells the agent how a round works. The first line is the
// round session's title.
func roundPrompt(j api.Job) string {
	return fmt.Sprintf(`Job %s, round %d.

Each round starts with fresh context in the same folder. Read PROGRESS.md first if it exists, do the next piece of work toward the goal, then update PROGRESS.md (what is done, what is left). When nothing is left, end your reply with %s.

Goal: %s`, j.Name, j.Runs, doneMark, j.Prompt)
}
