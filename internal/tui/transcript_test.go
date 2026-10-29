package tui

import (
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
