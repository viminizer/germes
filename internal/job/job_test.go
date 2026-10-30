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
// reply of "" fails the turn with an error.
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
	if reply == "" {
		out <- engine.Event{Kind: engine.KindError, Err: errors.New("engine broke")}
	} else {
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
	t.Fatal("job did not get there")
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
