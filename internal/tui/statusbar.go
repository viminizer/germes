package tui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

var (
	spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	busyFaces     = []string{"(◔_◔)", "(・_・ヾ", "(￣ー￣)", "(⊙_⊙)"}
)

// statusBar is the one-line rule above the input: state, engine, session,
// and the project on the right.
type statusBar struct {
	Busy      bool
	Since     time.Time // when the turn started
	Frame     int       // spinner frame
	Engine    string
	SessionID string
	Title     string
	Project   string
	Note      string // short message, e.g. "interrupted" or an error
	Hint      string // shown instead of the state for a moment, e.g. "Esc again to stop"
}

// spinner returns the current spinner frame.
func (s statusBar) spinner() string { return spinnerFrames[s.Frame%len(spinnerFrames)] }

// view renders the bar. When narrow, the least important parts drop first.
func (s statusBar) view(width int) string {
	var state string
	switch {
	case s.Hint != "":
		state = styleAccent.Bold(true).Render(s.Hint)
	case s.Busy:
		face := busyFaces[(s.Frame/20)%len(busyFaces)]
		state = styleMuted.Render(fmt.Sprintf("%s working… · %s", face, time.Since(s.Since).Round(time.Second)))
	case s.Note != "":
		state = styleWarn.Render(s.Note)
	default:
		state = styleOK.Render("ready")
	}
	sep := styleMuted.Render(" │ ")
	// Most important first; drop from the end until it fits.
	parts := []string{state}
	if s.Engine != "" {
		parts = append(parts, styleStatus.Render(s.Engine))
	}
	if s.SessionID != "" {
		parts = append(parts, styleStatus.Render(s.SessionID))
	}
	right := s.Project
	if s.Title != "" {
		right = s.Title
	}
	for {
		left := styleBorder.Render("─ ") + strings.Join(parts, sep)
		gap := width - lipgloss.Width(left) - 3
		if gap >= 10 && right != "" {
			r := truncateLeft(right, gap)
			pad := width - lipgloss.Width(left) - lipgloss.Width(r) - 3
			return left + styleBorder.Render(" "+strings.Repeat("─", max(pad, 1))+" ") + styleLabel.Render(r)
		}
		if len(parts) == 1 || gap >= 0 {
			return ansi.Truncate(left, max(width, 1), "")
		}
		parts = parts[:len(parts)-1]
	}
}
