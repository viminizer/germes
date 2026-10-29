package loop

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/viminizer/germes/internal/api"
	"github.com/viminizer/germes/internal/engine"
	"github.com/viminizer/germes/internal/llm"
	"github.com/viminizer/germes/internal/prompt"
	"github.com/viminizer/germes/internal/tool"
)

// fakeModel asks for the echo tool once, then answers with text.
type fakeModel struct{ requests []llm.Request }

func (f *fakeModel) Send(ctx context.Context, r llm.Request) (<-chan llm.Chunk, error) {
	f.requests = append(f.requests, r)
	out := make(chan llm.Chunk, 4)
	if len(f.requests) == 1 {
		out <- llm.Chunk{Kind: llm.KindToolCall, ToolCall: &llm.ToolCall{ID: "c1", Name: "echo", Args: json.RawMessage(`{}`)}}
	} else {
		out <- llm.Chunk{Kind: llm.KindText, Text: "done"}
	}
	out <- llm.Chunk{Kind: llm.KindDone}
	close(out)
	return out, nil
}

type fakeHistory []api.Event

func (h fakeHistory) ListEvents(ctx context.Context, f api.EventFilter) (api.Page[api.Event], error) {
	return api.Page[api.Event]{Items: h}, nil
}

func TestLoopRunsToolsThenAnswers(t *testing.T) {
	reg, err := tool.NewRegistry([]tool.Tool{{Name: "echo", Run: func(ctx context.Context, c tool.Call) (string, error) { return "pong", nil }}},
		map[string]tool.Toolset{"core": {Tools: []string{"echo"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	model := &fakeModel{}
	history := fakeHistory{{Seq: 1, Kind: api.KindUserMessage, Text: "hi"}}
	l := New(model, reg, prompt.NewBuilder(t.TempDir()), history, Options{Name: "fake", Toolsets: []string{"core"}})
	events, err := l.Run(context.Background(), engine.Turn{SessionID: "s", Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var kinds []engine.Kind
	for ev := range events {
		kinds = append(kinds, ev.Kind)
	}
	want := []engine.Kind{engine.KindToolCall, engine.KindToolResult, engine.KindText, engine.KindDone}
	if len(kinds) != len(want) {
		t.Fatalf("events %v, want %v", kinds, want)
	}
	second := model.requests[1].Messages
	if last := second[len(second)-1]; last.Role != llm.RoleTool || last.Text != "pong" {
		t.Fatalf("tool result not sent back: %+v", last)
	}
}

func TestHistoryClosesInterruptedCalls(t *testing.T) {
	msgs := messages([]api.Event{
		{Kind: api.KindUserMessage, Text: "a"},
		{Kind: api.KindToolCall, Tool: &api.ToolCall{ID: "x", Name: "Bash"}},
		{Kind: api.KindError, Text: "stopped"},
		{Kind: api.KindUserMessage, Text: "b"},
	})
	roles := []llm.Role{llm.RoleUser, llm.RoleAssistant, llm.RoleTool, llm.RoleUser}
	if len(msgs) != len(roles) {
		t.Fatalf("got %d messages: %+v", len(msgs), msgs)
	}
	for i, r := range roles {
		if msgs[i].Role != r {
			t.Fatalf("message %d is %s, want %s", i, msgs[i].Role, r)
		}
	}
}
