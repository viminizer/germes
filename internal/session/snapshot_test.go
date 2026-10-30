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

// snapshotStore holds the next turn after queue consumption but before its
// working status is saved. A marked lookup can also retain an older row.
type snapshotStore struct {
	Store
	claimed       chan struct{}
	release       chan struct{}
	read          chan struct{}
	returnRead    chan struct{}
	paused        chan struct{}
	finished      chan struct{}
	releaseFinish chan struct{}
}

type snapshotReadKey struct{}

func (s *snapshotStore) CreateEvent(ctx context.Context, ev api.Event) error {
	if ev.Kind == api.KindUserMessage && ev.Text == "b" {
		close(s.claimed)
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if ev.Kind == api.KindTurnDone && ev.Seq == 2 && s.finished != nil {
		close(s.finished)
		<-s.releaseFinish
	}
	return s.Store.CreateEvent(ctx, ev)
}

func (s *snapshotStore) GetSession(ctx context.Context, id string) (api.Session, error) {
	row, err := s.Store.GetSession(ctx, id)
	if ctx.Value(snapshotReadKey{}) != nil {
		close(s.read)
		<-s.returnRead
	}
	return row, err
}

func (s *snapshotStore) ListSessions(ctx context.Context, f api.SessionFilter) (api.Page[api.Session], error) {
	page, err := s.Store.ListSessions(ctx, f)
	if ctx.Value(snapshotReadKey{}) != nil {
		close(s.read)
		<-s.returnRead
	}
	return page, err
}

func (s *snapshotStore) UpdateSession(ctx context.Context, row api.Session) error {
	err := s.Store.UpdateSession(ctx, row)
	if err == nil && row.Status == api.StatusPaused && s.paused != nil {
		close(s.paused)
	}
	return err
}

type snapshotRunner func(context.Context, engine.Turn) (<-chan engine.Event, error)

func (r snapshotRunner) Run(ctx context.Context, turn engine.Turn) (<-chan engine.Event, error) {
	return r(ctx, turn)
}

func awaitSnapshot(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("snapshot barrier was not reached")
	}
}

// Completion consumers must not forget queued work when it leaves the queue,
// even if their database read began before the worker claimed that work.
func TestSessionSnapshotRetainsClaimedQueue(t *testing.T) {
	for _, lookup := range []string{"get", "list"} {
		for _, stale := range []bool{false, true} {
			t.Run(lookup+map[bool]string{false: "/claimed", true: "/older-row"}[stale], func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				db, err := store.New(filepath.Join(t.TempDir(), "germes.db"))
				if err != nil {
					t.Fatal(err)
				}
				st := &snapshotStore{Store: db, claimed: make(chan struct{}), release: make(chan struct{}), read: make(chan struct{}), returnRead: make(chan struct{}), finished: make(chan struct{}), releaseFinish: make(chan struct{})}
				first, second := make(chan engine.Event), make(chan engine.Event)
				started := make(chan struct{})
				m := NewManager(st, snapshotRunner(func(_ context.Context, turn engine.Turn) (<-chan engine.Event, error) {
					if turn.Prompt == "a" {
						close(started)
						return first, nil
					}
					return second, nil
				}), Options{})
				// Release every barrier before joining, including on assertion failure.
				t.Cleanup(func() {
					select {
					case <-st.releaseFinish:
					default:
						close(st.releaseFinish)
					}
					select {
					case <-st.release:
					default:
						close(st.release)
					}
					select {
					case <-st.returnRead:
					default:
						close(st.returnRead)
					}
					select {
					case <-first:
					default:
						close(first)
					}
					select {
					case <-second:
					default:
						close(second)
					}
					cancel()
					if err := m.Stop(context.Background()); err != nil {
						t.Error(err)
					}
					if err := db.Close(); err != nil {
						t.Error(err)
					}
				})
				res, err := m.SendMessage(ctx, api.MessageRequest{Dir: t.TempDir(), Text: "a"})
				if err != nil {
					t.Fatal(err)
				}
				awaitSnapshot(t, started)
				watch, err := m.Watch(ctx, api.WatchFilter{SessionID: res.SessionID})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := m.SendMessage(ctx, api.MessageRequest{SessionID: res.SessionID, Text: "b"}); err != nil {
					t.Fatal(err)
				}
				if s := mustSession(t, m, res.SessionID); len(s.Queued) != 1 || s.Status != api.StatusWorking {
					t.Fatalf("waiting snapshot = %+v", s)
				}

				result := make(chan api.Session, 1)
				read := func(readCtx context.Context) {
					if lookup == "get" {
						s, err := m.GetSession(readCtx, res.SessionID)
						if err != nil {
							t.Error(err)
						}
						result <- s
					} else {
						page, err := m.ListSessions(readCtx, api.SessionFilter{})
						if err != nil {
							t.Error(err)
						}
						if len(page.Items) != 1 {
							t.Errorf("sessions = %v", page.Items)
							result <- api.Session{}
							return
						}
						result <- page.Items[0]
					}
				}
				close(first)
				awaitSnapshot(t, st.finished)
				if stale {
					go read(context.WithValue(ctx, snapshotReadKey{}, true))
					awaitSnapshot(t, st.read)
				}
				close(st.releaseFinish)
				next(t, watch, api.KindTurnDone)
				awaitSnapshot(t, st.claimed)
				if stale {
					close(st.returnRead)
				} else {
					read(ctx)
				}
				s := <-result
				if s.Status != api.StatusWorking || len(s.Queued) != 0 {
					t.Errorf("claimed snapshot = %+v, want working with consumed queue", s)
				}
				close(st.release)
				close(second)
				next(t, watch, api.KindTurnDone)
				if s := mustSession(t, m, res.SessionID); s.Status != api.StatusIdle || len(s.Queued) != 0 {
					t.Errorf("completed snapshot = %+v", s)
				}
			})
		}
	}
}

func TestSessionSnapshotPreservesPausedTurn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db, err := store.New(filepath.Join(t.TempDir(), "germes.db"))
	if err != nil {
		t.Fatal(err)
	}
	st := &snapshotStore{Store: db, paused: make(chan struct{}), read: make(chan struct{}), returnRead: make(chan struct{})}
	m := NewManager(st, snapshotRunner(func(context.Context, engine.Turn) (<-chan engine.Event, error) {
		return nil, &engine.UnavailableError{ResetAt: time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)}
	}), Options{})
	t.Cleanup(func() {
		select {
		case <-st.returnRead:
		default:
			close(st.returnRead)
		}
		cancel()
		if err := m.Stop(context.Background()); err != nil {
			t.Error(err)
		}
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	res, err := m.SendMessage(ctx, api.MessageRequest{Dir: t.TempDir(), Text: "a"})
	if err != nil {
		t.Fatal(err)
	}
	watch, err := m.Watch(ctx, api.WatchFilter{SessionID: res.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	awaitSnapshot(t, st.paused)
	if s := mustSession(t, m, res.SessionID); s.Status != api.StatusPaused {
		t.Errorf("paused snapshot = %+v", s)
	}
	page, err := m.ListSessions(ctx, api.SessionFilter{})
	if err != nil || len(page.Items) != 1 || page.Items[0].Status != api.StatusPaused {
		t.Errorf("paused list = %+v, %v", page, err)
	}
	older := make(chan api.Session, 1)
	go func() {
		s, err := m.GetSession(context.WithValue(ctx, snapshotReadKey{}, true), res.SessionID)
		if err != nil {
			t.Error(err)
		}
		older <- s
	}()
	awaitSnapshot(t, st.read)
	if err := m.StopSession(ctx, api.StopRequest{ID: res.SessionID}); err != nil {
		t.Fatal(err)
	}
	next(t, watch, api.KindTurnDone)
	close(st.returnRead)
	if s := <-older; s.Status != api.StatusIdle || len(s.Queued) != 0 {
		t.Errorf("completed snapshot paired with an older paused row = %+v", s)
	}
	if s := mustSession(t, m, res.SessionID); s.Status != api.StatusIdle || len(s.Queued) != 0 {
		t.Errorf("stopped snapshot = %+v", s)
	}
}

// handoverElsewhere models the existing daemon GetDaemon.Busy contract.
type handoverElsewhere struct {
	old     *Manager
	checked chan bool
}

func (a handoverElsewhere) IsBusy(_ context.Context, id string) bool {
	busy := false
	for _, owner := range a.old.ListBusy() {
		busy = busy || owner == id
	}
	select {
	case a.checked <- busy:
	default:
	}
	return busy
}

func (a handoverElsewhere) GetSession(ctx context.Context, id string) (api.Session, bool, error) {
	if !a.IsBusy(ctx, id) {
		return api.Session{}, false, nil
	}
	s, err := a.old.GetSession(ctx, id)
	return s, true, err
}

func TestHandoverKeepsOwnershipThroughTurnDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db, err := store.New(filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st := &snapshotStore{Store: db, finished: make(chan struct{}), releaseFinish: make(chan struct{})}
	first := make(chan engine.Event)
	close(first)
	old := NewManager(st, snapshotRunner(func(context.Context, engine.Turn) (<-chan engine.Event, error) { return first, nil }), Options{})
	defer func() {
		select {
		case <-st.releaseFinish:
		default:
			close(st.releaseFinish)
		}
		if err := old.Stop(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	res, err := old.SendMessage(ctx, api.MessageRequest{Dir: t.TempDir(), Text: "a"})
	if err != nil {
		t.Fatal(err)
	}
	awaitSnapshot(t, st.finished)
	if s := mustSession(t, old, res.SessionID); s.Status != api.StatusIdle || len(s.Queued) != 0 {
		t.Errorf("finalizing public snapshot = %+v, want idle with no queue", s)
	}
	owner := handoverElsewhere{old: old, checked: make(chan bool, 1)}
	if !owner.IsBusy(ctx, res.SessionID) {
		t.Error("old daemon advertises not busy before its final turn_done has been persisted")
	}
	<-owner.checked
	started := make(chan struct{})
	second := make(chan engine.Event)
	newer := NewManager(db, snapshotRunner(func(context.Context, engine.Turn) (<-chan engine.Event, error) { close(started); return second, nil }), Options{Elsewhere: owner})
	defer func() {
		select {
		case <-second:
		default:
			close(second)
		}
		if err := newer.Stop(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	if _, err := newer.SendMessage(ctx, api.MessageRequest{SessionID: res.SessionID, Text: "continuation"}); err != nil {
		t.Fatal(err)
	}
	select {
	case busy := <-owner.checked:
		if !busy {
			t.Error("replacement can reuse the old turn_done sequence before old owner finished")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("replacement did not check the old owner")
	}
	watch, err := newer.Watch(ctx, api.WatchFilter{SessionID: res.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	close(st.releaseFinish)
	awaitSnapshot(t, started)
	close(second)
	for ev := next(t, watch, api.KindTurnDone); ev.Seq < 4; ev = next(t, watch, api.KindTurnDone) {
	}
	page, err := db.ListEvents(ctx, api.EventFilter{SessionID: res.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	want := []api.EventKind{api.KindUserMessage, api.KindTurnDone, api.KindUserMessage, api.KindTurnDone}
	if len(page.Items) != len(want) {
		t.Fatalf("persisted handover events = %+v", page.Items)
	}
	for i, ev := range page.Items {
		if ev.Seq != int64(i+1) || ev.Kind != want[i] {
			t.Errorf("persisted event %d = %+v, want seq %d kind %s", i, ev, i+1, want[i])
		}
	}
}
