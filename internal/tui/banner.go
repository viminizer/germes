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

// caduceus is the Hermes session box art (hermes/ui-tui/src/banner.ts).
var caduceus = []string{
	`⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⢀⣀⡀⠀⣀⣀⠀⢀⣀⡀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀`,
	`⠀⠀⠀⠀⠀⠀⢀⣠⣴⣾⣿⣿⣇⠸⣿⣿⠇⣸⣿⣿⣷⣦⣄⡀⠀⠀⠀⠀⠀⠀`,
	`⠀⢀⣠⣴⣶⠿⠋⣩⡿⣿⡿⠻⣿⡇⢠⡄⢸⣿⠟⢿⣿⢿⣍⠙⠿⣶⣦⣄⡀⠀`,
	`⠀⠀⠉⠉⠁⠶⠟⠋⠀⠉⠀⢀⣈⣁⡈⢁⣈⣁⡀⠀⠉⠀⠙⠻⠶⠈⠉⠉⠀⠀`,
	`⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⣴⣿⡿⠛⢁⡈⠛⢿⣿⣦⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀`,
	`⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠿⣿⣦⣤⣈⠁⢠⣴⣿⠿⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀`,
	`⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠈⠉⠻⢿⣿⣦⡉⠁⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀`,
	`⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠘⢷⣦⣈⠛⠃⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀`,
	`⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⢠⣴⠦⠈⠙⠿⣦⡄⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀`,
	`⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠸⣿⣤⡈⠁⢤⣿⠇⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀`,
	`⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠉⠛⠷⠄⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀`,
	`⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⢀⣀⠑⢶⣄⡀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀`,
	`⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⣿⠁⢰⡆⠈⡿⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀`,
	`⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠈⠳⠈⣡⠞⠁⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀`,
	`⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠈⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀⠀`,
}

// Art colors by row, like Hermes: the logo goes primary, accent, border;
// the caduceus border, accent, primary and back, its staff muted.
var (
	logoStyles     = []lipgloss.Style{stylePrimary, stylePrimary, styleAccent, styleAccent, styleBorder, styleBorder}
	caduceusStyles = []lipgloss.Style{styleBorder, styleBorder, styleAccent, styleAccent, stylePrimary, stylePrimary,
		styleAccent, styleAccent, styleBorder, styleBorder, styleMuted, styleMuted, styleMuted, styleMuted, styleMuted}
)

// banner is the intro printed once at the top: the logo, then the session
// box of the Hermes TUI (hermes/ui-tui/src/components/branding.tsx).
type banner struct {
	Version string
	Project string
	Session string // "" = new session
	Engine  string
}

// view renders the banner for the given width. Narrow terminals get the
// name instead of the logo, and the box drops the art.
func (b banner) view(width int) string {
	var out []string
	if width >= lipgloss.Width(logo[0])+2 {
		for i, row := range logo {
			out = append(out, logoStyles[i].Render(row))
		}
		out = append(out, styleMuted.Render("☤ Germes"))
	} else {
		out = append(out, stylePrimary.Bold(true).Render("☤ Germes"))
	}
	return strings.Join(append(out, "", b.box(width)), "\n")
}

// box is the session box: on a wide terminal the art with the engine,
// folder and session on the left and the name and version on the right;
// on a narrow one the right column only, with the engine, folder and
// session on top.
func (b banner) box(width int) string {
	session := b.Session
	if session == "" {
		session = "new"
	}
	artWidth := lipgloss.Width(caduceus[0])
	leftWidth := min(artWidth+4, width*2/5)
	wide := width >= 90 && leftWidth+40 < width
	infoWidth, projectWidth := max(20, width-12), max(20, width-12)
	if wide {
		infoWidth, projectWidth = max(20, width-leftWidth-14), leftWidth
	}
	about := []string{
		styleAccent.Render(b.Engine),
		styleMuted.Render(truncateLeft(b.Project, projectWidth)),
		styleLabel.Render("Session: ") + lipgloss.NewStyle().Foreground(colorSessionID).Render(session),
	}
	help := styleMuted.Render("/help for commands")
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorBorder).Padding(1, 2)
	if !wide {
		return box.Width(infoWidth + 6).Render(strings.Join(append(about, "", help), "\n"))
	}
	var left []string
	for i, row := range caduceus {
		left = append(left, caduceusStyles[i].Render(row))
	}
	left = append(append(left, ""), about...)
	title := lipgloss.PlaceHorizontal(infoWidth, lipgloss.Center, stylePrimary.Bold(true).Render("Germes v"+b.Version))
	right := []string{title, "", help}
	column := lipgloss.NewStyle().Width(leftWidth).Render(strings.Join(left, "\n"))
	return box.Width(leftWidth + 2 + infoWidth + 6).Render(lipgloss.JoinHorizontal(lipgloss.Top, column, "  ", strings.Join(right, "\n")))
}

// truncateLeft keeps the end of s (the useful part of a path).
func truncateLeft(s string, width int) string {
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	return "…" + string(r[len(r)-width+1:])
}
