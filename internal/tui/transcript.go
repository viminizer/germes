package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/viminizer/germes/internal/api"
)

// transcript turns session events into finished messages (printed above the
// live area) and keeps the live part: running tools and the last, unfinished
// line of the streaming reply. Finished lines go to the scrollback as they
// stream, so a long reply never outgrows the screen.
type transcript struct {
	dir       string          // project folder, for short paths
	streaming strings.Builder // the unfinished line
	printed   string          // the part of the current reply already printed
	started   bool            // a message of the current reply is printed
	running   []*api.ToolCall // tool calls waiting for their result
	group     []*api.ToolCall // finished routine calls (reads, searches) not printed yet
	lastSeq   int64
}

// routine tools only look around; in a row they print as one line.
var routine = map[string]bool{"Read": true, "Glob": true, "Grep": true, "LS": true, "skill_view": true,
	"session_search": true, "WebFetch": true, "WebSearch": true}

// apply takes one event and returns the messages it finished. Routine calls
// wait in a group, printed as one line before whatever comes next.
func (t *transcript) apply(ev api.Event) []message {
	out := t.applyEvent(ev)
	if len(t.group) > 0 && (len(out) > 0 || ev.Kind == api.KindTurnDone) {
		out = append([]message{{Kind: kindTools, Tools: t.group, Dir: t.dir}}, out...)
		t.group = nil
	}
	return out
}

func (t *transcript) applyEvent(ev api.Event) []message {
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
		return t.printLines()
	case api.KindText:
		// Only what the deltas did not print yet.
		rest := ev.Text
		if len(t.printed) <= len(rest) {
			rest = rest[len(t.printed):]
		}
		return t.endReply(rest)
	case api.KindToolCall:
		if ev.Tool != nil {
			t.running = append(t.running, ev.Tool)
		}
	case api.KindToolResult:
		return t.finishTool(ev.Tool)
	case api.KindError, api.KindLimitHit:
		// Text streamed so far comes first, then the error under it.
		return append(t.flushText(), message{Kind: kindError, Text: ev.Text})
	case api.KindEngineSwitched:
		return []message{{Kind: kindSwitch, Text: ev.Text}}
	case api.KindNotice:
		return []message{{Kind: kindAlert, Text: ev.Text}}
	case api.KindStatus, api.KindLearned:
		return []message{{Kind: kindNotice, Text: ev.Text}}
	case api.KindAttachment:
		return []message{{Kind: kindNotice, Text: "attached: " + attachedNames(ev.Text)}}
	case api.KindCompacted:
		return []message{{Kind: kindNotice, Text: "older history was summarized to keep the context small"}}
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
			if routine[done.Name] && !done.IsError {
				t.group = append(t.group, &done)
				return nil
			}
			return []message{{Kind: kindTool, Tool: &done, Dir: t.dir}}
		}
	}
	return nil
}

// flush ends the turn: leftover text and tools without a result are printed.
func (t *transcript) flush() []message {
	out := t.flushText()
	for _, call := range t.running {
		out = append(out, message{Kind: kindTool, Tool: call, Dir: t.dir})
	}
	t.running = nil
	return out
}

// flushText prints the rest of a reply that ends without its final text
// (stopped, failed, or the turn ended).
func (t *transcript) flushText() []message {
	return t.endReply(t.streaming.String())
}

// printLines prints the finished lines of the streaming reply and keeps the
// unfinished last line live.
func (t *transcript) printLines() []message {
	s := t.streaming.String()
	i := strings.LastIndex(s, "\n")
	if i < 0 {
		return nil
	}
	t.streaming.Reset()
	t.streaming.WriteString(s[i+1:])
	t.printed += s[:i+1]
	return t.reply(s[:i])
}

// endReply prints the rest of the reply and starts a new one.
func (t *transcript) endReply(rest string) []message {
	out := t.reply(strings.TrimRight(rest, " \t\n"))
	t.streaming.Reset()
	t.printed, t.started = "", false
	return out
}

// reply is a piece of the current reply as a message: the first piece
// carries the reply's mark, the next ones continue it.
func (t *transcript) reply(text string) []message {
	if !t.started {
		text = strings.TrimLeft(text, " \t\n")
	}
	if strings.TrimSpace(text) == "" && !t.started {
		return nil
	}
	m := message{Kind: kindAssistant, Text: text, Cont: t.started}
	t.started = true
	return []message{m}
}

// lastSeqShown is the last stored event shown, for reconnecting.
func (t *transcript) lastSeqShown() int64 { return t.lastSeq }

// view renders the live part: running tools with a spinner, then one row
// for the unfinished line of the reply (empty when none). The row is always
// there: a live area that changes height on every line loses or repeats
// printed lines at the bottom of the screen.
func (t *transcript) view(width int, spin string) string {
	var parts []string
	for _, call := range t.running {
		parts = append(parts, styleAccent.Render(spin+" ")+styleText.Render(toolLabel(call, t.dir)))
	}
	line := ""
	if s := t.streaming.String(); s != "" {
		line = ansi.Truncate(replyMark(t.started)+styleText.Render(s)+"▍", max(width, 1), "…")
	}
	return strings.Join(append(parts, line), "\n")
}

// attachedNames lists the references in an attachment ("--- @file:x ---" headers).
func attachedNames(att string) string {
	var names []string
	for _, line := range strings.Split(att, "\n") {
		if name, ok := strings.CutPrefix(line, "--- "); ok && strings.HasSuffix(name, " ---") {
			names = append(names, strings.TrimSuffix(name, " ---"))
		}
	}
	return strings.Join(names, ", ")
}
