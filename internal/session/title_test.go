package session

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/viminizer/germes/internal/api"
	"github.com/viminizer/germes/internal/store"
)

// gateTitler names every session "Add dark mode" once let through.
type gateTitler chan struct{}

func (g gateTitler) CreateTitle(context.Context, string) (string, error) {
	<-g
	return "Add dark mode", nil
}

func TestModelTitleReplacesTheCutTitleButNotARename(t *testing.T) {
	for _, rename := range []string{"", "my own name"} {
		ctx := context.Background()
		s, err := store.New(filepath.Join(t.TempDir(), "germes.db"))
		if err != nil {
			t.Fatal(err)
		}
		gate := gateTitler(make(chan struct{}))
		m := NewManager(s, fakeRunner{}, Options{Titles: gate})
		res, err := m.SendMessage(ctx, api.MessageRequest{Dir: t.TempDir(), Text: "please add a dark mode to the settings page of the app"})
		if err != nil {
			t.Fatal(err)
		}
		ch, _ := m.Watch(ctx, api.WatchFilter{SessionID: res.SessionID})
		next(t, ch, api.KindTurnDone) // the naming started with the turn
		if rename != "" {
			if err := m.UpdateSession(ctx, api.SessionUpdate{ID: res.SessionID, Title: rename}); err != nil {
				t.Fatal(err)
			}
		}
		close(gate)
		m.Stop(ctx) // waits for the title
		got, err := s.GetSession(ctx, res.SessionID)
		s.Close()
		want := "Add dark mode"
		if rename != "" {
			want = rename
		}
		if err != nil || got.Title != want {
			t.Fatalf("rename %q: title %q (err %v), want %q", rename, got.Title, err, want)
		}
	}
}

// countTitler names every session as a greeting and counts the calls.
type countTitler struct{ calls *atomic.Int32 }

func (c countTitler) CreateTitle(context.Context, string) (string, error) {
	c.calls.Add(1)
	return "friendly Greeting", nil
}

// Like Hermes: small talk asks for a title for 3 messages only, and a
// session made with a title (a job round) never asks.
func TestNamingStopsAfterThreeMessagesAndSkipsGivenTitles(t *testing.T) {
	ctx := context.Background()
	calls := &atomic.Int32{}
	m := newTestManager(t, fakeRunner{})
	m.opts.Titles = countTitler{calls}
	res, err := m.SendMessage(ctx, api.MessageRequest{Dir: t.TempDir(), Text: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	ch, _ := m.Watch(ctx, api.WatchFilter{SessionID: res.SessionID})
	next(t, ch, api.KindTurnDone)
	for range 4 {
		if _, err := m.SendMessage(ctx, api.MessageRequest{SessionID: res.SessionID, Text: "thanks"}); err != nil {
			t.Fatal(err)
		}
		next(t, ch, api.KindTurnDone)
	}
	job, err := m.SendMessage(ctx, api.MessageRequest{Dir: t.TempDir(), Text: "Job x, round 2. Do the work.", Title: "Job x, round 2"})
	if err != nil {
		t.Fatal(err)
	}
	waitIdle(t, m, job.SessionID)
	m.Stop(ctx)
	if n := calls.Load(); n != 3 {
		t.Errorf("asked for a title %d times, want 3", n)
	}
	if s := mustSession(t, m, job.SessionID); s.Title != "Job x, round 2" {
		t.Errorf("job round title %q", s.Title)
	}
}
