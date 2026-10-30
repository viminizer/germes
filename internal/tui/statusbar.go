package tui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/viminizer/germes/internal/api"
)

var (
	spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	busyFaces     = []string{"(◔_◔)", "(・_・ヾ", "(￣ー￣)", "(⊙_⊙)"}
)

// keyHints are the footer's right side, the first part to drop.
const keyHints = "/ commands · @ files"

// statusBar is the footer under the input: state, model, context used,
// session length, last turn time, branch, and key hints on the right.
type statusBar struct {
	Busy     bool
	Since    time.Time // when the turn started
	Frame    int       // spinner frame
	Engine   string
	Context  *api.Context  // after a turn: how full the model's context was
	Started  time.Time     // when the session was created; zero = not yet
	LastTurn time.Duration // how long the last turn took; 0 = none yet
	Branch   string        // the session's worktree branch, "" = none
	Note     string        // short message, e.g. "interrupted" or an error
	Hint     string        // shown instead of the state for a moment, e.g. "Esc again to stop"
}

// spinner returns the current spinner frame.
func (s statusBar) spinner() string { return spinnerFrames[s.Frame%len(spinnerFrames)] }

// view renders the footer on one line. When narrow, the key hints drop
// first, then the parts after the model and context, from the end.
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
	// Most important first; drop from the end until it fits.
	parts := []string{state}
	if model := s.model(); model != "" {
		parts = append(parts, styleStatus.Render(model))
	}
	keep := len(parts) // state and model never drop before the context
	if s.Context != nil {
		parts = append(parts, contextUse(*s.Context))
		keep++
	}
	if !s.Started.IsZero() {
		parts = append(parts, styleMuted.Render("session "+shortDuration(time.Since(s.Started))))
	}
	if s.LastTurn > 0 {
		parts = append(parts, styleMuted.Render("turn "+shortDuration(s.LastTurn)))
	}
	if s.Branch != "" {
		parts = append(parts, styleAccent.Render("⎇ "+s.Branch))
	}
	sep := styleMuted.Render(" · ")
	hints := styleMuted.Render(keyHints)
	for {
		left := strings.Join(parts, sep)
		if gap := width - lipgloss.Width(left) - lipgloss.Width(hints); gap >= 4 {
			return left + strings.Repeat(" ", gap) + hints
		}
		if lipgloss.Width(left) <= width || len(parts) <= keep {
			return ansi.Truncate(left, max(width, 1), "…")
		}
		parts = parts[:len(parts)-1]
	}
}

// model is the model of the last turn, or the engine before one ends.
func (s statusBar) model() string {
	if s.Context != nil && s.Context.Model != "" {
		return s.Context.Model
	}
	return s.Engine
}

// contextUse renders "57K/200K ██░░░░░░ 28%", or only the used tokens when
// the window is unknown.
func contextUse(c api.Context) string {
	if c.Window <= 0 {
		return styleStatus.Render(shortCount(c.Used))
	}
	pct := min(100*c.Used/c.Window, 100)
	full := pct * 8 / 100
	bar := strings.Repeat("█", full) + strings.Repeat("░", 8-full)
	style := styleStatus
	if pct >= 50 {
		style = styleWarn // compaction starts at half the window
	}
	return style.Render(fmt.Sprintf("%s/%s %s %d%%", shortCount(c.Used), shortCount(c.Window), bar, pct))
}

// shortCount writes a token count as 950, 57K or 1.2M.
func shortCount(n int) string {
	switch {
	case n >= 1_000_000:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1_000_000), ".0") + "M"
	case n >= 1000:
		return fmt.Sprintf("%dK", n/1000)
	}
	return fmt.Sprint(n)
}

// shortDuration writes a duration as 45s, 12m, 3h 5m or 2d 3h.
func shortDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
}
