package tui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/viminizer/germes/internal/api"
)

// The busy face and verb change every few seconds, like Hermes
// (hermes/ui-tui/src/content/verbs.ts, i18n/en/content.ts).
var (
	spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	busyFaces     = []string{"(◔_◔)", "( •_•)>⌐■-■", "(⌐■_■)", "(´･_･`)", "◉_◉", "(⊙_⊙)", "(¬‿¬)", "ಠ_ಠ"}
	busyVerbs     = []string{"pondering", "contemplating", "musing", "cogitating", "ruminating", "deliberating",
		"mulling", "reflecting", "processing", "reasoning", "analyzing", "computing", "synthesizing", "formulating"}
)

// statusBar is the line over the input, like the Hermes status rule:
// state, model, context used, session length and last turn time on the
// left; the session title (the project before there is one) and branch on
// the right.
type statusBar struct {
	Busy     bool
	Since    time.Time // when the turn started
	Frame    int       // spinner frame
	Engine   string
	Context  *api.Context  // after a turn: how full the model's context was
	Started  time.Time     // when the session was created; zero = not yet
	LastTurn time.Duration // how long the last turn took; 0 = none yet
	Branch   string        // the session's worktree branch, "" = none
	Title    string        // the session's title, "" = none yet
	Project  string
	Note     string // short message, e.g. "interrupted" or an error
	Hint     string // shown instead of the state for a moment, e.g. "Esc again to stop"
}

// spinner returns the current spinner frame.
func (s statusBar) spinner() string { return spinnerFrames[s.Frame%len(spinnerFrames)] }

// minRight is the narrowest the title on the right gets before it drops.
const minRight = 12

// view renders the line. When narrow, the title on the right gets shorter,
// then the left drops its parts after the model and context from the end,
// then the title goes.
func (s statusBar) view(width int) string {
	var state string
	switch {
	case s.Hint != "":
		state = styleAccent.Bold(true).Render(s.Hint)
	case s.Busy:
		n := s.Frame / 25 // about every 2.5s
		state = styleMuted.Render(fmt.Sprintf("%s %s… · %s", busyFaces[n%len(busyFaces)], busyVerbs[n%len(busyVerbs)],
			time.Since(s.Since).Round(time.Second)))
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
	if s.Context != nil {
		parts = append(parts, contextUse(*s.Context))
	}
	keep := len(parts) // state, model and context never drop
	if !s.Started.IsZero() {
		parts = append(parts, styleMuted.Render(shortDuration(time.Since(s.Started))))
	}
	if s.LastTurn > 0 {
		parts = append(parts, styleMuted.Render("✓ "+shortDuration(s.LastTurn)))
	}
	right := s.Title
	if right == "" {
		right = s.Project
	}
	if s.Branch != "" {
		right += " (" + s.Branch + ")"
	}
	lead := styleBorder.Render("─ ")
	for {
		left := lead + strings.Join(parts, styleMuted.Render(" │ "))
		room := width - lipgloss.Width(left) - 3 - 2 // a gap of 3, then "─ "
		if right != "" && room >= min(minRight, lipgloss.Width(right)) {
			r := lead + styleLabel.Render(truncateLeft(right, room))
			return left + strings.Repeat(" ", width-lipgloss.Width(left)-lipgloss.Width(r)) + r
		}
		if len(parts) <= keep {
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

// contextUse renders "57K/200K │ [██░░░░░░░░] 28%", or only the used
// tokens when the window is unknown.
func contextUse(c api.Context) string {
	if c.Window <= 0 {
		return styleStatus.Render(shortCount(c.Used))
	}
	pct := min(100*c.Used/c.Window, 100)
	full := pct * 10 / 100
	bar := strings.Repeat("█", full) + strings.Repeat("░", 10-full)
	style := styleStatus
	if pct >= 50 {
		style = styleWarn // compaction starts at half the window
	}
	return styleStatus.Render(shortCount(c.Used)+"/"+shortCount(c.Window)) + styleMuted.Render(" │ ") +
		style.Render(fmt.Sprintf("[%s] %d%%", bar, pct))
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
