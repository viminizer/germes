package loop

import (
	"context"
	"fmt"

	"github.com/viminizer/germes/internal/api"
	"github.com/viminizer/germes/internal/llm"
)

// load turns the session's stored events into chat messages. Engine-neutral:
// turns run by another engine (Claude) are read the same way.
func (l *Loop) load(ctx context.Context, sessionID string) ([]llm.Message, error) {
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
	return messages(events), nil
}

// messages converts events. Every tool call gets a result: a call without
// one (an interrupted turn) gets "interrupted", or the API rejects the list.
func messages(events []api.Event) []llm.Message {
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
			// Calls of one step share one assistant message.
			if n := len(out); n > 0 && out[n-1].Role == llm.RoleAssistant && out[n-1].Text == "" {
				out[n-1].ToolCalls = append(out[n-1].ToolCalls, llm.ToolCall{ID: ev.Tool.ID, Name: ev.Tool.Name, Args: ev.Tool.Args})
			} else {
				out = append(out, llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: ev.Tool.ID, Name: ev.Tool.Name, Args: ev.Tool.Args}}})
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
