package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/viminizer/germes/internal/api"
)

func TestStopPrintsStreamedTextBeforeError(t *testing.T) {
	var tr transcript
	tr.apply(api.Event{Kind: api.KindTextDelta, Text: "half a sto"})
	got := tr.apply(api.Event{Seq: 1, Kind: api.KindError, Text: "stopped"})
	if len(got) != 2 || got[0].Kind != kindAssistant || got[0].Text != "half a sto" || got[1].Kind != kindError {
		t.Fatalf("got %+v, want the streamed text, then the error", got)
	}
	if rest := tr.apply(api.Event{Seq: 2, Kind: api.KindTurnDone}); len(rest) != 1 || rest[0].Kind != kindTurnEnd {
		t.Fatalf("text printed twice: %+v", rest)
	}
}

func TestStreamedLinesPrintOnce(t *testing.T) {
	var tr transcript
	var printed []string
	add := func(ms []message) {
		for _, m := range ms {
			if m.Kind == kindAssistant {
				printed = append(printed, m.Text)
			}
		}
	}
	add(tr.apply(api.Event{Kind: api.KindTextDelta, Text: "Hello\nwor"}))
	if len(printed) != 1 || printed[0] != "Hello" || tr.streaming.String() != "wor" {
		t.Fatalf("after the first line: %q, live %q", printed, tr.streaming.String())
	}
	add(tr.apply(api.Event{Kind: api.KindTextDelta, Text: "ld\n\nbye"}))
	add(tr.apply(api.Event{Seq: 1, Kind: api.KindText, Text: "Hello\nworld\n\nbye"}))
	add(tr.apply(api.Event{Seq: 2, Kind: api.KindTurnDone}))
	if got := strings.Join(printed, "\n"); got != "Hello\nworld\n\nbye" {
		t.Fatalf("printed %q", got)
	}
}

func TestToolCallsPrintAsOneTreeBeforeTheText(t *testing.T) {
	var tr transcript
	start := time.Now()
	call := func(seq int64, id, name string, isErr bool) []message {
		tr.apply(api.Event{Seq: seq, Kind: api.KindToolCall, Time: start, Tool: &api.ToolCall{ID: id, Name: name, Args: []byte(`{"file_path":"a.go"}`)}})
		return tr.apply(api.Event{Seq: seq + 1, Kind: api.KindToolResult, Time: start.Add(time.Second), Tool: &api.ToolCall{ID: id, Output: "x", IsError: isErr}})
	}
	if got := append(append(call(1, "1", "Read", false), call(3, "2", "Bash", false)...), call(5, "3", "Read", true)...); len(got) != 0 {
		t.Fatalf("calls printed before the text: %+v", got)
	}
	got := tr.apply(api.Event{Seq: 7, Kind: api.KindText, Text: "done"})
	if len(got) != 2 || got[0].Kind != kindTools || len(got[0].Tools) != 3 || got[1].Kind != kindAssistant {
		t.Fatalf("got %+v, want the tree, then the text", got)
	}
	if r := got[0].Tools[2]; !r.Done || !r.Call.IsError || r.Took != time.Second {
		t.Fatalf("failed call: %+v", r)
	}
}

// A code block or a table prints whole, once it ends.
func TestOpenBlocksWaitUntilTheyEnd(t *testing.T) {
	for in, want := range map[string][2]string{
		"text\n```go\nx := 1\n":          {"text\n", "```go\nx := 1\n"},
		"```go\nx := 1\n```\nafter\n":    {"```go\nx := 1\n```\nafter\n", ""},
		"a\n| h |\n|---|\n| 1 |\n":       {"a\n", "| h |\n|---|\n| 1 |\n"},
		"| h |\n|---|\n| 1 |\n\nafter\n": {"| h |\n|---|\n| 1 |\n\nafter\n", ""},
	} {
		if ready, held := readyBlocks(in); ready != want[0] || held != want[1] {
			t.Errorf("%q: got %q / %q, want %q / %q", in, ready, held, want[0], want[1])
		}
	}
}

// A memory call says what it saves, a remove what it removed (issue #18).
func TestMemoryCallsSayWhatTheySave(t *testing.T) {
	for args, want := range map[string]string{
		`{"action":"add","target":"user","text":"likes short answers"}`:         `Memory added (about you): "likes short answers"`,
		`{"action":"replace","target":"agent","old_text":"x","text":"uses Go"}`: `Memory updated (agent notes): "uses Go"`,
		`{"action":"remove","target":"agent","old_text":"an old note"}`:         `Memory removed (agent notes): "an old note"`,
	} {
		if got := toolLabel(&api.ToolCall{Name: "memory", Args: []byte(args)}, ""); got != want {
			t.Errorf("%s: got %q, want %q", args, got, want)
		}
	}
}
