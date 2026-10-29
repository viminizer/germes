package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// maxQueueLines is how many queued messages are shown before "+N more".
const maxQueueLines = 3

// queueView shows the messages waiting for the next turn, like Hermes:
// `↳ text` in muted, one line each. They run together as one turn.
type queueView struct {
	items []string
}

// view renders the queue, or "" when it is empty.
func (q queueView) view(width int) string {
	if len(q.items) == 0 {
		return ""
	}
	var lines []string
	for i, item := range q.items {
		if i == maxQueueLines {
			lines = append(lines, styleDim.Render(fmt.Sprintf("  +%d more", len(q.items)-i)))
			break
		}
		lines = append(lines, styleLabel.Render("↳ ")+styleMuted.Render(ansi.Truncate(firstLine(item), max(width-2, 10), "…")))
	}
	lines = append(lines, styleDim.Render(ansi.Truncate("  queued: sent together when this turn ends · Esc drops them", max(width, 10), "…")))
	return strings.Join(lines, "\n")
}
