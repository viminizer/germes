package loop

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/viminizer/germes/internal/api"
	"github.com/viminizer/germes/internal/engine"
	"github.com/viminizer/germes/internal/llm"
	"github.com/viminizer/germes/internal/memory"
	"github.com/viminizer/germes/internal/prompt"
	"github.com/viminizer/germes/internal/skill"
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
	reg := newEchoRegistry(t)
	model := &fakeModel{}
	history := fakeHistory{{Seq: 1, Kind: api.KindUserMessage, Text: "hi"}}
	l := New(model, reg, prompt.NewBuilder(t.TempDir(), noMemory{}, noSkills{}), history, Options{Name: "fake", Toolsets: []string{"core"}})
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

type noMemory struct{}

func (noMemory) Load(context.Context) (memory.Snapshot, error) { return memory.Snapshot{}, nil }

type noSkills struct{}

func (noSkills) List(context.Context) ([]skill.Summary, error) { return nil, nil }

// summaryModel answers every request with one fixed text.
type summaryModel struct{ calls int }

func (m *summaryModel) Send(ctx context.Context, r llm.Request) (<-chan llm.Chunk, error) {
	m.calls++
	out := make(chan llm.Chunk, 2)
	out <- llm.Chunk{Kind: llm.KindText, Text: "SUMMARY"}
	out <- llm.Chunk{Kind: llm.KindDone}
	close(out)
	return out, nil
}

func TestCompactionKeepsTailAndLoadsSummary(t *testing.T) {
	big := strings.Repeat("x", 2000)
	var events []api.Event
	for i := int64(1); i <= 20; i += 2 {
		events = append(events, api.Event{Seq: i, Kind: api.KindUserMessage, Text: "question " + big},
			api.Event{Seq: i + 1, Kind: api.KindText, Text: "answer " + big})
	}
	model := &summaryModel{}
	l := New(model, nil, nil, nil, Options{ContextWindow: 8000}) // half = 4000 tokens = 16k chars
	summary, keepFrom, ok, err := l.compact(context.Background(), "", events)
	if err != nil || !ok || summary != "SUMMARY" {
		t.Fatalf("compact: %q %v %v", summary, ok, err)
	}
	var keepEvent api.Event
	for _, ev := range events {
		if ev.Seq == keepFrom {
			keepEvent = ev
		}
	}
	if keepEvent.Kind != api.KindUserMessage || keepFrom <= 1 || keepFrom >= 20 {
		t.Fatalf("keepFrom %d (%s) is not a user message in the middle", keepFrom, keepEvent.Kind)
	}

	// The next turn loads the summary, then exactly the kept events.
	events = append(events, api.Event{Seq: 21, Kind: api.KindCompacted, Text: summary, KeepFrom: keepFrom},
		api.Event{Seq: 22, Kind: api.KindUserMessage, Text: "new question"})
	msgs := messages(events)
	if !strings.Contains(msgs[0].Text, "SUMMARY") {
		t.Fatalf("first message is not the summary: %q", msgs[0].Text)
	}
	if want := int(21-keepFrom) + 1 + 1; len(msgs) != want {
		t.Fatalf("got %d messages, want summary + %d kept + new question", len(msgs), want-2)
	}
	if last := msgs[len(msgs)-1]; last.Text != "new question" {
		t.Fatalf("last message %q", last.Text)
	}

	// Small history: no compaction, no model call.
	model.calls = 0
	if _, _, ok, _ := l.compact(context.Background(), "", events[:2]); ok || model.calls != 0 {
		t.Fatalf("compacted a small history")
	}
}

// planOf is a task list with fixed open items.
type planOf string

func (p planOf) GetActive(sessionID string) string { return string(p) }

func TestOpenTasksFollowTheSummary(t *testing.T) {
	reg := newEchoRegistry(t)
	model := &fakeModel{}
	history := fakeHistory{
		{Seq: 1, Kind: api.KindUserMessage, Text: "old"},
		{Seq: 2, Kind: api.KindCompacted, Text: "we fixed the parser", KeepFrom: 3},
		{Seq: 3, Kind: api.KindUserMessage, Text: "go on"},
	}
	l := New(model, reg, prompt.NewBuilder(t.TempDir(), noMemory{}, noSkills{}), history,
		Options{Name: "fake", Toolsets: []string{"core"}, Todos: planOf("- [ ] 2. Write the docs")})
	events, _ := l.Run(context.Background(), engine.Turn{SessionID: "s", Dir: t.TempDir()})
	for range events {
	}
	first := model.requests[0].Messages[0].Text
	if !strings.Contains(first, "we fixed the parser") || !strings.HasSuffix(first, "- [ ] 2. Write the docs") {
		t.Fatalf("first message:\n%s", first)
	}
}

// newEchoRegistry has one tool, echo, which answers pong, in toolset core,
// and a read-only toolset plan with no tools.
func newEchoRegistry(t *testing.T) *tool.Registry {
	t.Helper()
	reg, err := tool.NewRegistry([]tool.Tool{{Name: "echo", Run: func(ctx context.Context, c tool.Call) (string, error) { return "pong", nil }}},
		map[string]tool.Toolset{"core": {Tools: []string{"echo"}}, "plan": {}}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestPlanTurnReadsOnly(t *testing.T) {
	model := &fakeModel{}
	l := New(model, newEchoRegistry(t), prompt.NewBuilder(t.TempDir(), noMemory{}, noSkills{}),
		fakeHistory{{Seq: 1, Kind: api.KindUserMessage, Text: "add dark mode"}},
		Options{Name: "fake", Toolsets: []string{"core"}, PlanToolsets: []string{"plan"}, SubagentToolsets: []string{"core"}})
	events, _ := l.Run(context.Background(), engine.Turn{SessionID: "s", Dir: t.TempDir(), Plan: true})
	for range events {
	}
	r := model.requests[0]
	for _, s := range r.Tools {
		if s.Name == "echo" || s.Name == delegateTool {
			t.Fatalf("a plan turn got %s", s.Name)
		}
	}
	if last := r.Messages[len(r.Messages)-1].Text; !strings.Contains(last, "Plan mode") {
		t.Fatalf("no plan instruction: %q", last)
	}
}

// finalModel answers with a final answer and a tool call in one response.
type finalModel struct{ requests int }

func (f *finalModel) Send(ctx context.Context, r llm.Request) (<-chan llm.Chunk, error) {
	f.requests++
	out := make(chan llm.Chunk, 3)
	out <- llm.Chunk{Kind: llm.KindText, Text: "All done.", Phase: llm.PhaseFinalAnswer}
	out <- llm.Chunk{Kind: llm.KindToolCall, ToolCall: &llm.ToolCall{ID: "c1", Name: "echo", Args: json.RawMessage(`{}`)}}
	out <- llm.Chunk{Kind: llm.KindDone}
	close(out)
	return out, nil
}

func TestFinalAnswerEndsTheTurn(t *testing.T) {
	model := &finalModel{}
	l := New(model, newEchoRegistry(t), prompt.NewBuilder(t.TempDir(), noMemory{}, noSkills{}),
		fakeHistory{{Seq: 1, Kind: api.KindUserMessage, Text: "check it"}}, Options{Name: "fake", Toolsets: []string{"core"}})
	events, _ := l.Run(context.Background(), engine.Turn{SessionID: "s", Dir: t.TempDir()})
	var kinds []engine.Kind
	for ev := range events {
		kinds = append(kinds, ev.Kind)
	}
	// Asked once why it is not done; the same answer again ends the turn.
	if model.requests != 2 || slices.Contains(kinds, engine.KindToolCall) || kinds[len(kinds)-1] != engine.KindDone {
		t.Fatalf("requests %d, events %v; want two requests, no tool run, then done", model.requests, kinds)
	}
}

func TestHistoryRebuildsPhases(t *testing.T) {
	msgs := messages([]api.Event{
		{Kind: api.KindUserMessage, Text: "a"},
		{Kind: api.KindText, Text: "looking"},
		{Kind: api.KindToolCall, Tool: &api.ToolCall{ID: "x", Name: "Read"}},
		{Kind: api.KindToolResult, Tool: &api.ToolCall{ID: "x", Output: "ok"}},
		{Kind: api.KindText, Text: "first part"},
		{Kind: api.KindText, Text: "it works"},
		{Kind: api.KindTurnDone},
	})
	var phases []llm.Phase
	for _, m := range msgs {
		if m.Text != "" && m.Role == llm.RoleAssistant {
			phases = append(phases, m.Phase)
		}
	}
	want := []llm.Phase{llm.PhaseCommentary, llm.PhaseCommentary, llm.PhaseFinalAnswer}
	if !slices.Equal(phases, want) {
		t.Fatalf("phases %v, want %v", phases, want)
	}
}

// contextModel answers once and says how full the context was.
type contextModel struct{}

func (contextModel) Send(ctx context.Context, r llm.Request) (<-chan llm.Chunk, error) {
	out := make(chan llm.Chunk, 3)
	out <- llm.Chunk{Kind: llm.KindText, Text: "done"}
	out <- llm.Chunk{Kind: llm.KindContext, Context: &llm.Context{Model: "gpt-5.5", Tokens: 1234}}
	out <- llm.Chunk{Kind: llm.KindDone}
	close(out)
	return out, nil
}

func TestDoneSaysHowFullTheContextIs(t *testing.T) {
	history := fakeHistory{{Seq: 1, Kind: api.KindUserMessage, Text: "hi"}}
	l := New(contextModel{}, newEchoRegistry(t), prompt.NewBuilder(t.TempDir(), noMemory{}, noSkills{}), history, Options{Name: "fake", Toolsets: []string{"core"}})
	events, err := l.Run(context.Background(), engine.Turn{SessionID: "s", Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var done engine.Event
	for ev := range events {
		done = ev
	}
	if done.Kind != engine.KindDone || done.Context == nil || *done.Context != (engine.Context{Model: "gpt-5.5", Used: 1234, Window: 200_000, Input: 1234}) {
		t.Fatalf("done %+v", done)
	}
}
