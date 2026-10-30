package loop

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/viminizer/germes/internal/api"
	"github.com/viminizer/germes/internal/engine"
	"github.com/viminizer/germes/internal/llm"
	"github.com/viminizer/germes/internal/prompt"
	"github.com/viminizer/germes/internal/tool"
)

// askingModel calls clarify on its first request.
type askingModel struct{ requests int }

func (m *askingModel) Send(ctx context.Context, r llm.Request) (<-chan llm.Chunk, error) {
	m.requests++
	args, _ := json.Marshal(map[string]any{"questions": []map[string]any{
		{"question": "Which database?", "options": []string{"SQLite", "Postgres"}},
		{"question": "Add tests?", "options": []string{"Yes", "No"}},
	}})
	out := make(chan llm.Chunk, 2)
	out <- llm.Chunk{Kind: llm.KindToolCall, ToolCall: &llm.ToolCall{ID: "q1", Name: clarifyTool, Args: args}}
	out <- llm.Chunk{Kind: llm.KindDone}
	close(out)
	return out, nil
}

func TestClarifyEndsTheTurnWithQuestions(t *testing.T) {
	reg, _ := tool.NewRegistry(nil, map[string]tool.Toolset{"none": {}}, nil, "")
	model := &askingModel{}
	l := New(model, reg, prompt.NewBuilder(t.TempDir(), noMemory{}, noSkills{}),
		fakeHistory{{Seq: 1, Kind: api.KindUserMessage, Text: "build the app"}}, Options{Name: "fake", Toolsets: []string{"none"}})
	events, _ := l.Run(context.Background(), engine.Turn{SessionID: "s", Dir: t.TempDir()})
	var kinds []engine.Kind
	var text string
	for ev := range events {
		kinds = append(kinds, ev.Kind)
		if ev.Kind == engine.KindText {
			text = ev.Text
		}
	}
	if model.requests != 1 {
		t.Fatalf("%d model calls; the turn should end after asking", model.requests)
	}
	want := "1. Which database?\n   a) SQLite (recommended)\n   b) Postgres\n\n2. Add tests?"
	if !strings.Contains(text, want) || kinds[len(kinds)-1] != engine.KindDone {
		t.Fatalf("events %v, text:\n%s", kinds, text)
	}
}
