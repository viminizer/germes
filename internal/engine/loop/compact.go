package loop

import (
	"context"
	"fmt"
	"strings"

	"github.com/viminizer/germes/internal/api"
	"github.com/viminizer/germes/internal/llm"
	"github.com/viminizer/germes/internal/text"
	"github.com/viminizer/germes/internal/tool"
)

const (
	charsPerToken   = 4       // rough estimate, good enough to decide when to compact
	maxSummaryInput = 400_000 // characters of old conversation sent to the summary call
	prunedHead      = 300     // characters kept from the start of an old tool output
	prunedTail      = 500     // and from its end, where exit codes and test results are
	prunedMax       = 1_200   // longer than any pruned output, so pruning again changes nothing
	prunedNote      = "[Germes] old tool output cut to save context"
)

const summaryPrompt = `You summarize a coding session so another agent can continue it. Use exactly these sections,
each short; write "none" when a section is empty:

## Task (as it stood)
## Done (actions taken, with exact file paths and commands)
## Current state (what works, what is half done)
## Blocked (errors still open, with the exact message)
## Decisions (what was decided and why)
## Files (paths that matter)
## Notes (names, values and facts the next agent must not lose)

Keep exact names, paths and commands. Do not invent anything. If a summary of earlier work is given, update it:
keep what still holds, drop what was replaced.`

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
	window := l.window(ctx)
	if tokens(system, messages(events)) <= window/2 {
		return "", 0, false, nil
	}
	prev, kept := effective(events)
	// Keep the newest quarter of the window as it is, starting at a user
	// message so tool calls and results stay together.
	budget := window / 4 * charsPerToken
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
	transcript := text.Transcript(kept[:cut]).String()
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
		if msgs[i].Role == llm.RoleTool {
			msgs[i].Text = pruneOutput(msgs[i].Text)
		}
	}
}

// pruneOutput keeps the start and the end of an old tool output, so what
// ran and how it ended (an exit code, a test result, a refusal) are not
// lost (#34), and where the full output was saved, if it was.
func pruneOutput(s string) string {
	r := []rune(s)
	if len(r) <= prunedMax {
		return s
	}
	note := prunedNote + "; run it again to see all of it"
	if path := tool.GetSavedOutput(s); path != "" {
		note = prunedNote + "; the full output is in " + path
	}
	return string(r[:prunedHead]) + "\n…\n" + note + "\n…\n" + string(r[len(r)-prunedTail:])
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
