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

// banner is the intro printed once at the top: logo, tagline, info box.
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
	// Border (2) + padding (4) + label (8) + one spare column (writing the last
	// column makes some terminals wrap): the rest is for the value, so the
	// box never gets wider than the terminal.
	valueWidth := max(width-15, 8)
	info := []string{
		styleLabel.Render("project ") + styleText.Render(truncateLeft(b.Project, valueWidth)),
		styleLabel.Render("session ") + styleText.Render(session),
		styleLabel.Render("engine  ") + styleAccent.Render(b.Engine),
	}
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorBorder).Padding(0, 2)
	out = append(out, box.Render(strings.Join(info, "\n")))
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
