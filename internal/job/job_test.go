package job

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/viminizer/germes/internal/api"
	"github.com/viminizer/germes/internal/engine"
	"github.com/viminizer/germes/internal/session"
	"github.com/viminizer/germes/internal/store"
)

// scriptRunner answers round n with replies[n-1] (the last one repeats). A
// reply of "" fails the turn with an error; "!interrupted" ends it as a
// daemon restart does.
type scriptRunner struct {
	mu      sync.Mutex
	replies []string
	turns   []engine.Turn
}

func (r *scriptRunner) Run(ctx context.Context, t engine.Turn) (<-chan engine.Event, error) {
	r.mu.Lock()
	r.turns = append(r.turns, t)
	reply := r.replies[min(len(r.turns), len(r.replies))-1]
	r.mu.Unlock()
	out := make(chan engine.Event, 2)
	switch reply {
	case "":
		out <- engine.Event{Kind: engine.KindError, Err: errors.New("engine broke")}
	case "!interrupted":
		out <- engine.Event{Kind: engine.KindError, Err: errors.New(api.TextInterrupted)}
	default:
		out <- engine.Event{Kind: engine.KindText, Text: reply}
	}
	out <- engine.Event{Kind: engine.KindDone, Engine: "fake"}
	close(out)
	return out, nil
}

func newTestScheduler(t *testing.T, replies ...string) (*Scheduler, *scriptRunner) {
	t.Helper()
	return newTestSchedulerWith(t, Options{}, replies...)
}

func newTestSchedulerWith(t *testing.T, opts Options, replies ...string) (*Scheduler, *scriptRunner) {
	t.Helper()
	st, err := store.New(filepath.Join(t.TempDir(), "germes.db"))
	if err != nil {
		t.Fatal(err)
	}
	r := &scriptRunner{replies: replies}
	sessions := session.NewManager(st, r, session.Options{})
	s := NewScheduler(st, sessions, opts)
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.Stop(context.Background())
		sessions.Stop(context.Background())
		st.Close()
	})
	return s, r
}

// waitStatus waits until the job is no longer running.
func waitStatus(t *testing.T, s *Scheduler, id string) api.Job {
	t.Helper()
	return waitJob(t, s, id, func(j api.Job) bool { return j.Status != api.JobRunning })
}

// waitJob waits until ok says yes about the job.
func waitJob(t *testing.T, s *Scheduler, id string, ok func(api.Job) bool) api.Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		j, err := s.GetJob(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if ok(j) {
			return j
		}
		time.Sleep(10 * time.Millisecond)
	}
	j, _ := s.GetJob(context.Background(), id)
	t.Fatalf("job did not get there: %+v", j)
	return api.Job{}
}

func TestJobRunsUntilDone(t *testing.T) {
	s, r := newTestScheduler(t, "did part one", "all finished [DONE]")
	j, err := s.CreateJob(context.Background(), api.JobRequest{Name: "build", Dir: t.TempDir(), Prompt: "build it"})
	if err != nil {
		t.Fatal(err)
	}
	j = waitStatus(t, s, j.ID)
	if j.Status != api.JobDone || j.Runs != 2 {
		t.Fatalf("status %s after %d runs, want done after 2", j.Status, j.Runs)
	}
	// Each round is a new session, in the same folder.
	if r.turns[0].SessionID == r.turns[1].SessionID || r.turns[0].Dir != r.turns[1].Dir {
		t.Fatalf("rounds: %+v", r.turns)
	}
}

func TestJobFailsAfterThreeFailedRounds(t *testing.T) {
	s, _ := newTestScheduler(t, "")
	j, _ := s.CreateJob(context.Background(), api.JobRequest{Dir: t.TempDir(), Prompt: "x"})
	j = waitStatus(t, s, j.ID)
	if j.Status != api.JobFailed || j.Runs != maxFailures || j.Error != "engine broke" {
		t.Fatalf("got %s after %d runs (%q)", j.Status, j.Runs, j.Error)
	}
}

func TestJobStopsAtMaxRuns(t *testing.T) {
	s, _ := newTestScheduler(t, "ok")
	j, _ := s.CreateJob(context.Background(), api.JobRequest{Dir: t.TempDir(), Prompt: "x", MaxRuns: 2})
	j = waitStatus(t, s, j.ID)
	if j.Status != api.JobDone || j.Runs != 2 {
		t.Fatalf("got %s after %d runs", j.Status, j.Runs)
	}
	if err := s.StartJob(context.Background(), j.ID); err == nil {
		t.Fatal("started a job that ran all its rounds")
	}
}

func TestRepeatingJobWaitsWhenIdle(t *testing.T) {
	s, r := newTestScheduler(t, "nothing to do [DONE]")
	j, err := s.CreateJob(context.Background(), api.JobRequest{Dir: t.TempDir(), Prompt: "tidy up", Repeat: true})
	if err != nil {
		t.Fatal(err)
	}
	j = waitJob(t, s, j.ID, func(j api.Job) bool { return j.Idle > 0 })
	if j.Status != api.JobRunning || j.Idle != 1 || time.Until(j.NextRunAt) < 50*time.Second {
		t.Fatalf("got %s, idle %d, next round in %s; want running, idle 1, about a minute", j.Status, j.Idle, time.Until(j.NextRunAt))
	}
	if strings.Contains(r.turns[0].Prompt, "PROGRESS.md") {
		t.Fatalf("a repeating round got the goal instructions: %q", r.turns[0].Prompt)
	}
}

func TestStartingAnIdleJobRunsNow(t *testing.T) {
	s, _ := newTestScheduler(t, "nothing to do [DONE]", "nothing to do [DONE]")
	ctx := context.Background()
	j, _ := s.CreateJob(ctx, api.JobRequest{Dir: t.TempDir(), Prompt: "tidy up", Repeat: true})
	waitJob(t, s, j.ID, func(j api.Job) bool { return j.Idle > 0 })
	if err := s.StopJob(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.StartJob(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	waitJob(t, s, j.ID, func(j api.Job) bool { return j.Runs == 2 && j.Idle == 1 })
}

// A round can end during a hand-over, while no scheduler watches it: the
// next scheduler must still read its [DONE] instead of starting a new round.
func TestRoundThatEndedDuringAHandOverIsRead(t *testing.T) {
	s, r := newTestScheduler(t, "all finished [DONE]", "an extra round")
	ctx := context.Background()
	dir := t.TempDir()
	res, err := s.sessions.SendMessage(ctx, api.MessageRequest{Dir: dir, Text: "round 1"})
	if err != nil {
		t.Fatal(err)
	}
	waitIdleSession(t, s, res.SessionID)
	// Round 1 was counted and ran, but its result was never recorded.
	j := api.Job{ID: "handover", Name: "handover", Dir: dir, Prompt: "build it", Status: api.JobStopped, Runs: 1,
		SessionID: res.SessionID, NextRunAt: time.Now()}
	if err := s.store.CreateJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	if err := s.StartJob(ctx, j.ID); err != nil { // its goroutine starts like after a hand-over
		t.Fatal(err)
	}
	j = waitStatus(t, s, j.ID)
	if j.Status != api.JobDone || j.Runs != 1 || len(r.turns) != 1 {
		t.Fatalf("job %s after %d runs and %d turns; want done after the one round", j.Status, j.Runs, len(r.turns))
	}
}

// A round the user steered has more than one turn: its last reply counts.
func TestHandOverReadsTheRoundsLastTurn(t *testing.T) {
	s, r := newTestScheduler(t, "working on it", "all finished [DONE]", "an extra round")
	ctx := context.Background()
	dir := t.TempDir()
	res, err := s.sessions.SendMessage(ctx, api.MessageRequest{Dir: dir, Text: "round 1"})
	if err != nil {
		t.Fatal(err)
	}
	waitIdleSession(t, s, res.SessionID)
	if _, err := s.sessions.SendMessage(ctx, api.MessageRequest{SessionID: res.SessionID, Text: "finish it"}); err != nil {
		t.Fatal(err)
	}
	waitTurns(t, s, res.SessionID, 2)
	j := api.Job{ID: "steered", Name: "steered", Dir: dir, Prompt: "build it", Status: api.JobStopped, Runs: 1,
		SessionID: res.SessionID, NextRunAt: time.Now()}
	if err := s.store.CreateJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	if err := s.StartJob(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	j = waitStatus(t, s, j.ID)
	if j.Status != api.JobDone || j.Runs != 1 || len(r.turns) != 2 {
		t.Fatalf("job %s after %d runs and %d turns; want done from the last turn's [DONE]", j.Status, j.Runs, len(r.turns))
	}
}

// A round a daemon restart cut is not a failed round: the job runs on.
func TestRoundCutByARestartIsNotAFailure(t *testing.T) {
	s, _ := newTestScheduler(t, "!interrupted", "all finished [DONE]")
	ctx := context.Background()
	dir := t.TempDir()
	res, err := s.sessions.SendMessage(ctx, api.MessageRequest{Dir: dir, Text: "round 1"})
	if err != nil {
		t.Fatal(err)
	}
	waitIdleSession(t, s, res.SessionID)
	// Two failures already: one more would fail the job.
	j := api.Job{ID: "cut", Name: "cut", Dir: dir, Prompt: "build it", Status: api.JobRunning, Runs: 1, Failures: 2,
		SessionID: res.SessionID, NextRunAt: time.Now()}
	if err := s.store.CreateJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	s.Stop(ctx)
	if err := s.Start(ctx); err != nil { // a new daemon resumes its running jobs
		t.Fatal(err)
	}
	j = waitStatus(t, s, j.ID)
	if j.Status != api.JobDone || j.Runs != 2 {
		t.Fatalf("job %s after %d runs (%s); want round 2 to run and finish", j.Status, j.Runs, j.Error)
	}
}

// waitTurns waits until the session has n finished turns and is idle.
func waitTurns(t *testing.T, s *Scheduler, id string, n int) {
	t.Helper()
	ctx := context.Background()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		page, _ := s.sessions.ListEvents(ctx, api.EventFilter{SessionID: id})
		done := 0
		for _, ev := range page.Items {
			if ev.Kind == api.KindTurnDone {
				done++
			}
		}
		if sess, err := s.sessions.GetSession(ctx, id); done >= n && err == nil && sess.Status == api.StatusIdle {
			return
		}
	}
	t.Fatalf("the session never finished %d turns", n)
}

// A round whose result was recorded is not read again.
func TestRecordedRoundIsNotReadAgain(t *testing.T) {
	s, r := newTestScheduler(t, "all finished [DONE]", "round 2 [DONE]")
	ctx := context.Background()
	dir := t.TempDir()
	res, err := s.sessions.SendMessage(ctx, api.MessageRequest{Dir: dir, Text: "round 1"})
	if err != nil {
		t.Fatal(err)
	}
	waitIdleSession(t, s, res.SessionID)
	// Round 1's result was recorded (Finished), so its [DONE] is not read
	// again: round 2 runs. This also checks the store keeps Finished.
	j := api.Job{ID: "recorded", Name: "recorded", Dir: dir, Prompt: "build it", Status: api.JobStopped, Runs: 1, Finished: 1,
		SessionID: res.SessionID, NextRunAt: time.Now()}
	if err := s.store.CreateJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	if err := s.StartJob(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	j = waitStatus(t, s, j.ID)
	if j.Status != api.JobDone || j.Runs != 2 || len(r.turns) != 2 {
		t.Fatalf("job %s after %d runs and %d turns; want round 2 to run", j.Status, j.Runs, len(r.turns))
	}
}

// waitIdleSession waits until the session's turn is done and it is idle
// (a new session shows idle before its first turn starts).
func waitIdleSession(t *testing.T, s *Scheduler, id string) {
	t.Helper()
	ctx := context.Background()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		page, err := s.sessions.ListEvents(ctx, api.EventFilter{SessionID: id})
		done := err == nil && len(page.Items) > 0 && page.Items[len(page.Items)-1].Kind == api.KindTurnDone
		if sess, err := s.sessions.GetSession(ctx, id); done && err == nil && sess.Status == api.StatusIdle {
			return
		}
	}
	t.Fatal("the round's turn never ended")
}
