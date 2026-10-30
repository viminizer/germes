package tui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// titleRule is the thin rule above the input, with the session title (or
// the project before there is one) on the right.
type titleRule struct {
	Title   string
	Project string
}

// view renders the rule. A long title loses its start.
func (t titleRule) view(width int) string {
	right := t.Project
	if t.Title != "" {
		right = t.Title
	}
	if right == "" || width < 12 {
		return styleBorder.Render(strings.Repeat("─", max(width, 1)))
	}
	r := styleLabel.Render(truncateLeft(right, width-4))
	return styleBorder.Render(strings.Repeat("─", max(width-lipgloss.Width(r)-1, 1))+" ") + r
}
