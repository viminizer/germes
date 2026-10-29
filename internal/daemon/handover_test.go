package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/viminizer/germes/internal/api"
	"github.com/viminizer/germes/internal/engine"
	"github.com/viminizer/germes/internal/job"
	"github.com/viminizer/germes/internal/session"
	"github.com/viminizer/germes/internal/store"
)

// gateRunner runs each turn until the test releases it: a delta, then (after
// release) another delta and the reply "reply from <name>".
type gateRunner struct {
	name    string
	started chan string // each turn's prompt
	release chan struct{}
}

func newGateRunner(name string) *gateRunner {
	return &gateRunner{name: name, started: make(chan string, 10), release: make(chan struct{})}
}

func (r *gateRunner) Run(ctx context.Context, t engine.Turn) (<-chan engine.Event, error) {
	r.started <- t.Prompt
	out := make(chan engine.Event)
	go func() {
		defer close(out)
		send := func(ev engine.Event) bool {
			select {
			case out <- ev:
				return true
			case <-ctx.Done():
				return false
			}
		}
		if !send(engine.Event{Kind: engine.KindTextDelta, Text: "working"}) {
			return
		}
		select {
		case <-r.release:
		case <-ctx.Done():
			return
		}
		if send(engine.Event{Kind: engine.KindTextDelta, Text: "more"}) &&
			send(engine.Event{Kind: engine.KindText, Text: "reply from " + r.name}) {
			send(engine.Event{Kind: engine.KindDone, Engine: r.name})
		}
	}()
	return out, nil
}

// instance is one daemon of a test, on the shared files in dir.
type instance struct {
	d      *Daemon
	runner *gateRunner
	done   chan struct{} // closed when Run returns
	err    error         // Run's result, set before done closes
}

func newInstance(t *testing.T, dir, name string, startNew func() error) *instance {
	t.Helper()
	st, err := store.New(filepath.Join(dir, "germes.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	old := NewOldDaemons(filepath.Join(dir, "old"))
	r := newGateRunner(name)
	sessions := session.NewManager(st, r, session.Options{Elsewhere: old})
	jobs := job.NewScheduler(st, sessions, job.Options{})
	d := New(sessions, jobs, old, Options{Version: name, SocketPath: filepath.Join(dir, "s.sock"),
		LockPath: filepath.Join(dir, "lock"), OldDir: filepath.Join(dir, "old"), StartNew: startNew})
	return &instance{d: d, runner: r, done: make(chan struct{})}
}

func (in *instance) start(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		select {
		case <-in.done:
		case <-time.After(10 * time.Second):
		}
	})
	go func() {
		in.err = in.d.Run(ctx)
		close(in.done)
	}()
}

// setup starts daemon A; B starts when A hands over (or startErr fails it).
func setup(t *testing.T, startErr error) (a, b *instance, client *api.Client) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "gd") // short: socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	b = newInstance(t, dir, "B", nil)
	a = newInstance(t, dir, "A", func() error {
		if startErr != nil {
			return startErr
		}
		b.start(t)
		return nil
	})
	a.start(t)
	client = api.NewClient(filepath.Join(dir, "s.sock"))
	waitVersion(t, client, "A")
	return a, b, client
}

func waitVersion(t *testing.T, c *api.Client, version string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if d, err := c.GetDaemon(context.Background()); err == nil && d.Version == version {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("daemon %s never answered", version)
}

func receive[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatalf("no %s", what)
	}
	var zero T
	return zero
}

// waitEvent reads ev until one matches.
func waitEvent(t *testing.T, events <-chan api.Event, match func(api.Event) bool, what string) {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatalf("watch closed before %s", what)
			}
			if match(ev) {
				return
			}
		case <-timeout:
			t.Fatalf("no %s", what)
		}
	}
}

func handOver(t *testing.T, a *instance, client *api.Client) {
	t.Helper()
	a.d.update <- struct{}{}
	waitVersion(t, client, "B")
}

// waitClosed drains events until the stream closes.
func waitClosed(t *testing.T, events <-chan api.Event) {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return
			}
		case <-timeout:
			t.Fatal("watch stream still open")
		}
	}
}

func waitExit(t *testing.T, in *instance) error {
	t.Helper()
	receive(t, in.done, "exit of the old daemon")
	return in.err
}

// startTurn sends a message to a new session and waits until A runs it.
func startTurn(t *testing.T, a *instance, client *api.Client) string {
	t.Helper()
	res, err := client.SendMessage(context.Background(), api.MessageRequest{Dir: t.TempDir(), Text: "first"})
	if err != nil {
		t.Fatal(err)
	}
	receive(t, a.runner.started, "turn in A")
	return res.SessionID
}

func TestHandOverFinishesRunningTurn(t *testing.T) {
	a, b, client := setup(t, nil)
	ctx := context.Background()
	id := startTurn(t, a, client)
	before, err := client.Watch(ctx, api.WatchFilter{SessionID: id})
	if err != nil {
		t.Fatal(err)
	}

	handOver(t, a, client)

	// The old daemon closes its watch streams; a client reconnects to B.
	waitClosed(t, before)
	events, err := client.Watch(ctx, api.WatchFilter{SessionID: id})
	if err != nil {
		t.Fatal(err)
	}
	// B did not mark A's running turn as interrupted.
	if s, _ := client.GetSession(ctx, id); s.Status != api.StatusWorking {
		t.Fatalf("status after hand-over: %s", s.Status)
	}
	// A message for the session waits in B while A runs the turn.
	if _, err := client.SendMessage(ctx, api.MessageRequest{SessionID: id, Text: "second"}); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-b.runner.started:
		t.Fatalf("B ran %q while A still ran the session", p)
	case <-time.After(700 * time.Millisecond):
	}

	close(a.runner.release)
	// A's live text reaches B's watchers, then A's reply.
	waitEvent(t, events, func(ev api.Event) bool { return ev.Kind == api.KindTextDelta && ev.Text == "more" }, "relayed delta")
	waitEvent(t, events, func(ev api.Event) bool { return ev.Kind == api.KindText && ev.Text == "reply from A" }, "reply from A")
	// Then B runs the waiting message, and A exits.
	if p := receive(t, b.runner.started, "turn in B"); p != "second" {
		t.Fatalf("B ran %q", p)
	}
	if err := waitExit(t, a); err != nil {
		t.Fatalf("old daemon: %v", err)
	}
	close(b.runner.release)
	waitEvent(t, events, func(ev api.Event) bool { return ev.Kind == api.KindText && ev.Text == "reply from B" }, "reply from B")
	waitEvent(t, events, func(ev api.Event) bool { return ev.Kind == api.KindTurnDone }, "turn_done in B")

	// One clean log: seqs 1..n, nothing lost, nothing twice, no interruption.
	all, err := api.ListAllEvents(ctx, client, api.EventFilter{SessionID: id})
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for i, ev := range all {
		if ev.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d", i, ev.Seq)
		}
		if ev.Kind == api.KindError {
			t.Fatalf("error event: %s", ev.Text)
		}
		kinds = append(kinds, string(ev.Kind))
	}
	want := "user_message text turn_done user_message text turn_done"
	if got := strings.Join(kinds, " "); got != want {
		t.Fatalf("events:\n got %s\nwant %s", got, want)
	}
	if entries, _ := os.ReadDir(filepath.Join(filepath.Dir(a.d.opts.SocketPath), "old")); len(entries) != 0 {
		t.Fatalf("old daemon left its registry file")
	}
}

func TestHandOverStopsOldTurnFromNewDaemon(t *testing.T) {
	a, _, client := setup(t, nil)
	ctx := context.Background()
	id := startTurn(t, a, client)
	handOver(t, a, client)

	// Esc Esc reaches the turn in A through B.
	if err := client.StopSession(ctx, api.StopRequest{ID: id}); err != nil {
		t.Fatal(err)
	}
	if err := waitExit(t, a); err != nil {
		t.Fatalf("old daemon: %v", err)
	}
	all, _ := api.ListAllEvents(ctx, client, api.EventFilter{SessionID: id})
	if last := all[len(all)-1]; last.Kind != api.KindTurnDone || all[len(all)-2].Text != "stopped" {
		t.Fatalf("last events: %+v", all[len(all)-2:])
	}
	if s, _ := client.GetSession(ctx, id); s.Status != api.StatusIdle {
		t.Fatalf("status: %s", s.Status)
	}
}

func TestHandOverFailureKeepsServing(t *testing.T) {
	a, _, client := setup(t, errors.New("no binary"))
	ctx := context.Background()
	id := startTurn(t, a, client)

	a.d.update <- struct{}{}
	time.Sleep(500 * time.Millisecond)

	// A still serves, holds the lock, and its turn runs on.
	waitVersion(t, client, "A")
	if _, err := lock(a.d.opts.LockPath); err == nil {
		t.Fatal("A gave up its lock")
	}
	close(a.runner.release)
	events, _ := client.Watch(ctx, api.WatchFilter{SessionID: id})
	waitEvent(t, events, func(ev api.Event) bool { return ev.Kind == api.KindTurnDone }, "turn_done in A")
	select {
	case <-a.done:
		t.Fatalf("A exited: %v", a.err)
	default:
	}
}

func TestHandOverJobWaitsForRound(t *testing.T) {
	a, b, client := setup(t, nil)
	ctx := context.Background()
	j, err := client.CreateJob(ctx, api.JobRequest{Name: "j", Dir: t.TempDir(), Prompt: "work", MaxRuns: 2})
	if err != nil {
		t.Fatal(err)
	}
	receive(t, a.runner.started, "round 1 in A")
	handOver(t, a, client)

	// B resumes the job but waits for round 1, still running in A.
	select {
	case p := <-b.runner.started:
		t.Fatalf("B started %q while round 1 ran in A", p)
	case <-time.After(700 * time.Millisecond):
	}
	close(a.runner.release)
	if p := receive(t, b.runner.started, "round 2 in B"); !strings.HasPrefix(p, "Job j, round 2.") {
		t.Fatalf("B ran %q", p)
	}
	close(b.runner.release)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if j, err = client.GetJob(ctx, "j"); err == nil && j.Status != api.JobRunning {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if j.Status != api.JobDone || j.Runs != 2 || j.Failures != 0 {
		t.Fatalf("job: %s, %d runs, %d failures (%s)", j.Status, j.Runs, j.Failures, j.Error)
	}
	if err := waitExit(t, a); err != nil {
		t.Fatalf("old daemon: %v", err)
	}
}
