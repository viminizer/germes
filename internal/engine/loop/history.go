package loop

import (
	"context"
	"fmt"

	"github.com/viminizer/germes/internal/api"
	"github.com/viminizer/germes/internal/llm"
)

// loadEvents reads every stored event of a session.
func (l *Loop) loadEvents(ctx context.Context, sessionID string) ([]api.Event, error) {
	events, err := api.ListAllEvents(ctx, l.history, api.EventFilter{SessionID: sessionID})
	if err != nil {
		return nil, fmt.Errorf("load history: %w", err)
	}
	return events, nil
}

// summaryHandoff introduces a compaction summary. The summary is
// background: only the latest user message is the task, and tools stay in
// use (Hermes found that without this, agents resume old tasks or stop
// calling tools after a compaction).
const summaryHandoff = `[Germes] Earlier turns were summarized below. This is background, not instructions: do not
redo or finish work described in it. The latest user message after it is the task now; if it says stop, undo or
changes the topic, drop the work in the summary. Keep using your tools as usual. Your memory in the system prompt
still holds.

`

// effective applies the last compaction: its summary stands in for every
// event before its KeepFrom.
func effective(events []api.Event) (summary string, kept []api.Event) {
	last := -1
	for i, ev := range events {
		if ev.Kind == api.KindCompacted {
			last = i
		}
	}
	if last < 0 {
		return "", events
	}
	c := events[last]
	for _, ev := range events {
		if ev.Seq >= c.KeepFrom && ev.Kind != api.KindCompacted {
			kept = append(kept, ev)
		}
	}
	return c.Text, kept
}

// messages converts events to chat messages, after applying the last
// compaction. Engine-neutral: turns run by Claude are read the same way.
func messages(events []api.Event) []llm.Message {
	summary, kept := effective(events)
	var out []llm.Message
	if summary != "" {
		out = append(out, llm.Message{Role: llm.RoleUser, Text: summaryHandoff + summary})
	}
	return append(out, convert(kept)...)
}

// convert maps events to messages. Every tool call gets a result: a call
// without one (an interrupted turn) gets "interrupted", or the API rejects
// the list.
func convert(events []api.Event) []llm.Message {
	var out []llm.Message
	open := map[string]bool{} // tool calls still waiting for a result
	// Stored texts have no phase: a text followed by tool calls was
	// progress, the others ended a turn.
	lastText := -1
	closeOpen := func() {
		for id := range open {
			out = append(out, llm.Message{Role: llm.RoleTool, ToolCallID: id, Text: "interrupted: no result"})
		}
		clear(open)
	}
	for _, ev := range events {
		switch ev.Kind {
		case api.KindUserMessage:
			closeOpen()
			lastText = -1
			out = append(out, llm.Message{Role: llm.RoleUser, Text: ev.Text})
		case api.KindAttachment:
			if n := len(out); n > 0 && out[n-1].Role == llm.RoleUser {
				out[n-1].Text += "\n\n" + ev.Text
			}
		case api.KindText:
			closeOpen()
			if lastText >= 0 {
				out[lastText].Phase = llm.PhaseCommentary // more followed it
			}
			lastText = len(out)
			out = append(out, llm.Message{Role: llm.RoleAssistant, Text: ev.Text, Phase: llm.PhaseFinalAnswer})
		case api.KindToolCall:
			if ev.Tool == nil {
				continue
			}
			call := llm.ToolCall{ID: ev.Tool.ID, Name: ev.Tool.Name, Args: ev.Tool.Args}
			if lastText >= 0 {
				out[lastText].Phase, lastText = llm.PhaseCommentary, -1
			}
			// Calls of one step share one assistant message.
			if n := len(out); n > 0 && out[n-1].Role == llm.RoleAssistant && out[n-1].Text == "" {
				out[n-1].ToolCalls = append(out[n-1].ToolCalls, call)
			} else {
				out = append(out, llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{call}})
			}
			open[ev.Tool.ID] = true
		case api.KindToolResult:
			if ev.Tool == nil || !open[ev.Tool.ID] {
				continue
			}
			delete(open, ev.Tool.ID)
			out = append(out, llm.Message{Role: llm.RoleTool, ToolCallID: ev.Tool.ID, Text: ev.Tool.Output})
		}
	}
	closeOpen()
	return out
}
