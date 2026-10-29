package tui

import (
	"strings"
	"testing"

	"github.com/viminizer/germes/internal/api"
)

func TestStopPrintsStreamedTextBeforeError(t *testing.T) {
	var tr transcript
	tr.apply(api.Event{Kind: api.KindTextDelta, Text: "half a sto"})
	got := tr.apply(api.Event{Seq: 1, Kind: api.KindError, Text: "stopped"})
	if len(got) != 2 || got[0].Kind != kindAssistant || got[0].Text != "half a sto" || got[1].Kind != kindError {
		t.Fatalf("got %+v, want the streamed text, then the error", got)
	}
	if rest := tr.apply(api.Event{Seq: 2, Kind: api.KindTurnDone}); len(rest) != 0 {
		t.Fatalf("text printed twice: %+v", rest)
	}
}

func TestStreamedLinesPrintOnce(t *testing.T) {
	var tr transcript
	var printed []string
	add := func(ms []message) {
		for _, m := range ms {
			printed = append(printed, m.Text)
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
