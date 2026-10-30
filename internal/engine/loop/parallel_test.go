package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/viminizer/germes/internal/api"
	"github.com/viminizer/germes/internal/engine"
	"github.com/viminizer/germes/internal/llm"
	"github.com/viminizer/germes/internal/prompt"
	"github.com/viminizer/germes/internal/tool"
	"github.com/viminizer/germes/internal/tools/files"
)

// batchModel asks for calls in its first answer, then answers.
type batchModel struct{ calls []llm.ToolCall }

func (m *batchModel) Send(ctx context.Context, r llm.Request) (<-chan llm.Chunk, error) {
	out := make(chan llm.Chunk, len(m.calls)+2)
	if len(r.Messages) == 1 {
		for _, c := range m.calls {
			out <- llm.Chunk{Kind: llm.KindToolCall, ToolCall: &c}
		}
	} else {
		out <- llm.Chunk{Kind: llm.KindText, Text: "done"}
	}
	out <- llm.Chunk{Kind: llm.KindDone}
	close(out)
	return out, nil
}

func call(id, name, path string) llm.ToolCall {
	args, _ := json.Marshal(map[string]string{"file_path": path, "command": path})
	return llm.ToolCall{ID: id, Name: name, Args: args}
}

// span is when one tool call ran.
type span struct{ start, end time.Time }

// timedTools are a slow Read and a Bash that record when each call ran,
// and how many ran at once at most.
type timedTools struct {
	mu            sync.Mutex
	spans         map[string]span
	running, most int
}

func (tt *timedTools) run(wait time.Duration) func(ctx context.Context, c tool.Call) (string, error) {
	return func(ctx context.Context, c tool.Call) (string, error) {
		var a struct {
			Path string `json:"file_path"`
		}
		json.Unmarshal(c.Args, &a)
		tt.mu.Lock()
		start := time.Now()
		tt.running++
		tt.most = max(tt.most, tt.running)
		tt.mu.Unlock()
		select {
		case <-time.After(wait):
		case <-ctx.Done():
		}
		tt.mu.Lock()
		tt.running--
		tt.spans[a.Path] = span{start, time.Now()}
		tt.mu.Unlock()
		return "content of " + a.Path, nil
	}
}

// newBatchLoop is a loop whose model asks for calls in one step.
func newBatchLoop(t *testing.T, reg *tool.Registry, calls []llm.ToolCall) *Loop {
	return New(&batchModel{calls: calls}, reg, prompt.NewBuilder(t.TempDir(), noMemory{}, noSkills{}),
		fakeHistory{{Seq: 1, Kind: api.KindUserMessage, Text: "read them"}}, Options{Name: "fake", Toolsets: []string{"core"}})
}

func runBatch(t *testing.T, ctx context.Context, calls []llm.ToolCall, blocked []string) (*timedTools, []engine.Event, time.Duration) {
	t.Helper()
	tt := &timedTools{spans: map[string]span{}}
	reg, err := tool.NewRegistry([]tool.Tool{
		{Name: "Read", MatchArg: "file_path", Run: tt.run(200 * time.Millisecond)},
		{Name: "Bash", MatchArg: "command", Run: tt.run(50 * time.Millisecond)},
	}, map[string]tool.Toolset{"core": {Tools: []string{"Read", "Bash"}}}, blocked, "")
	if err != nil {
		t.Fatal(err)
	}
	l := newBatchLoop(t, reg, calls)
	start := time.Now()
	events, err := l.Run(ctx, engine.Turn{SessionID: "s", Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var results []engine.Event
	for ev := range events {
		if ev.Kind == engine.KindToolResult {
			results = append(results, ev)
		}
	}
	return tt, results, time.Since(start)
}

func TestReadsInOneStepRunAtOnceAndBashWaits(t *testing.T) {
	calls := []llm.ToolCall{call("1", "Read", "a"), call("2", "Read", "b"), call("3", "Read", "c"), call("4", "Bash", "x"), call("5", "Read", "d")}
	tt, results, took := runBatch(t, context.Background(), calls, nil)
	// a, b and c at once (200 ms), then x (50 ms), then d (200 ms); one by one it is 850 ms.
	if took > 650*time.Millisecond {
		t.Errorf("the step took %s: the reads did not overlap", took)
	}
	// Bash is a barrier: it starts after the reads before it end, and the
	// read after it starts after it ends.
	x := tt.spans["x"]
	for _, p := range []string{"a", "b", "c"} {
		if tt.spans[p].end.After(x.start) {
			t.Errorf("Bash started before read %s ended", p)
		}
	}
	if tt.spans["d"].start.Before(x.end) {
		t.Error("the read after Bash started before Bash ended")
	}
	// Results in the order of the calls, whatever order they finished in.
	if len(results) != len(calls) {
		t.Fatalf("%d results for %d calls", len(results), len(calls))
	}
	for i, r := range results {
		if r.Tool.ID != calls[i].ID {
			t.Fatalf("result %d is for call %s, want %s", i, r.Tool.ID, calls[i].ID)
		}
	}
}

func TestAtMostFourReadsRunAtOnce(t *testing.T) {
	var calls []llm.ToolCall
	for i := range 7 {
		calls = append(calls, call(fmt.Sprint(i), "Read", fmt.Sprint("f", i)))
	}
	tt, results, _ := runBatch(t, context.Background(), calls, nil)
	if tt.most != 4 || len(results) != 7 {
		t.Fatalf("at most %d at once (want 4), %d results", tt.most, len(results))
	}
}

func TestBlockedReadInABatchIsRefusedAlone(t *testing.T) {
	calls := []llm.ToolCall{call("1", "Read", "a"), call("2", "Read", ".env"), call("3", "Read", "b")}
	tt, results, _ := runBatch(t, context.Background(), calls, []string{"Read(.env)"})
	if len(results) != 3 || !results[1].Tool.IsError || results[0].Tool.IsError || results[2].Tool.IsError {
		t.Fatalf("results %+v", results)
	}
	if _, ran := tt.spans[".env"]; ran {
		t.Fatal("the blocked read ran")
	}
}

func TestStoppingStopsTheWholeBatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	calls := []llm.ToolCall{call("1", "Read", "a"), call("2", "Read", "b"), call("3", "Read", "c")}
	_, results, took := runBatch(t, ctx, calls, nil)
	if took > 150*time.Millisecond || len(results) != 0 {
		t.Fatalf("took %s, %d results; want a quick stop and no results", took, len(results))
	}
}

// The real file tools in one step: run with -race, this shows they are safe
// to run at once (the read log Edit checks has its own lock).
func TestRealReadsInOneStep(t *testing.T) {
	dir := t.TempDir()
	var calls []llm.ToolCall
	for i := range 6 {
		path := filepath.Join(dir, fmt.Sprint("f", i, ".txt"))
		os.WriteFile(path, []byte(fmt.Sprint("line ", i)), 0o600)
		calls = append(calls, call(fmt.Sprint(i), "Read", path))
	}
	calls = append(calls, llm.ToolCall{ID: "g", Name: "Grep", Args: json.RawMessage(`{"pattern":"line"}`)})
	reg, err := tool.NewRegistry(files.New(), map[string]tool.Toolset{"core": {Tools: []string{"Read", "Grep"}}}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	l := newBatchLoop(t, reg, calls)
	events, err := l.Run(context.Background(), engine.Turn{SessionID: "s", Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	i := 0
	for ev := range events {
		if ev.Kind != engine.KindToolResult {
			continue
		}
		if ev.Tool.ID != calls[i].ID || ev.Tool.IsError {
			t.Fatalf("result %d: %+v", i, ev.Tool)
		}
		i++
	}
	if i != len(calls) {
		t.Fatalf("%d results for %d calls", i, len(calls))
	}
}
