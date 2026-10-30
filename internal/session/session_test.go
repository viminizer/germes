package session

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/viminizer/germes/internal/api"
	"github.com/viminizer/germes/internal/engine"
	"github.com/viminizer/germes/internal/store"
)

// fakeRunner answers every turn with a delta, a text and done. When block is
// set, it waits for it before answering. prompts records each turn's prompt.
// status, when set, is sent first.
type fakeRunner struct {
	block   chan struct{}
	prompts chan string
	turns   chan engine.Turn
	status  string
}

func (f fakeRunner) Run(ctx context.Context, t engine.Turn) (<-chan engine.Event, error) {
	if f.prompts != nil {
		f.prompts <- t.Prompt
	}
	if f.turns != nil {
		f.turns <- t
	}
	out := make(chan engine.Event, 4)
	go func() {
		defer close(out)
		if f.block != nil {
			select {
			case <-f.block:
			case <-ctx.Done():
				return
			}
		}
		if f.status != "" {
			out <- engine.Event{Kind: engine.KindStatus, Text: f.status}
		}
		out <- engine.Event{Kind: engine.KindTextDelta, Text: "o"}
		out <- engine.Event{Kind: engine.KindText, Text: "ok"}
		out <- engine.Event{Kind: engine.KindDone, Engine: "fake", ResumeID: "r1"}
	}()
	return out, nil
}

func newTestManager(t *testing.T, r Runner) *Manager {
	t.Helper()
	s, err := store.New(filepath.Join(t.TempDir(), "germes.db"))
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(s, r, Options{Worktrees: t.TempDir()})
	t.Cleanup(func() { m.Stop(context.Background()); s.Close() })
	return m
}

// next reads events until one of kind k arrives.
func next(t *testing.T, ch <-chan api.Event, k api.EventKind) api.Event {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("watch closed before %s", k)
			}
			if ev.Kind == k {
				return ev
			}
		case <-timeout:
			t.Fatalf("no %s event", k)
		}
	}
}

func TestSendAndWatch(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t, fakeRunner{})
	res, err := m.SendMessage(ctx, api.MessageRequest{Dir: t.TempDir(), Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	ch, _ := m.Watch(ctx, api.WatchFilter{SessionID: res.SessionID})
	next(t, ch, api.KindTurnDone)

	// Second turn on the same session: seq keeps counting, resume id saved.
	if _, err := m.SendMessage(ctx, api.MessageRequest{SessionID: res.SessionID, Text: "again"}); err != nil {
		t.Fatal(err)
	}
	next(t, ch, api.KindTurnDone)

	page, err := m.ListEvents(ctx, api.EventFilter{SessionID: res.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	for i, ev := range page.Items {
		if ev.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d (gap or duplicate)", i, ev.Seq)
		}
		if ev.Kind == api.KindTextDelta {
			t.Fatal("text delta was saved")
		}
	}
	sess, _ := m.GetSession(ctx, res.SessionID)
	if sess.ResumeIDs["fake"] != "r1" || sess.Status != api.StatusIdle || sess.Title != "hello" {
		t.Fatalf("session = %+v", sess)
	}

	// A late watcher replays history without duplicates.
	replay, _ := m.Watch(ctx, api.WatchFilter{SessionID: res.SessionID, AfterSeq: 2})
	first := next(t, replay, api.KindText)
	if first.Seq != 5 { // user 1, text 2, done 3, user 4, text 5
		t.Fatalf("replay gave text seq %d, want 5", first.Seq)
	}
}

func TestEngineStatusIsLiveOnly(t *testing.T) {
	ctx := context.Background()
	block := make(chan struct{})
	m := newTestManager(t, fakeRunner{block: block, status: "codex: overloaded. Trying again in 6s (retry 1 of 2)."})
	res, err := m.SendMessage(ctx, api.MessageRequest{Dir: t.TempDir(), Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	ch, _ := m.Watch(ctx, api.WatchFilter{SessionID: res.SessionID})
	close(block)
	if ev := next(t, ch, api.KindStatus); ev.Text != "codex: overloaded. Trying again in 6s (retry 1 of 2)." || ev.SessionID != res.SessionID {
		t.Fatalf("status = %+v", ev)
	}
	next(t, ch, api.KindTurnDone)
	page, _ := m.ListEvents(ctx, api.EventFilter{SessionID: res.SessionID})
	for _, ev := range page.Items {
		if ev.Kind == api.KindStatus {
			t.Fatal("a retry status was saved in the history")
		}
	}
}

func TestStopSession(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t, fakeRunner{block: make(chan struct{})})
	res, err := m.SendMessage(ctx, api.MessageRequest{Dir: t.TempDir(), Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	ch, _ := m.Watch(ctx, api.WatchFilter{SessionID: res.SessionID})
	next(t, ch, api.KindUserMessage)
	m.StopSession(ctx, api.StopRequest{ID: res.SessionID})
	if ev := next(t, ch, api.KindError); ev.Text != "stopped" {
		t.Fatalf("error = %q", ev.Text)
	}
	next(t, ch, api.KindTurnDone)
}

func TestQueuedMessagesMergeIntoOneTurn(t *testing.T) {
	ctx := context.Background()
	r := fakeRunner{block: make(chan struct{}), prompts: make(chan string, 10)}
	m := newTestManager(t, r)
	res, err := m.SendMessage(ctx, api.MessageRequest{Dir: t.TempDir(), Text: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if p := <-r.prompts; p != "a" {
		t.Fatalf("first prompt = %q", p)
	}
	ch, _ := m.Watch(ctx, api.WatchFilter{SessionID: res.SessionID})
	for _, text := range []string{"b", "c"} {
		if _, err := m.SendMessage(ctx, api.MessageRequest{SessionID: res.SessionID, Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	if q := next(t, ch, api.KindQueue).Queue; len(q) != 1 {
		t.Fatalf("queue after b = %v", q)
	}
	if s, _ := m.GetSession(ctx, res.SessionID); len(s.Queued) != 2 {
		t.Fatalf("GetSession queued = %v", s.Queued)
	}
	close(r.block)
	if p := <-r.prompts; p != "b\n\nc" {
		t.Fatalf("merged prompt = %q", p)
	}
}

func TestStopDropsQueue(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t, fakeRunner{block: make(chan struct{})})
	res, _ := m.SendMessage(ctx, api.MessageRequest{Dir: t.TempDir(), Text: "a"})
	ch, _ := m.Watch(ctx, api.WatchFilter{SessionID: res.SessionID})
	next(t, ch, api.KindUserMessage)
	m.SendMessage(ctx, api.MessageRequest{SessionID: res.SessionID, Text: "b"})
	m.StopSession(ctx, api.StopRequest{ID: res.SessionID})
	next(t, ch, api.KindTurnDone)
	if s, _ := m.GetSession(ctx, res.SessionID); len(s.Queued) != 0 {
		t.Fatalf("queue kept after stop: %v", s.Queued)
	}
}

func TestStopAndSendQueue(t *testing.T) {
	ctx := context.Background()
	r := fakeRunner{block: make(chan struct{}), prompts: make(chan string, 10)}
	m := newTestManager(t, r)
	res, _ := m.SendMessage(ctx, api.MessageRequest{Dir: t.TempDir(), Text: "a"})
	<-r.prompts
	m.SendMessage(ctx, api.MessageRequest{SessionID: res.SessionID, Text: "b"})
	m.StopSession(ctx, api.StopRequest{ID: res.SessionID, SendQueue: true})
	select {
	case p := <-r.prompts:
		if p != "b" {
			t.Fatalf("next prompt = %q, want the queued b", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("queue was not sent after stop")
	}
}

func TestSessionEntersAndLeavesWorktree(t *testing.T) {
	ctx := context.Background()
	project := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"commit", "-q", "--allow-empty", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = project
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	m := newTestManager(t, fakeRunner{})
	// By default a session works in the project folder.
	res, err := m.SendMessage(ctx, api.MessageRequest{Dir: project, Text: "hi"})
	if err != nil || res.Warning != "" {
		t.Fatalf("send: %v %q", err, res.Warning)
	}
	ch, _ := m.Watch(ctx, api.WatchFilter{SessionID: res.SessionID})
	next(t, ch, api.KindTurnDone)
	if s, _ := m.GetSession(ctx, res.SessionID); s.Dir != project || s.Branch != "" {
		t.Fatalf("new session = %+v", s)
	}
	// Asked for one, it moves into its own worktree.
	if err := m.UpdateSession(ctx, api.SessionUpdate{ID: res.SessionID, EnterWorktree: true}); err != nil {
		t.Fatal(err)
	}
	s, _ := m.GetSession(ctx, res.SessionID)
	if s.Branch != "germes/"+s.ID || s.Dir == project || s.Project != project {
		t.Fatalf("session = %+v", s)
	}
	if err := m.UpdateSession(ctx, api.SessionUpdate{ID: s.ID, EnterWorktree: true}); err == nil {
		t.Fatal("entered a second worktree")
	}

	// Work in the worktree: neither leaving nor deleting may lose it.
	os.WriteFile(s.Dir+"/new.txt", []byte("x"), 0o644)
	if err := m.UpdateSession(ctx, api.SessionUpdate{ID: s.ID, LeaveWorktree: true}); !api.HasCode(err, api.CodeInvalid) {
		t.Fatalf("leave with work: %v", err)
	}
	if err := m.DeleteSession(ctx, s.ID); err == nil {
		t.Fatal("deleted a session whose worktree has work")
	}
	// Without work it leaves cleanly and the session moves to the project.
	os.Remove(s.Dir + "/new.txt")
	if err := m.UpdateSession(ctx, api.SessionUpdate{ID: s.ID, LeaveWorktree: true}); err != nil {
		t.Fatal(err)
	}
	if s, _ := m.GetSession(ctx, s.ID); s.Dir != project || s.Branch != "" {
		t.Fatalf("after leaving: %+v", s)
	}
}

func TestPlanTurnSavesThePlan(t *testing.T) {
	ctx := context.Background()
	project := t.TempDir()
	turns := make(chan engine.Turn, 1)
	m := newTestManager(t, fakeRunner{turns: turns})
	res, err := m.SendMessage(ctx, api.MessageRequest{Dir: project, Text: "Add dark mode!", Plan: true})
	if err != nil {
		t.Fatal(err)
	}
	ch, _ := m.Watch(ctx, api.WatchFilter{SessionID: res.SessionID})
	note := next(t, ch, api.KindNotice)
	if turn := <-turns; !turn.Plan {
		t.Fatal("the engine did not get a plan turn")
	}
	files, _ := filepath.Glob(filepath.Join(project, ".germes", "plans", "*-add-dark-mode.md"))
	if len(files) != 1 || !strings.Contains(note.Text, files[0]) {
		t.Fatalf("plans %v, notice %q", files, note.Text)
	}
	if b, _ := os.ReadFile(files[0]); string(b) != "# Plan: Add dark mode!\n\nok\n" {
		t.Fatalf("plan file:\n%s", b)
	}
}

func TestRollbackToBeforeATurn(t *testing.T) {
	ctx := context.Background()
	project := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"commit", "-q", "--allow-empty", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = project
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	os.WriteFile(filepath.Join(project, "a.txt"), []byte("before"), 0o644)
	st, err := store.New(filepath.Join(t.TempDir(), "germes.db"))
	if err != nil {
		t.Fatal(err)
	}
	block, started := make(chan struct{}), make(chan string, 1)
	m := NewManager(st, fakeRunner{block: block, prompts: started}, Options{Checkpoints: t.TempDir()})
	t.Cleanup(func() { m.Stop(ctx); st.Close() })

	res, _ := m.SendMessage(ctx, api.MessageRequest{Dir: project, Text: "change a.txt"})
	ch, _ := m.Watch(ctx, api.WatchFilter{SessionID: res.SessionID})
	<-started                                                             // the snapshot is taken before the engine runs
	os.WriteFile(filepath.Join(project, "a.txt"), []byte("after"), 0o644) // the turn's work
	list, err := m.ListCheckpoints(ctx, res.SessionID)
	if err != nil || len(list) != 1 {
		t.Fatalf("snapshots %v, %v", list, err)
	}
	if err := m.RestoreCheckpoint(ctx, res.SessionID, list[0].ID); err == nil {
		t.Fatal("restored while the turn runs")
	}
	close(block)
	next(t, ch, api.KindTurnDone)
	waitIdle(t, m, res.SessionID)
	if err := m.RestoreCheckpoint(ctx, res.SessionID, list[0].ID); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(project, "a.txt")); string(b) != "before" {
		t.Fatalf("a.txt = %q", b)
	}
}

// waitIdle waits until the session's worker has nothing running or queued.
func waitIdle(t *testing.T, m *Manager, id string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if len(m.ListBusy()) == 0 {
			return
		}
	}
	t.Fatalf("session %s stays busy", id)
}

// busyElsewhere says the listed sessions still run in an old daemon.
type busyElsewhere map[string]bool

func (b busyElsewhere) IsBusy(_ context.Context, id string) bool { return b[id] }

// A daemon that stopped or crashed left turns behind: working ones, and
// paused ones waiting for an engine limit (their timer was only in memory).
// Start marks both interrupted, but not one an old daemon still runs.
func TestStartRecoversCutAndPausedTurns(t *testing.T) {
	ctx := context.Background()
	st, err := store.New(filepath.Join(t.TempDir(), "germes.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	dir, now := t.TempDir(), time.Now()
	for id, status := range map[string]api.Status{"cut": api.StatusWorking, "paused": api.StatusPaused,
		"old": api.StatusPaused, "idle": api.StatusIdle} {
		s := api.Session{ID: id, Title: id, Project: dir, Dir: dir, Status: status, CreatedAt: now, UpdatedAt: now}
		if err := st.CreateSession(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	m := NewManager(st, fakeRunner{}, Options{Elsewhere: busyElsewhere{"old": true}})
	defer m.Stop(ctx)
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]api.Status{"cut": api.StatusIdle, "paused": api.StatusIdle, "old": api.StatusPaused, "idle": api.StatusIdle} {
		s, err := m.GetSession(ctx, id)
		if err != nil || s.Status != want {
			t.Errorf("%s: status %s (%v), want %s", id, s.Status, err, want)
		}
		page, _ := m.ListEvents(ctx, api.EventFilter{SessionID: id})
		var kinds []string
		for _, ev := range page.Items {
			kinds = append(kinds, string(ev.Kind)+":"+ev.Text)
		}
		got, interrupted := strings.Join(kinds, " "), "error:"+api.TextInterrupted+" turn_done:"
		if (id == "cut" || id == "paused") != (got == interrupted) {
			t.Errorf("%s: events %q", id, got)
		}
	}
}
