package loop

import (
	"context"
	"github.com/viminizer/germes/internal/text"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/viminizer/germes/internal/llm"
	"github.com/viminizer/germes/internal/tool"
)

const (
	reviewEvery   = 10 // tool steps without saving memory or a skill
	reviewSteps   = 6  // model calls the review may make
	reviewTimeout = 5 * time.Minute
)

const reviewPrompt = `You review a finished piece of work by a coding agent, to help it learn.
Decide whether anything is worth keeping for future sessions:
- memory (target agent): durable facts about the environment, tools or conventions;
- memory (target user): how the user likes to work;
- a skill: a reusable how-to for a kind of task that was hard or took several tries.
Save only what will clearly help later. Do not save one-off details. Update an existing entry or skill
instead of adding a near copy. If nothing is worth saving, reply "nothing to save" and stop.`

// countSteps updates a session's counter of tool calls since the agent last
// saved memory or a skill, and reports whether a review is due.
func (l *Loop) countSteps(sessionID string, calls []llm.ToolCall) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range calls {
		if c.Name == "memory" || c.Name == "skill_save" {
			l.steps[sessionID] = 0
			continue
		}
		l.steps[sessionID]++
	}
	if l.steps[sessionID] >= reviewEvery {
		l.steps[sessionID] = 0
		return true
	}
	return false
}

// review runs in the background after a turn: a short, silent agent run with
// only the memory and skill tools that reads the session and saves what is
// worth keeping (Hermes' learning loop). The user does not wait for it.
func (l *Loop) review(sessionID, dir string) {
	if len(l.opts.ReviewToolsets) == 0 {
		return
	}
	go func() {
		defer func() {
			if p := recover(); p != nil {
				slog.Error("review panicked", "session", sessionID, "panic", p, "stack", string(debug.Stack()))
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), reviewTimeout)
		defer cancel()
		events, err := l.loadEvents(ctx, sessionID)
		if err != nil {
			slog.Warn("review: load history", "session", sessionID, "err", err)
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
			ch, err := l.client.Send(ctx, llm.Request{System: reviewPrompt, Messages: msgs, Tools: specs})
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
				res := l.tools.Run(ctx, c.Name, tool.Call{SessionID: sessionID, Dir: dir, Args: c.Args})
				if !res.IsError {
					saved++
				}
				msgs = append(msgs, llm.Message{Role: llm.RoleTool, ToolCallID: c.ID, Text: res.Output})
			}
		}
		slog.Info("review done", "session", sessionID, "saved", saved)
	}()
}
