package tui

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/viminizer/germes/internal/api"
)

// message is one finished entry of the transcript.
type message struct {
	Kind  messageKind
	Text  string
	Tool  *api.ToolCall // call with its result, for kindDetail
	Tools []toolRun     // the calls of one step, for kindTools
	Todos []todoItem    // the task list at the end of a turn, for kindTodo
	Dir   string        // project folder: paths under it are shown short
	Cont  bool          // kindAssistant: more of a reply already partly printed
}

type messageKind int

const (
	kindUser messageKind = iota
	kindAssistant
	kindTools  // the tool calls of one step, as a tree
	kindDetail // a tool call in full (/details)
	kindError
	kindNotice
	kindSwitch  // the router moved the session to another engine
	kindRound   // an attached job moved on to its next round
	kindAlert   // a job's message for the user
	kindTurnEnd // the short rule under a finished turn
	kindTodo    // the model's task list at the end of a turn
)

// view renders the message for the given width. Each block starts with a
// blank line, like Hermes; the rest of a reply printed in pieces does not.
func (m message) view(width int) string {
	switch m.Kind {
	case kindUser:
		return "\n" + gutter(styleLabel.Bold(true).Render("❯ "), styleLabel, m.Text, width)
	case kindAssistant:
		text := gutter(replyMark(m.Cont), lipgloss.NewStyle(), renderMarkdown(m.Text, width-3), width)
		if m.Cont {
			return text
		}
		return "\n" + text
	case kindTools:
		return "\n" + treeView(m.Tools, m.Dir, width, "", time.Time{}, 0)
	case kindDetail:
		return detailView(m.Tool, m.Dir, width)
	case kindError:
		return "\n" + gutter(styleError.Render("✗ "), styleError, m.Text, width)
	case kindSwitch:
		return "\n" + gutter(styleAccent.Render("⇄ "), styleAccent, m.Text, width)
	case kindAlert:
		return "\n" + gutter(styleAccent.Render("◆ "), styleAccent, m.Text, width)
	case kindRound:
		return "\n" + styleAccent.Render("── "+m.Text+" ──")
	case kindTodo:
		// Like Hermes: a finished list closed, an open one shown in full.
		done := isTodoDone(m.Todos)
		return "\n" + todoView(m.Todos, width, done, !done, 0)
	case kindTurnEnd:
		return "\n" + styleBorder.Render("───")
	default:
		return "\n" + gutter(styleMuted.Render("· "), styleMuted, m.Text, width)
	}
}

// replyMark starts a reply; the rest of a reply printed in pieces is only
// indented.
func replyMark(cont bool) string {
	if cont {
		return "   "
	}
	return styleBorder.Render("┊  ")
}

// gutter wraps text to width and puts mark before the first line.
func gutter(mark string, style lipgloss.Style, text string, width int) string {
	w := max(width-lipgloss.Width(mark), 10)
	lines := strings.Split(style.Width(w).Render(text), "\n")
	pad := strings.Repeat(" ", lipgloss.Width(mark))
	for i := range lines {
		if i == 0 {
			lines[i] = mark + lines[i]
		} else {
			lines[i] = pad + lines[i]
		}
	}
	return strings.Join(lines, "\n")
}

// toolLabel is Name("preview"), with the most telling argument as preview.
func toolLabel(t *api.ToolCall, dir string) string {
	var args map[string]any
	json.Unmarshal(t.Args, &args)
	if tasks, ok := args["tasks"].([]any); ok && t.Name == "delegate_task" {
		return fmt.Sprintf("%s(%d tasks)", toolName(t.Name), len(tasks))
	}
	if label := memoryLabel(t, args); label != "" {
		return label
	}
	if p := toolPreview(t, dir); p != "" {
		return fmt.Sprintf("%s(%q)", toolName(t.Name), p)
	}
	return toolName(t.Name)
}

// memoryLabel says what a memory call saves: Memory added (about you):
// "likes short answers". A remove shows the removed text (issue #18).
func memoryLabel(t *api.ToolCall, args map[string]any) string {
	verbs := map[string]string{"add": "added", "replace": "updated", "remove": "removed"}
	verb, ok := verbs[fmt.Sprint(args["action"])]
	if t.Name != "memory" || !ok {
		return ""
	}
	where := "agent notes"
	if args["target"] == "user" {
		where = "about you"
	}
	text, _ := args["text"].(string)
	if verb == "removed" {
		text, _ = args["old_text"].(string)
	}
	return fmt.Sprintf("Memory %s (%s): %q", verb, where, ansi.Truncate(firstLine(text), 64, "…"))
}

// toolPreview is the call's most telling argument, short, or "".
func toolPreview(t *api.ToolCall, dir string) string {
	var args map[string]any
	json.Unmarshal(t.Args, &args)
	for _, k := range []string{"command", "file_path", "pattern", "url", "query", "name", "text", "description"} {
		if s, ok := args[k].(string); ok && s != "" {
			return ansi.Truncate(firstLine(shortPaths(s, dir)), 64, "…")
		}
	}
	return ""
}

// subagentList shows one line per subagent report: its goal and how long
// the report is.
func subagentList(out string, width int) string {
	parts := strings.Split(out, "## Subagent ")
	var lines []string
	for _, p := range parts[1:] {
		head, body, _ := strings.Cut(p, "\n")
		num, goal, _ := strings.Cut(head, ": ")
		n := strings.Count(strings.TrimSpace(body), "\n") + 1
		lines = append(lines, styleDim.Render(ansi.Truncate(fmt.Sprintf("%s. %d-line report: %s", num, n, goal), max(width, 10), "…")))
	}
	return strings.Join(lines, "\n")
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

// shortPaths shows paths under the project folder relative to it, so tool
// lines look the same for every engine.
func shortPaths(s, dir string) string {
	if dir == "" || dir == "/" {
		return s
	}
	return strings.ReplaceAll(s, strings.TrimSuffix(dir, "/")+"/", "")
}
