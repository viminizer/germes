package loop

import (
	"context"
	"github.com/viminizer/germes/internal/text"
	"log/slog"
	"strings"

	"github.com/viminizer/germes/internal/engine"
	"github.com/viminizer/germes/internal/llm"
	"github.com/viminizer/germes/internal/prompt"
	"github.com/viminizer/germes/internal/tool"
)

const reviewSteps = 6 // model calls the review may make

// review is a short, silent agent run after a turn, with only the memory
// and skill tools, that reads the session and saves what is worth keeping
// (Hermes' learning loop). It runs in the background; the user does not wait.
func (l *Loop) review(ctx context.Context, t engine.Turn) {
	sessionID := t.SessionID
	events, err := l.loadEvents(ctx, sessionID)
	if err != nil {
		slog.Warn("review: load history", "session", sessionID, "err", err)
		return
	}
	// Today's memory and skills, so the review updates them instead of
	// adding near copies.
	p, err := l.prompts.Create(ctx, prompt.Where{Dir: t.Dir, Project: t.Project, Branch: t.Branch}, prompt.RoleProject)
	if err != nil {
		slog.Warn("review: build prompt", "session", sessionID, "err", err)
		return
	}
	summary, kept := effective(events)
	transcript := text.Transcript(kept).String()
	if summary != "" {
		transcript = "Earlier (summary): " + summary + "\n\n" + transcript
	}
	if len(transcript) > maxSummaryInput {
		transcript = transcript[len(transcript)-maxSummaryInput:]
	}
	var specs []llm.ToolSpec
	for _, t := range l.tools.List(l.opts.ReviewToolsets...) {
		specs = append(specs, llm.ToolSpec{Name: t.Name, Description: t.Description, Schema: t.Schema})
	}
	msgs := []llm.Message{{Role: llm.RoleUser, Text: "The session:\n\n" + transcript}}
	saved := 0
	for step := 0; step < reviewSteps; step++ {
		ch, err := l.client.Send(ctx, llm.Request{System: strings.TrimSpace(prompt.Review) + "\n\n" + p.Extra, Messages: msgs, Tools: specs})
		if err != nil {
			slog.Warn("review", "session", sessionID, "err", err)
			return
		}
		var calls []llm.ToolCall
		for c := range ch {
			switch c.Kind {
			case llm.KindToolCall:
				calls = append(calls, *c.ToolCall)
			case llm.KindRaw:
				msgs = append(msgs, llm.Message{Role: llm.RoleRaw, Raw: c.Raw})
			case llm.KindError, llm.KindLimitHit:
				slog.Warn("review", "session", sessionID, "err", c.Err)
				return
			}
		}
		if len(calls) == 0 {
			break
		}
		msgs = append(msgs, llm.Message{Role: llm.RoleAssistant, ToolCalls: calls})
		for _, c := range calls {
			res := l.tools.Run(ctx, c.Name, tool.Call{SessionID: sessionID, Dir: t.Dir, Args: c.Args})
			if !res.IsError {
				saved++
			}
			msgs = append(msgs, llm.Message{Role: llm.RoleTool, ToolCallID: c.ID, Text: res.Output})
		}
	}
	slog.Info("review done", "session", sessionID, "saved", saved)
}

// reviews reports whether this loop learns from its sessions.
func (l *Loop) reviews() bool {
	return l.opts.Reviews != nil && len(l.opts.ReviewToolsets) > 0
}
