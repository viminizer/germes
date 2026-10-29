package loop

import (
	"context"
	"fmt"

	"github.com/viminizer/germes/internal/api"
	"github.com/viminizer/germes/internal/llm"
)

// loadEvents reads every stored event of a session.
func (l *Loop) loadEvents(ctx context.Context, sessionID string) ([]api.Event, error) {
	var events []api.Event
	var after int64
	for {
		page, err := l.history.ListEvents(ctx, api.EventFilter{SessionID: sessionID, AfterSeq: after, Limit: 1000})
		if err != nil {
			return nil, fmt.Errorf("load history: %w", err)
		}
		events = append(events, page.Items...)
		if page.Next == "" || len(page.Items) == 0 {
			break
		}
		after = page.Items[len(page.Items)-1].Seq
	}
	return events, nil
}

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
		out = append(out, llm.Message{Role: llm.RoleUser, Text: "[Germes] Summary of the earlier conversation:\n\n" + summary})
	}
	return append(out, convert(kept)...)
}

// convert maps events to messages. Every tool call gets a result: a call
// without one (an interrupted turn) gets "interrupted", or the API rejects
// the list.
func convert(events []api.Event) []llm.Message {
	var out []llm.Message
	open := map[string]bool{} // tool calls still waiting for a result
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
			out = append(out, llm.Message{Role: llm.RoleUser, Text: ev.Text})
		case api.KindText:
			closeOpen()
			out = append(out, llm.Message{Role: llm.RoleAssistant, Text: ev.Text})
		case api.KindToolCall:
			if ev.Tool == nil {
				continue
			}
			call := llm.ToolCall{ID: ev.Tool.ID, Name: ev.Tool.Name, Args: ev.Tool.Args}
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
