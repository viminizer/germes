package session

import (
	"context"
	"path/filepath"
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
