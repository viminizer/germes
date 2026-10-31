package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"

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
		"text\n```go\nx := 1\n":                {"text\n", "```go\nx := 1\n"},
		"```go\nx := 1\n```\nafter\n":          {"```go\nx := 1\n```\nafter\n", ""},
		"````md\n```go\nx\n```\n````\nafter\n": {"````md\n```go\nx\n```\n````\nafter\n", ""},
		"````md\n```go\n":                      {"", "````md\n```go\n"},
		"```js x := 1```\nafter\n":             {"```js x := 1```\nafter\n", ""},
		"a\n| h |\n|---|\n| 1 |\n":             {"a\n", "| h |\n|---|\n| 1 |\n"},
		"| h |\n|---|\n| 1 |\n\nafter\n":       {"| h |\n|---|\n| 1 |\n\nafter\n", ""},
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

// Like Hermes, the task list prints when the turn ends: in full while
// items are open, as one line once all are done.
func TestTaskListPrintsWhenTheTurnEnds(t *testing.T) {
	for out, wantLines := range map[string]int{
		"- [x] 1. Read main.go\n- [>] 2. Add sub\n  - [ ] 2.1. Test it\n  - [ ] step two. Ship": 5,
		"- [x] 1. Read main.go\n- [-] 2. Add sub":                                               1,
	} {
		var tr transcript
		tr.apply(api.Event{Seq: 1, Kind: api.KindToolCall, Tool: &api.ToolCall{ID: "1", Name: "todo_list"}})
		tr.apply(api.Event{Seq: 2, Kind: api.KindToolResult, Tool: &api.ToolCall{ID: "1", Output: out}})
		got := tr.apply(api.Event{Seq: 3, Kind: api.KindTurnDone})
		if len(got) != 3 || got[0].Kind != kindTools || got[1].Kind != kindTodo || got[2].Kind != kindTurnEnd {
			t.Fatalf("%q: got %+v, want the tree, the list, the end", out, got)
		}
		if n := strings.Count(strings.TrimPrefix(got[1].view(80), "\n"), "\n") + 1; n != wantLines {
			t.Errorf("%q: list is %d lines, want %d:\n%s", out, n, wantLines, got[1].view(80))
		}
	}
}

// The live area never gets taller than the screen: a taller one loses
// printed lines. A long task list and many failed calls shrink to fit.
func TestLiveAreaFitsTheScreen(t *testing.T) {
	var tr transcript
	list := ""
	for i := range 15 {
		list += fmt.Sprintf("- [ ] %d. Step %d\n", i, i)
	}
	tr.apply(api.Event{Seq: 1, Kind: api.KindToolCall, Tool: &api.ToolCall{ID: "t", Name: "todo_list"}})
	tr.apply(api.Event{Seq: 2, Kind: api.KindToolResult, Tool: &api.ToolCall{ID: "t", Output: list}})
	for i := range 12 {
		id := fmt.Sprint(i)
		tr.apply(api.Event{Seq: int64(3 + 2*i), Kind: api.KindToolCall, Tool: &api.ToolCall{ID: id, Name: "Bash"}})
		tr.apply(api.Event{Seq: int64(4 + 2*i), Kind: api.KindToolResult, Tool: &api.ToolCall{ID: id, Output: "boom", IsError: true}})
	}
	tr.apply(api.Event{Kind: api.KindTextDelta, Text: "working on it"})
	short := transcript{todos: parseTodos("- [>] 1. Only step")}
	if got := short.view(80, 30, "", time.Now()); !strings.Contains(got, "Only step") {
		t.Errorf("a one-item list is hidden: %q", got)
	}
	for _, height := range []int{5, 12, 30} {
		if got := lipgloss.Height(tr.view(80, height, "⠋", time.Now())); got > height {
			t.Errorf("height %d: live area is %d rows", height, got)
		}
	}
}

// A diff shows unchanged lines too, and removed lines before added ones.
func TestDiffLinesKeepUnchangedLines(t *testing.T) {
	got := diffLines([]string{"a", "for i := 1", "b"}, []string{"a", "for i := 0", "b", "c"})
	want := []string{"  a", "- for i := 1", "+ for i := 0", "  b", "+ c"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("diff %q, want %q", got, want)
	}
}

// An edit prints its change after the tree, like Hermes; an edit that
// changes nothing prints none.
func TestEditPrintsItsDiffAfterTheTree(t *testing.T) {
	for args, want := range map[string]int{
		`{"file_path":"calc.go","old_string":"x := 1","new_string":"x := 2"}`: 3,
		`{"file_path":"calc.go","old_string":"x","new_string":"x"}`:           2,
		`{"file_path":"calc.go","old_string":"x\r\n","new_string":"x\n"}`:     2,
	} {
		var tr transcript
		tr.apply(api.Event{Seq: 1, Kind: api.KindToolCall, Tool: &api.ToolCall{ID: "1", Name: "Edit", Args: []byte(args)}})
		tr.apply(api.Event{Seq: 2, Kind: api.KindToolResult, Tool: &api.ToolCall{ID: "1", Output: "ok"}})
		out := tr.apply(api.Event{Seq: 3, Kind: api.KindText, Text: "done"})
		if len(out) != want || (want == 3 && out[1].Kind != kindDiff) {
			t.Fatalf("%s: got %+v", args, out)
		}
	}
}

// Text from tools and files is cleaned before it is printed: a CRLF file
// or a terminal command in a fixture must not garble the screen.
func TestLinesAreCleanedBeforeTheyPrint(t *testing.T) {
	for in, want := range map[string]string{
		"x := 1\r":             "x := 1",
		"\x1b[2Jclear\x07":     "clear",
		"\tindent\x00":         "    indent",
		"\x1b]0;title\x07text": "text",
	} {
		if got := cleanLine(in); got != want {
			t.Errorf("cleanLine(%q) = %q, want %q", in, got, want)
		}
	}
}
