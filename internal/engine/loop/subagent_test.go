package loop

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/viminizer/germes/internal/api"
	"github.com/viminizer/germes/internal/engine"
	"github.com/viminizer/germes/internal/llm"
	"github.com/viminizer/germes/internal/prompt"
	"github.com/viminizer/germes/internal/tool"
)

// delegatingModel: the parent delegates two tasks, then answers with what
// came back; a subagent answers with a report naming its goal.
type delegatingModel struct {
	mu       sync.Mutex
	requests []llm.Request
}

func (m *delegatingModel) Send(ctx context.Context, r llm.Request) (<-chan llm.Chunk, error) {
	m.mu.Lock()
	m.requests = append(m.requests, r)
	m.mu.Unlock()
	out := make(chan llm.Chunk, 4)
	first := r.Messages[0].Text
	last := r.Messages[len(r.Messages)-1]
	switch {
	case strings.Contains(first, "You are a subagent"):
		goal := first[strings.Index(first, "Goal: ")+6:]
		goal, _, _ = strings.Cut(goal, "\n")
		out <- llm.Chunk{Kind: llm.KindText, Text: "report: " + goal}
	case last.Role == llm.RoleTool:
		out <- llm.Chunk{Kind: llm.KindText, Text: "parent saw: " + last.Text}
	default:
		args, _ := json.Marshal(map[string]any{"tasks": []map[string]string{{"goal": "find A"}, {"goal": "find B", "context": "look in b/"}}})
		out <- llm.Chunk{Kind: llm.KindToolCall, ToolCall: &llm.ToolCall{ID: "d1", Name: delegateTool, Args: args}}
	}
	out <- llm.Chunk{Kind: llm.KindDone}
	close(out)
	return out, nil
}

func TestSubagentsReportBack(t *testing.T) {
	reg, err := tool.NewRegistry(nil, map[string]tool.Toolset{"none": {}}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	model := &delegatingModel{}
	history := fakeHistory{{Seq: 1, Kind: api.KindUserMessage, Text: "look into A and B"}}
	l := New(model, reg, prompt.NewBuilder(t.TempDir(), noMemory{}, noSkills{}), history,
		Options{Name: "fake", Toolsets: []string{"none"}, SubagentToolsets: []string{"none"}})
	events, err := l.Run(context.Background(), engine.Turn{SessionID: "s", Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var final string
	for ev := range events {
		if ev.Kind == engine.KindText {
			final = ev.Text
		}
	}
	for _, want := range []string{"## Subagent 1: find A\n\nreport: find A", "## Subagent 2: find B\n\nreport: find B"} {
		if !strings.Contains(final, want) {
			t.Fatalf("final answer lacks %q:\n%s", want, final)
		}
	}
	// A subagent may not start subagents of its own.
	for _, r := range model.requests {
		if strings.Contains(r.Messages[0].Text, "You are a subagent") {
			for _, s := range r.Tools {
				if s.Name == delegateTool {
					t.Fatal("a subagent got delegate_task")
				}
			}
		}
	}
}
