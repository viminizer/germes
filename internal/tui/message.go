package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/viminizer/germes/internal/api"
)

// message is one finished entry of the transcript.
type message struct {
	Kind  messageKind
	Text  string
	Tool  *api.ToolCall   // call with its result, for kindTool and kindDetail
	Tools []*api.ToolCall // routine calls in a row, for kindTools
	Dir   string          // project folder: paths under it are shown short
	Cont  bool            // kindAssistant: more of a reply already partly printed
}

type messageKind int

const (
	kindUser messageKind = iota
	kindAssistant
	kindTool
	kindTools  // routine calls in a row, as one line
	kindDetail // a tool call in full (/details)
	kindError
	kindNotice
	kindSwitch // the router moved the session to another engine
	kindRound  // an attached job moved on to its next round
	kindAlert  // a job's message for the user
)

// view renders the message for the given width.
func (m message) view(width int) string {
	switch m.Kind {
	case kindUser:
		return "\n" + gutter(styleLabel.Bold(true).Render("❯ "), styleLabel, m.Text, width) + "\n"
	case kindAssistant:
		return gutter(replyMark(m.Cont), styleText, m.Text, width)
	case kindTool:
		return toolView(m.Tool, m.Dir, width)
	case kindTools:
		return groupView(m.Tools, m.Dir, width)
	case kindDetail:
		return detailView(m.Tool, m.Dir, width)
	case kindError:
		return gutter(styleError.Render("✗ "), styleError, m.Text, width)
	case kindSwitch:
		return gutter(styleAccent.Render("⇄ "), styleAccent, m.Text, width)
	case kindAlert:
		return gutter(styleAccent.Render("◆ "), styleAccent, m.Text, width)
	case kindRound:
		return "\n" + styleAccent.Render("── "+m.Text+" ──")
	default:
		return gutter(styleMuted.Render("· "), styleMuted, m.Text, width)
	}
}

// replyMark starts a reply; the rest of a reply printed in pieces is only
// indented.
func replyMark(cont bool) string {
	if cont {
		return "  "
	}
	return styleBorder.Render("┊ ")
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

// toolView is `● Name("preview")` and, once done, one dim result line.
func toolView(t *api.ToolCall, dir string, width int) string {
	dot, label := toolMark(t)
	head := dot + label.Render(ansi.Truncate(toolLabel(t, dir), max(width-2, 10), "…"))
	if t.Output == "" {
		return head
	}
	if t.Name == "todo_list" && !t.IsError {
		return head + "\n" + taskList(t.Output, width)
	}
	if t.Name == "delegate_task" && !t.IsError {
		return head + "\n" + subagentList(t.Output, width)
	}
	result := strings.Join(strings.Fields(firstLine(shortPaths(t.Output, dir))), " ") // tabs and runs of spaces
	if n := strings.Count(strings.TrimSpace(t.Output), "\n"); n > 0 {
		result += fmt.Sprintf(" (+%d lines)", n)
	}
	return head + "\n" + styleDim.Render(ansi.Truncate("  └─ "+result, max(width, 10), "…"))
}

// groupView is one line for routine calls in a row (`● Inspected: Read ×3,
// Grep`) and one dim line of what they looked at.
func groupView(calls []*api.ToolCall, dir string, width int) string {
	var names, previews []string
	counts := map[string]int{}
	for _, c := range calls {
		if counts[c.Name] == 0 {
			names = append(names, c.Name)
		}
		counts[c.Name]++
		if p := toolPreview(c, dir); p != "" {
			previews = append(previews, p)
		}
	}
	for i, n := range names {
		if counts[n] > 1 {
			names[i] = fmt.Sprintf("%s ×%d", n, counts[n])
		}
	}
	head := styleAccent.Render("● ") + styleText.Render(ansi.Truncate("Inspected: "+strings.Join(names, ", "), max(width-2, 10), "…"))
	if len(previews) == 0 {
		return head
	}
	return head + "\n" + styleDim.Render(ansi.Truncate("  └─ "+strings.Join(previews, ", "), max(width, 10), "…"))
}

// toolMark is a call's dot and label style: a red cross for a failure.
func toolMark(t *api.ToolCall) (string, lipgloss.Style) {
	if t.IsError {
		return styleError.Render("✗ "), styleError
	}
	return styleAccent.Render("● "), styleText
}

// toolLabel is Name("preview"), with the most telling argument as preview.
func toolLabel(t *api.ToolCall, dir string) string {
	var args map[string]any
	json.Unmarshal(t.Args, &args)
	if tasks, ok := args["tasks"].([]any); ok && t.Name == "delegate_task" {
		return fmt.Sprintf("%s(%d tasks)", t.Name, len(tasks))
	}
	if label := memoryLabel(t, args); label != "" {
		return label
	}
	if p := toolPreview(t, dir); p != "" {
		return fmt.Sprintf("%s(%q)", t.Name, p)
	}
	return t.Name
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

// maxTaskLines is how much of a task list the transcript shows.
const maxTaskLines = 20

// taskList shows the model's task list under its call: open items plain,
// finished ones dim.
func taskList(list string, width int) string {
	lines := strings.Split(strings.TrimSpace(list), "\n")
	more := len(lines) - maxTaskLines
	if more > 0 {
		lines = lines[:maxTaskLines]
	}
	for i, l := range lines {
		style := styleText
		if strings.Contains(l, "[x]") || strings.Contains(l, "[-]") {
			style = styleDim
		}
		lines[i] = style.Render(ansi.Truncate("  "+l, max(width, 10), "…"))
	}
	if more > 0 {
		lines = append(lines, styleDim.Render(fmt.Sprintf("  (+%d more)", more)))
	}
	return strings.Join(lines, "\n")
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
		lines = append(lines, styleDim.Render(ansi.Truncate(fmt.Sprintf("  └─ %s. %d-line report: %s", num, n, goal), max(width, 10), "…")))
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
