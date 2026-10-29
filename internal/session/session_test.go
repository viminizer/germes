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
type fakeRunner struct {
	block   chan struct{}
	prompts chan string
}

func (f fakeRunner) Run(ctx context.Context, t engine.Turn) (<-chan engine.Event, error) {
	if f.prompts != nil {
		f.prompts <- t.Prompt
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
	m := NewManager(s, r, t.TempDir())
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

func TestSessionGetsOwnWorktree(t *testing.T) {
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
	res, err := m.SendMessage(ctx, api.MessageRequest{Dir: project, Text: "hi"})
	if err != nil || res.Warning != "" {
		t.Fatalf("send: %v %q", err, res.Warning)
	}
	s, _ := m.GetSession(ctx, res.SessionID)
	if s.Branch != "germes/"+s.ID || s.Dir == project || s.Project != project {
		t.Fatalf("session = %+v", s)
	}
	ch, _ := m.Watch(ctx, api.WatchFilter{SessionID: s.ID})
	next(t, ch, api.KindTurnDone)

	// Work in the worktree: neither leaving nor deleting may lose it.
	os.WriteFile(s.Dir+"/new.txt", []byte("x"), 0o644)
	if err := m.UpdateSession(ctx, api.SessionUpdate{ID: s.ID, LeaveWorktree: true}); err == nil || !strings.Contains(err.Error(), "merge") {
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
