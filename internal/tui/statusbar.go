package tui

import (
	"fmt"
	"regexp"
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
	busyFaces     = []string{"(｡•́︿•̀｡)", "(◔_◔)", "(¬‿¬)", "( •_•)>⌐■-■", "(⌐■_■)", "(´･_･`)", "◉_◉", "(°ロ°)",
		"( ˘⌣˘)♡", "ヽ(>∀<☆)☆", "٩(๑❛ᴗ❛๑)۶", "(⊙_⊙)", "(¬_¬)", "( ͡° ͜ʖ ͡°)", "ಠ_ಠ"}
	busyVerbs = []string{"pondering", "contemplating", "musing", "cogitating", "ruminating", "deliberating",
		"mulling", "reflecting", "processing", "reasoning", "analyzing", "computing", "synthesizing", "formulating",
		"brainstorming"}
)

// verbWidth keeps the status line still while the verb changes: the
// longest verb and its "…".
const verbWidth = len("brainstorming…")

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
		n := s.Frame / 25 // about every 2.5s, like Hermes
		verb := busyVerbs[n%len(busyVerbs)] + "…"
		state = styleMuted.Render(fmt.Sprintf("%s %s · %s", busyFaces[n%len(busyFaces)],
			verb+strings.Repeat(" ", verbWidth-len([]rune(verb))), shortDuration(time.Since(s.Since))))
	case s.Note != "":
		state = styleWarn.Render(s.Note)
	default:
		state = styleOK.Render("ready")
	}
	// Most important first; drop from the end until it fits.
	parts := []string{state}
	if model := s.model(); model != "" {
		parts = append(parts, styleMuted.Render(modelLabel(model)))
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
	// The title bold in accent; before there is one, the project.
	right, rightStyle := s.Title, styleAccent.Bold(true)
	if right == "" {
		right, rightStyle = s.Project, styleLabel
	}
	if s.Branch != "" {
		right += " (" + s.Branch + ")"
	}
	lead := styleBorder.Render("─ ")
	for {
		left := lead + strings.Join(parts, styleMuted.Render(" │ "))
		room := width - lipgloss.Width(left) - 3 - 2 // a gap of 3, then "─ "
		if right != "" && room >= min(minRight, lipgloss.Width(right)) {
			r := lead + rightStyle.Render(truncateLeft(right, room))
			return left + strings.Repeat(" ", width-lipgloss.Width(left)-lipgloss.Width(r)) + r
		}
		if len(parts) <= keep {
			return ansi.Truncate(left, max(width, 1), "…")
		}
		parts = parts[:len(parts)-1]
	}
}

// modelLabel writes a model name short, like Hermes: claude-opus-5-5 is
// "opus 5.5", gpt-6.1-sol is "gpt 6.1 sol".
func modelLabel(model string) string {
	model = strings.TrimPrefix(model[strings.LastIndex(model, "/")+1:], "claude-")
	model = strings.NewReplacer("-", " ", "_", " ").Replace(model)
	return reVersionPair.ReplaceAllString(model, "$1.$2")
}

var reVersionPair = regexp.MustCompile(`\b(\d+) (\d+)\b`)

// model is the model of the last turn, or the engine before one ends.
func (s statusBar) model() string {
	if s.Context != nil && s.Context.Model != "" {
		return s.Context.Model
	}
	return s.Engine
}

// contextUse renders "57.3k/200k │ [███░░░░░░░] 28%", the bar colored
// by how full it is like Hermes, or only the used tokens when the window
// is unknown.
func contextUse(c api.Context) string {
	if c.Window <= 0 {
		return styleMuted.Render(shortCount(c.Used))
	}
	pct := min(100*c.Used/c.Window, 100)
	full := (pct*10 + 50) / 100
	bar := strings.Repeat("█", full) + strings.Repeat("░", 10-full)
	color := colorStatusGood
	switch {
	case pct >= 95:
		color = colorStatusCritical
	case pct > 80:
		color = colorStatusBad
	case pct >= 50:
		color = colorStatusWarn // compaction starts at half the window
	}
	return styleMuted.Render(shortCount(c.Used)+"/"+shortCount(c.Window)+" │ ") +
		lipgloss.NewStyle().Foreground(color).Render(fmt.Sprintf("[%s] %d%%", bar, pct))
}

// shortCount writes a token count like Hermes: 950, 57.3k or 1.2M.
func shortCount(n int) string {
	scaled := func(v float64, unit string) string {
		return strings.TrimSuffix(fmt.Sprintf("%.1f", v), ".0") + unit
	}
	switch {
	case n >= 999_950:
		return scaled(float64(n)/1_000_000, "M")
	case n >= 1000:
		return scaled(float64(n)/1000, "k")
	}
	return fmt.Sprint(n)
}

// shortDuration writes a duration as 45s, 12m 5s, 3h 5m or 2d 3h.
func shortDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		if sec := int(d.Seconds()) % 60; sec > 0 {
			return fmt.Sprintf("%dm %ds", int(d.Minutes()), sec)
		}
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
}
