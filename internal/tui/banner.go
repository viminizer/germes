package tui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

var logo = []string{
	` ██████╗ ███████╗██████╗ ███╗   ███╗███████╗███████╗`,
	`██╔════╝ ██╔════╝██╔══██╗████╗ ████║██╔════╝██╔════╝`,
	`██║  ███╗█████╗  ██████╔╝██╔████╔██║█████╗  ███████╗`,
	`██║   ██║██╔══╝  ██╔══██╗██║╚██╔╝██║██╔══╝  ╚════██║`,
	`╚██████╔╝███████╗██║  ██║██║ ╚═╝ ██║███████╗███████║`,
	` ╚═════╝ ╚══════╝╚═╝  ╚═╝╚═╝     ╚═╝╚══════╝╚══════╝`,
}

// banner is the intro printed once at the top: logo, version, info box.
type banner struct {
	Version string
	Project string
	Session string // "" = new session
	Engine  string
}

// view renders the banner for the given width. Narrow terminals get a
// one-line title instead of the logo.
func (b banner) view(width int) string {
	var out []string
	if width >= lipgloss.Width(logo[0])+2 {
		// Gradient by rows, like Hermes: primary, accent, border.
		for i, row := range logo {
			style := []lipgloss.Style{stylePrimary, stylePrimary, styleAccent, styleAccent, styleBorder, styleBorder}[i]
			out = append(out, style.Render(row))
		}
	} else {
		out = append(out, stylePrimary.Bold(true).Render("GERMES"))
	}
	out = append(out, styleMuted.Render("☤ Germes "+b.Version))

	session := b.Session
	if session == "" {
		session = "new"
	}
	// Like the Hermes info box: the engine, the folder, the session, and
	// where the commands are. One spare column: writing the last column
	// makes some terminals wrap.
	boxWidth := max(width-1, 20)
	valueWidth := boxWidth - 2 - 6 // border, padding
	info := []string{
		stylePrimary.Render(b.Engine),
		styleLabel.Render(truncateLeft(b.Project, valueWidth)),
		styleLabel.Render("Session: ") + styleStatus.Render(session),
		"",
		styleText.Render("/help for commands"),
	}
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorBorder).Padding(1, 3).Width(boxWidth)
	out = append(out, "", box.Render(strings.Join(info, "\n")))
	return strings.Join(out, "\n")
}

// truncateLeft keeps the end of s (the useful part of a path).
func truncateLeft(s string, width int) string {
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	return "…" + string(r[len(r)-width+1:])
}
