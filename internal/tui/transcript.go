package tui

import (
	"strings"

	"github.com/viminizer/germes/internal/api"
)

// transcript turns session events into finished messages (printed above the
// live area) and keeps the live part: streaming text and running tools.
type transcript struct {
	streaming strings.Builder
	running   []*api.ToolCall // tool calls waiting for their result
	lastSeq   int64
}

// apply takes one event and returns the messages it finished.
func (t *transcript) apply(ev api.Event) []message {
	if ev.Seq != 0 {
		if ev.Seq <= t.lastSeq {
			return nil // already shown (replay after a reconnect)
		}
		t.lastSeq = ev.Seq
	}
	switch ev.Kind {
	case api.KindUserMessage:
		return []message{{Kind: kindUser, Text: ev.Text}}
	case api.KindTextDelta:
		t.streaming.WriteString(ev.Text)
	case api.KindText:
		t.streaming.Reset()
		return []message{{Kind: kindAssistant, Text: ev.Text}}
	case api.KindToolCall:
		if ev.Tool != nil {
			t.running = append(t.running, ev.Tool)
		}
	case api.KindToolResult:
		return t.finishTool(ev.Tool)
	case api.KindError, api.KindLimitHit:
		return []message{{Kind: kindError, Text: ev.Text}}
	case api.KindEngineSwitched:
		return []message{{Kind: kindNotice, Text: ev.Text}}
	case api.KindTurnDone:
		return t.flush()
	}
	return nil
}

func (t *transcript) finishTool(result *api.ToolCall) []message {
	if result == nil {
		return nil
	}
	for i, call := range t.running {
		if call.ID == result.ID {
			t.running = append(t.running[:i], t.running[i+1:]...)
			done := *call
			done.Output, done.IsError = result.Output, result.IsError
			if done.Output == "" {
				done.Output = "done"
			}
			return []message{{Kind: kindTool, Tool: &done}}
		}
	}
	return nil
}

// flush ends the turn: leftover text and tools without a result are printed.
func (t *transcript) flush() []message {
	var out []message
	if s := strings.TrimSpace(t.streaming.String()); s != "" {
		out = append(out, message{Kind: kindAssistant, Text: s})
	}
	t.streaming.Reset()
	for _, call := range t.running {
		out = append(out, message{Kind: kindTool, Tool: call})
	}
	t.running = nil
	return out
}

// lastSeqShown is the last stored event shown, for reconnecting.
func (t *transcript) lastSeqShown() int64 { return t.lastSeq }

// view renders the live part: running tools with a spinner, then the text
// that is still streaming.
func (t *transcript) view(width int, spin string) string {
	var parts []string
	for _, call := range t.running {
		parts = append(parts, styleAccent.Render(spin+" ")+styleText.Render(toolLabel(call)))
	}
	if s := t.streaming.String(); s != "" {
		parts = append(parts, gutter(styleBorder.Render("┊ "), styleText, s+"▍", width))
	}
	return strings.Join(parts, "\n")
}
