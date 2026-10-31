package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/viminizer/germes/internal/api"
)

// maxDetailLines is how much of one tool's arguments or output /details shows.
const maxDetailLines = 40

// showDetails prints the last turn's tool calls in full (arguments and
// output) and the model's reasoning, in order, read from the saved events
// (nothing runs again).
func (m *model) showDetails() tea.Cmd {
	id, dir := m.sessionID, m.transcript.dir
	if id == "" {
		return m.print(message{Kind: kindNotice, Text: "no session yet"})
	}
	return func() tea.Msg {
		events, err := api.ListAllEvents(m.ctx, m.svc, api.EventFilter{SessionID: id})
		if err != nil {
			return printMsg{[]message{{Kind: kindError, Text: err.Error()}}}
		}
		var items []message
		byID := map[string]*api.ToolCall{}
		for _, ev := range events {
			switch {
			case ev.Kind == api.KindUserMessage:
				items, byID = nil, map[string]*api.ToolCall{}
			case ev.Kind == api.KindReasoning:
				items = append(items, message{Kind: kindThought, Text: ev.Text})
			case ev.Kind == api.KindToolCall && ev.Tool != nil:
				c := *ev.Tool
				items = append(items, message{Kind: kindDetail, Tool: &c, Dir: dir})
				byID[c.ID] = &c
			case ev.Kind == api.KindToolResult && ev.Tool != nil && byID[ev.Tool.ID] != nil:
				byID[ev.Tool.ID].Output, byID[ev.Tool.ID].IsError = ev.Tool.Output, ev.Tool.IsError
			}
		}
		if len(items) == 0 {
			return printMsg{[]message{{Kind: kindNotice, Text: "no tool calls or reasoning in the last turn"}}}
		}
		head := fmt.Sprintf("the last turn's %d tool calls in full", len(byID))
		switch {
		case len(byID) == 0:
			head = "the last turn's reasoning"
		case len(byID) < len(items):
			head += ", and its reasoning"
		}
		return printMsg{append([]message{{Kind: kindNotice, Text: head}}, items...)}
	}
}

// detailView is one tool call in full: its name, its arguments (a command
// as `$ command`, others as one line of JSON) and its output, each cut at
// maxDetailLines.
func detailView(t *api.ToolCall, dir string, width int) string {
	label := toolStyle(t)
	var args struct {
		Command string `json:"command"`
	}
	json.Unmarshal(t.Args, &args)
	in := "$ " + args.Command
	if args.Command == "" {
		in = string(t.Args)
	}
	lines := []string{styleAccent.Render("● ") + label.Render(toolName(t.Name))}
	lines = append(lines, detailLines(shortPaths(in, dir), styleText, width)...)
	if t.Output != "" {
		lines = append(lines, detailLines(shortPaths(t.Output, dir), styleDim, width)...)
	}
	return strings.Join(lines, "\n")
}

// thoughtView is the model's reasoning in full (/details): what Hermes
// shows when its Thinking row is opened.
func thoughtView(text string, width int) string {
	lines := []string{styleAccent.Render("▾ ") + styleDim.Render("Thinking")}
	return strings.Join(append(lines, detailLines(text, styleDim, width)...), "\n")
}

// detailLines indents text, cuts long lines at the width and stops after
// maxDetailLines with a count of the rest.
func detailLines(text string, style lipgloss.Style, width int) []string {
	all := strings.Split(strings.TrimRight(text, "\n"), "\n")
	shown := all[:min(len(all), maxDetailLines)]
	var out []string
	for _, l := range shown {
		out = append(out, style.Render(ansi.Truncate("  "+cleanLine(l), max(width, 10), "…")))
	}
	if more := len(all) - len(shown); more > 0 {
		out = append(out, styleDim.Render(fmt.Sprintf("  … (+%d more lines)", more)))
	}
	return out
}
