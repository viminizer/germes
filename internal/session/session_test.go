package session

import (
	"context"
	"path/filepath"
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
	m := NewManager(s, r)
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
	m.StopSession(ctx, res.SessionID)
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
	m.StopSession(ctx, res.SessionID)
	next(t, ch, api.KindTurnDone)
	if s, _ := m.GetSession(ctx, res.SessionID); len(s.Queued) != 0 {
		t.Fatalf("queue kept after stop: %v", s.Queued)
	}
}
