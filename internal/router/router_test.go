package router

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/viminizer/germes/internal/api"
	"github.com/viminizer/germes/internal/config"
	"github.com/viminizer/germes/internal/engine"
)

var now = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

// fakeEngine either hits its limit or answers. It records the turns it got.
type fakeEngine struct {
	name    string
	limited bool
	usage   *engine.Usage
	turns   []engine.Turn
}

func (f *fakeEngine) Name() string { return f.name }

func (f *fakeEngine) Run(ctx context.Context, t engine.Turn) (<-chan engine.Event, error) {
	f.turns = append(f.turns, t)
	out := make(chan engine.Event, 4)
	if f.usage != nil {
		out <- engine.Event{Kind: engine.KindUsage, Usage: f.usage}
	}
	if f.limited {
		out <- engine.Event{Kind: engine.KindLimitHit, ResetAt: now.Add(2 * time.Hour)}
	} else {
		out <- engine.Event{Kind: engine.KindText, Text: "hi from " + f.name}
		out <- engine.Event{Kind: engine.KindDone, Engine: f.name}
	}
	close(out)
	return out, nil
}

type fakeStore struct {
	limits map[string]time.Time
	usage  []engine.Usage
}

func (s *fakeStore) ListLimits(context.Context) (map[string]time.Time, error) { return s.limits, nil }
func (s *fakeStore) UpdateLimit(_ context.Context, n string, u time.Time) error {
	s.limits[n] = u
	return nil
}
func (s *fakeStore) ListUsage(context.Context) ([]engine.Usage, error) { return s.usage, nil }
func (s *fakeStore) UpdateUsage(_ context.Context, u engine.Usage) error {
	s.usage = append(s.usage, u)
	return nil
}
func (s *fakeStore) ListEvents(context.Context, api.EventFilter) (api.Page[api.Event], error) {
	return api.Page[api.Event]{Items: []api.Event{{Seq: 1, Kind: api.KindUserMessage, Text: "build the thing"}}}, nil
}

func newRouter(t *testing.T, st *fakeStore, cfg []config.Engine, engines ...engine.Engine) *Router {
	t.Helper()
	r, err := New(engines, cfg, st, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func drain(t *testing.T, ch <-chan engine.Event) []engine.Event {
	t.Helper()
	var evs []engine.Event
	for ev := range ch {
		evs = append(evs, ev)
	}
	return evs
}

func TestLimitMovesTurnToNextEngineWithHandoff(t *testing.T) {
	st := &fakeStore{limits: map[string]time.Time{}}
	codex := &fakeEngine{name: "codex-api", limited: true}
	claude := &fakeEngine{name: "claude-cli"}
	r := newRouter(t, st, nil, codex, claude)

	ch, err := r.Run(context.Background(), engine.Turn{SessionID: "s", Dir: t.TempDir(), Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	evs := drain(t, ch)
	var kinds []engine.Kind
	for _, ev := range evs {
		kinds = append(kinds, ev.Kind)
	}
	want := []engine.Kind{engine.KindLimitHit, engine.KindSwitched, engine.KindText, engine.KindDone}
	if len(kinds) != len(want) {
		t.Fatalf("events %v, want %v", kinds, want)
	}
	if evs[1].Engine != "claude-cli" {
		t.Fatalf("switched to %q", evs[1].Engine)
	}
	if extra := claude.turns[0].Extra; !strings.Contains(extra, "Handoff from codex-api") || !strings.Contains(extra, "build the thing") {
		t.Fatalf("no handoff for claude: %q", extra)
	}
	if !st.limits["codex-api"].Equal(now.Add(2 * time.Hour)) {
		t.Fatalf("limit not saved: %v", st.limits)
	}

	// The next turn skips codex while it is limited, and a new router
	// (daemon restart) remembers the limit.
	r2 := newRouter(t, st, nil, codex, claude)
	drain(t, runTurn(t, r2, engine.Turn{SessionID: "s", LastEngine: "claude-cli"}))
	if len(codex.turns) != 1 {
		t.Fatalf("limited codex was tried again: %d turns", len(codex.turns))
	}
}

func TestReserveSkipsEngineAndUsageIsLearned(t *testing.T) {
	st := &fakeStore{limits: map[string]time.Time{}}
	codex := &fakeEngine{name: "codex-api", usage: &engine.Usage{Engine: "codex-api", Window: "seven_day", UsedPercent: 83, ResetAt: now.Add(48 * time.Hour)}}
	claude := &fakeEngine{name: "claude-cli"}
	cfg := []config.Engine{{Name: "codex-api", Reserve: map[string]float64{"seven_day": 20}}}
	r := newRouter(t, st, cfg, codex, claude)

	// Turn 1: usage unknown, codex runs and reports 83% used.
	drain(t, runTurn(t, r, engine.Turn{SessionID: "s"}))
	// Turn 2: 17% left <= 20% kept, so claude runs, with a switch notice.
	evs := drain(t, runTurn(t, r, engine.Turn{SessionID: "s", LastEngine: "codex-api"}))
	if len(claude.turns) != 1 || evs[0].Kind != engine.KindSwitched || !strings.Contains(evs[0].Text, "reserved") {
		t.Fatalf("reserve not applied: claude turns %d, first event %+v", len(claude.turns), evs[0])
	}

	// reserve_off lets codex run to its limit.
	cfg[0].ReserveOff = true
	r = newRouter(t, st, cfg, codex, claude)
	drain(t, runTurn(t, r, engine.Turn{SessionID: "s"}))
	if len(codex.turns) != 2 {
		t.Fatalf("reserve_off ignored: codex turns %d", len(codex.turns))
	}
}

func TestAllBlockedIsUnavailable(t *testing.T) {
	st := &fakeStore{limits: map[string]time.Time{"a": now.Add(time.Hour), "b": now.Add(3 * time.Hour)}}
	r := newRouter(t, st, nil, &fakeEngine{name: "a"}, &fakeEngine{name: "b"})
	_, err := r.Run(context.Background(), engine.Turn{SessionID: "s"})
	var unavail *engine.UnavailableError
	if !errors.As(err, &unavail) || !unavail.ResetAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("err = %v, want unavailable until the earliest reset", err)
	}
}

func runTurn(t *testing.T, r *Router, turn engine.Turn) <-chan engine.Event {
	t.Helper()
	ch, err := r.Run(context.Background(), turn)
	if err != nil {
		t.Fatal(err)
	}
	return ch
}
