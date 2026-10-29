package loop

import (
	"context"
	"fmt"
	"strings"

	"github.com/viminizer/germes/internal/api"
	"github.com/viminizer/germes/internal/llm"
)

const (
	charsPerToken    = 4       // rough estimate, good enough to decide when to compact
	maxSummaryInput  = 400_000 // characters of old conversation sent to the summary call
	prunedToolOutput = "[old tool output removed to save context]"
)

const summaryPrompt = `You summarize a coding session so another agent can continue it.
Write a short structured summary: the goal, decisions made, files created or changed (exact paths),
commands that matter, errors and how they were fixed, the current state, and what is left to do.
Keep exact names, paths and commands. Do not invent anything.`

// tokens estimates the size of a request.
func tokens(system string, msgs []llm.Message) int {
	n := len(system)
	for _, m := range msgs {
		n += len(m.Text) + len(m.Raw)
		for _, c := range m.ToolCalls {
			n += len(c.Name) + len(c.Args)
		}
	}
	return n / charsPerToken
}

// compact summarizes old history when the session is over half the context
// window. It returns the summary and the seq from which events are kept, or
// ok=false when no compaction is needed or possible.
func (l *Loop) compact(ctx context.Context, system string, events []api.Event) (summary string, keepFrom int64, ok bool, err error) {
	if tokens(system, messages(events)) <= l.opts.ContextWindow/2 {
		return "", 0, false, nil
	}
	prev, kept := effective(events)
	// Keep the newest quarter of the window as it is, starting at a user
	// message so tool calls and results stay together.
	budget := l.opts.ContextWindow / 4 * charsPerToken
	cut, size := len(kept), 0
	for cut > 0 && size < budget {
		cut--
		size += len(kept[cut].Text)
		if kept[cut].Tool != nil {
			size += len(kept[cut].Tool.Output) + len(kept[cut].Tool.Args)
		}
	}
	for cut < len(kept) && kept[cut].Kind != api.KindUserMessage {
		cut++
	}
	if cut == 0 || cut >= len(kept) {
		return "", 0, false, nil // nothing old enough to summarize
	}
	transcript := render(kept[:cut])
	if prev != "" {
		transcript = "Summary of what came before:\n" + prev + "\n\n" + transcript
	}
	if len(transcript) > maxSummaryInput {
		transcript = "[... oldest part cut ...]\n" + transcript[len(transcript)-maxSummaryInput:]
	}
	text, err := l.ask(ctx, summaryPrompt, transcript)
	if err != nil {
		return "", 0, false, fmt.Errorf("compact history: %w", err)
	}
	return text, kept[cut].Seq, true, nil
}

// prune shortens old tool outputs inside a long turn. It is free: no model
// call. The newest messages stay as they are.
func prune(msgs []llm.Message, keep int) {
	for i := 0; i < len(msgs)-keep; i++ {
		if msgs[i].Role == llm.RoleTool && len(msgs[i].Text) > len(prunedToolOutput) {
			msgs[i].Text = prunedToolOutput
		}
	}
}

// render writes events as plain text for a summary or a review.
func render(events []api.Event) string {
	var b strings.Builder
	for _, ev := range events {
		switch ev.Kind {
		case api.KindUserMessage:
			fmt.Fprintf(&b, "User: %s\n", ev.Text)
		case api.KindText:
			fmt.Fprintf(&b, "Assistant: %s\n", ev.Text)
		case api.KindToolCall:
			if ev.Tool != nil {
				fmt.Fprintf(&b, "Tool call: %s %s\n", ev.Tool.Name, clip(string(ev.Tool.Args), 500))
			}
		case api.KindToolResult:
			if ev.Tool != nil {
				fmt.Fprintf(&b, "Tool result: %s\n", clip(ev.Tool.Output, 800))
			}
		case api.KindError:
			fmt.Fprintf(&b, "Error: %s\n", ev.Text)
		}
	}
	return b.String()
}

// ask makes one model call without tools and returns its text.
func (l *Loop) ask(ctx context.Context, system, user string) (string, error) {
	ch, err := l.client.Send(ctx, llm.Request{System: system, Messages: []llm.Message{{Role: llm.RoleUser, Text: user}}})
	if err != nil {
		return "", err
	}
	var text []string
	for c := range ch {
		switch c.Kind {
		case llm.KindText:
			text = append(text, c.Text)
		case llm.KindError, llm.KindLimitHit:
			return "", c.Err
		}
	}
	if len(text) == 0 {
		return "", fmt.Errorf("empty answer")
	}
	return strings.Join(text, "\n"), nil
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
