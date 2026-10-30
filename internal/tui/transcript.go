package tui

import (
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/viminizer/germes/internal/api"
)

// transcript turns session events into finished messages (printed above the
// live area) and keeps the live part: the tool calls of the current step
// and the last, unfinished line of the streaming reply. Finished lines go
// to the scrollback as they stream, so a long reply never outgrows the
// screen.
type transcript struct {
	dir       string          // project folder, for short paths
	streaming strings.Builder // the unfinished line
	held      string          // finished lines of an open code block or table
	printed   string          // the part of the current reply already printed
	started   bool            // a message of the current reply is printed
	tools     []toolRun       // calls since the last text, printed as one tree
	lastSeq   int64
}

// toolRun is a tool call in the tree, with how long it took.
type toolRun struct {
	Call  *api.ToolCall
	Start time.Time
	Took  time.Duration
	Done  bool
}

// apply takes one event and returns the messages it finished. Tool calls
// wait in a tree, printed whole before the text or error that follows
// them, like Hermes.
func (t *transcript) apply(ev api.Event) []message {
	out := t.applyEvent(ev)
	ends := ev.Kind == api.KindTurnDone || slices.ContainsFunc(out, func(m message) bool {
		return m.Kind == kindAssistant || m.Kind == kindError
	})
	if len(t.tools) > 0 && ends {
		out = append([]message{{Kind: kindTools, Tools: t.tools, Dir: t.dir}}, out...)
		t.tools = nil
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
			t.tools = append(t.tools, toolRun{Call: ev.Tool, Start: eventTime(ev)})
		}
	case api.KindToolResult:
		t.finishTool(ev)
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
		return append(t.flushText(), message{Kind: kindTurnEnd})
	}
	return nil
}

// finishTool puts a result on its call in the tree.
func (t *transcript) finishTool(ev api.Event) {
	if ev.Tool == nil {
		return
	}
	for i := range t.tools {
		run := &t.tools[i]
		if run.Call.ID == ev.Tool.ID && !run.Done {
			done := *run.Call
			done.Output, done.IsError = ev.Tool.Output, ev.Tool.IsError
			run.Call, run.Took, run.Done = &done, eventTime(ev).Sub(run.Start), true
			return
		}
	}
}

// flushText prints the rest of a reply that ends without its final text
// (stopped, failed, or the turn ended).
func (t *transcript) flushText() []message {
	return t.endReply(t.held + t.streaming.String())
}

// printLines prints the finished lines of the streaming reply and keeps the
// unfinished last line live. An open code block or table waits until it
// ends, so it is drawn whole.
func (t *transcript) printLines() []message {
	s := t.streaming.String()
	i := strings.LastIndex(s, "\n")
	if i < 0 {
		return nil
	}
	t.streaming.Reset()
	t.streaming.WriteString(s[i+1:])
	ready, held := readyBlocks(t.held + s[:i+1])
	t.held = held
	if ready == "" {
		return nil
	}
	t.printed += ready
	return t.reply(strings.TrimSuffix(ready, "\n"))
}

// endReply prints the rest of the reply and starts a new one.
func (t *transcript) endReply(rest string) []message {
	out := t.reply(strings.TrimRight(rest, " \t\n"))
	t.streaming.Reset()
	t.held, t.printed, t.started = "", "", false
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

// maxLiveTools is how many tree rows the live area shows; the printed
// tree has them all.
const maxLiveTools = 8

// view renders the live part: the tool tree of the current step, running
// calls with a spinner, then one row for the unfinished line of the reply
// (empty when none). The row is always there: a live area that changes
// height on every line loses or repeats printed lines at the bottom of the
// screen.
func (t *transcript) view(width int, spin string, now time.Time) string {
	var parts []string
	// A blank line before a block, as it will be printed.
	if len(t.tools) > 0 {
		parts = append(parts, "", treeView(t.tools, t.dir, width, spin, now, maxLiveTools))
	}
	if s := lastLine(t.held + t.streaming.String()); s != "" {
		if !t.started {
			parts = append(parts, "")
		}
		parts = append(parts, ansi.Truncate(replyMark(t.started)+styleText.Render(s)+"▍", max(width, 1), "…"))
	}
	return strings.Join(parts, "\n")
}

func lastLine(s string) string {
	s = strings.TrimSuffix(s, "\n")
	return s[strings.LastIndex(s, "\n")+1:]
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
