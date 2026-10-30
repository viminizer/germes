package session

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/viminizer/germes/internal/api"
	"github.com/viminizer/germes/internal/engine/loop"
	"github.com/viminizer/germes/internal/llm"
	"github.com/viminizer/germes/internal/memory"
	"github.com/viminizer/germes/internal/prompt"
	"github.com/viminizer/germes/internal/skill"
	"github.com/viminizer/germes/internal/store"
	"github.com/viminizer/germes/internal/tool"
)

// failingStore is a real store whose saves of one event kind fail, like a
// full disk.
type failingStore struct {
	*store.Store
	fail api.EventKind
}

func (s failingStore) CreateEvent(ctx context.Context, e api.Event) error {
	if e.Kind == s.fail || s.fail == "*" {
		return errors.New("disk full")
	}
	return s.Store.CreateEvent(ctx, e)
}

// touchModel asks for the touch tool once, then answers.
type touchModel struct{ n int32 }

func (m *touchModel) Send(ctx context.Context, r llm.Request) (<-chan llm.Chunk, error) {
	out := make(chan llm.Chunk, 3)
	if atomic.AddInt32(&m.n, 1) == 1 {
		out <- llm.Chunk{Kind: llm.KindToolCall, ToolCall: &llm.ToolCall{ID: "c1", Name: "touch", Args: json.RawMessage(`{}`)}}
	} else {
		out <- llm.Chunk{Kind: llm.KindText, Text: "done"}
	}
	out <- llm.Chunk{Kind: llm.KindDone}
	close(out)
	return out, nil
}

type noMemory struct{}

func (noMemory) Load(context.Context) (memory.Snapshot, error) { return memory.Snapshot{}, nil }

type noSkills struct{}

func (noSkills) List(context.Context) ([]skill.Summary, error) { return nil, nil }

// A turn on the Germes loop whose saves fail (issue #25): no tool runs
// before its call is saved, a tool that ran is never run again, nothing
// unsaved is shown as saved, the user is told, and the turn still ends for
// watchers even when nothing can be saved.
func TestFailedSavesStopTheTurnSafely(t *testing.T) {
	for _, tc := range []struct {
		fail     api.EventKind // "*": every save fails
		ran      int32         // tool runs
		requests int32         // model calls
		told     bool
	}{
		{fail: api.KindToolCall, ran: 0, requests: 1, told: true},
		{fail: api.KindToolResult, ran: 1, requests: 1, told: true}, // no next model call, no second run
		{fail: "*", ran: 0, requests: 0, told: true},                // the database is down
		{fail: "", ran: 1, requests: 2, told: false},                // saves work: the turn goes on
	} {
		ran, model, seen := runSavingTurn(t, tc.fail)
		if requests := atomic.LoadInt32(&model.n); ran != tc.ran || requests != tc.requests {
			t.Errorf("fail %q: the tool ran %d times and the model got %d calls; want %d and %d", tc.fail, ran, atomic.LoadInt32(&model.n), tc.ran, tc.requests)
		}
		told, ended := false, false
		for _, ev := range seen {
			if ev.Kind == tc.fail && ev.Kind != "" {
				t.Errorf("fail %q: the unsaved event was shown: %+v", tc.fail, ev)
			}
			told = told || (ev.Kind == api.KindError && ev.Seq == 0 && strings.Contains(ev.Text, "could not save"))
			ended = ended || ev.Kind == api.KindTurnDone
		}
		if told != tc.told || !ended {
			t.Errorf("fail %q: told %v, turn ended %v: %+v", tc.fail, told, ended, seen)
		}
	}
}

// runSavingTurn runs one turn on the Germes loop with a model that calls the
// touch tool once, on a store whose saves of fail fail. It returns how
// often the tool ran, the model, and the events a watcher saw.
func runSavingTurn(t *testing.T, fail api.EventKind) (int32, *touchModel, []api.Event) {
	t.Helper()
	ctx := context.Background()
	st, err := store.New(filepath.Join(t.TempDir(), "germes.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var ran atomic.Int32
	reg, err := tool.NewRegistry([]tool.Tool{{Name: "touch", Run: func(context.Context, tool.Call) (string, error) {
		ran.Add(1) // a tool that changes something
		return "touched", nil
	}}}, map[string]tool.Toolset{"core": {Tools: []string{"touch"}}}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	model := &touchModel{}
	fs := failingStore{Store: st, fail: fail}
	runner := loop.New(model, reg, prompt.NewBuilder(t.TempDir(), noMemory{}, noSkills{}), fs, loop.Options{Name: "fake", Toolsets: []string{"core"}})
	m := NewManager(fs, runner, Options{})
	defer m.Stop(ctx)
	res, err := m.SendMessage(ctx, api.MessageRequest{Dir: t.TempDir(), Text: "touch it"})
	if err != nil {
		t.Fatal(err)
	}
	ch, _ := m.Watch(ctx, api.WatchFilter{SessionID: res.SessionID})
	var seen []api.Event
	for ev := range ch {
		seen = append(seen, ev)
		if ev.Kind == api.KindTurnDone {
			break
		}
	}
	return ran.Load(), model, seen
}
