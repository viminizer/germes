package daemon

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/viminizer/germes/internal/api"
	"github.com/viminizer/germes/internal/engine"
	"github.com/viminizer/germes/internal/session"
	"github.com/viminizer/germes/internal/store"
)

type ownerStore struct {
	session.Store
	finishing, releaseFinish, claimed, releaseClaim, paused chan struct{}
}

func (s *ownerStore) CreateEvent(ctx context.Context, ev api.Event) error {
	if ev.Kind == api.KindTurnDone && ev.Seq == 2 {
		close(s.finishing)
		<-s.releaseFinish
	}
	if ev.Kind == api.KindUserMessage && ev.Text == "old queued" {
		close(s.claimed)
		<-s.releaseClaim
	}
	return s.Store.CreateEvent(ctx, ev)
}

func (s *ownerStore) UpdateSession(ctx context.Context, row api.Session) error {
	err := s.Store.UpdateSession(ctx, row)
	if err == nil && row.Status == api.StatusPaused {
		close(s.paused)
	}
	return err
}

type ownerRunner func(context.Context, engine.Turn) (<-chan engine.Event, error)

func (r ownerRunner) Run(ctx context.Context, turn engine.Turn) (<-chan engine.Event, error) {
	return r(ctx, turn)
}
func ownerBarrier(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("owner barrier not reached")
	}
}

func TestGetSessionUsesHandoverOwner(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	db, err := store.New(filepath.Join(dir, "germes.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	st := &ownerStore{Store: db, finishing: make(chan struct{}), releaseFinish: make(chan struct{}), claimed: make(chan struct{}), releaseClaim: make(chan struct{}), paused: make(chan struct{})}
	first, started := make(chan engine.Event), make(chan struct{})
	oldSessions := session.NewManager(st, ownerRunner(func(_ context.Context, turn engine.Turn) (<-chan engine.Event, error) {
		if turn.Prompt == "first" {
			close(started)
			return first, nil
		}
		return nil, &engine.UnavailableError{ResetAt: time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)}
	}), session.Options{})
	old := New(oldSessions, nil, NewOldDaemons(filepath.Join(dir, "old")), Options{OldDir: filepath.Join(dir, "old")})
	private, err := old.servePrivate()
	if err != nil {
		t.Fatal(err)
	}
	defer private.close()
	owners := NewOldDaemons(filepath.Join(dir, "old"))
	owners.load(ctx)
	local := session.NewManager(db, newGateRunner("replacement"), session.Options{Elsewhere: owners})
	replacement := New(local, nil, owners, Options{})
	t.Cleanup(func() {
		for _, ch := range []chan struct{}{st.releaseFinish, st.releaseClaim} {
			select {
			case <-ch:
			default:
				close(ch)
			}
		}
		for _, ch := range []chan engine.Event{first} {
			select {
			case <-ch:
			default:
				close(ch)
			}
		}
		cancel()
		if err := local.Stop(context.Background()); err != nil {
			t.Error(err)
		}
		if err := oldSessions.Stop(context.Background()); err != nil {
			t.Error(err)
		}
	})
	res, err := old.SendMessage(ctx, api.MessageRequest{Dir: t.TempDir(), Text: "first"})
	if err != nil {
		t.Fatal(err)
	}
	ownerBarrier(t, started)
	if _, err := old.SendMessage(ctx, api.MessageRequest{SessionID: res.SessionID, Text: "old queued"}); err != nil {
		t.Fatal(err)
	}
	close(first)
	ownerBarrier(t, st.finishing) // stored idle, old owner's queue still pending
	check := func(status api.Status, queued []string) {
		t.Helper()
		s, err := replacement.GetSession(ctx, res.SessionID)
		if err != nil || s.Status != status || !reflect.DeepEqual(s.Queued, queued) {
			t.Errorf("replacement snapshot = %+v, %v; want %s %v", s, err, status, queued)
		}
		page, err := replacement.ListSessions(ctx, api.SessionFilter{})
		if err != nil || len(page.Items) != 1 || page.Items[0].Status != status || !reflect.DeepEqual(page.Items[0].Queued, queued) {
			t.Errorf("replacement list = %+v, %v; want %s %v", page, err, status, queued)
		}

	}
	check(api.StatusIdle, []string{"old queued"})
	if _, err := replacement.SendMessage(ctx, api.MessageRequest{SessionID: res.SessionID, Text: "local queued"}); err != nil {
		t.Fatal(err)
	}
	check(api.StatusIdle, []string{"old queued", "local queued"})
	close(st.releaseFinish)
	ownerBarrier(t, st.claimed) // consumed queue, not yet saved working
	check(api.StatusWorking, []string{"local queued"})
	close(st.releaseClaim)
	ownerBarrier(t, st.paused)
	check(api.StatusPaused, []string{"local queued"})
	// A live process with an unavailable private socket is not proof of idle.
	if err := private.srv.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = replacement.GetSession(ctx, res.SessionID)
	if !api.HasCode(err, api.CodeUnavailable) {
		t.Errorf("unresolved owner = %v, want unavailable", err)
	}
	if !owners.IsBusy(ctx, res.SessionID) {
		t.Error("unavailable live owner released ownership")
	}
	// Loading an already-unreachable but live owner must not erase it either.
	reloaded := NewOldDaemons(filepath.Join(dir, "old"))
	reloaded.load(ctx)
	_, _, err = reloaded.GetSession(ctx, res.SessionID)
	if !api.HasCode(err, api.CodeUnavailable) || !reloaded.IsBusy(ctx, res.SessionID) {
		t.Errorf("reloaded unresolved owner = %v, want unavailable and busy", err)
	}
}
