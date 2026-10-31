package tui

import (
	"fmt"
	"os"
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

// busyWidth is the room the busy state is given: the widest face, the
// verb and the turn's time (Hermes' busyIndicatorWidth).
var busyWidth = func() int {
	face := 0
	for _, f := range busyFaces {
		face = max(face, lipgloss.Width(f))
	}
	return face + 1 + verbWidth + len(" · ") + timeWidth
}()

// statusBar is the line over the input, like the Hermes status rule
// (hermes/ui-tui/src/components/appChrome.tsx): state, model and context
// used on the left, then the extras that fit (context bar, session age,
// time since the last turn, cache hit rate); on the right the session title,
// or the folder and its git branch before there is one.
type statusBar struct {
	Busy    bool
	Since   time.Time // when the turn started
	Frame   int       // spinner frame
	Engine  string
	Context *api.Context // after a turn: how full the model's context was
	// CacheInput and CacheRead add up the session's model calls seen here:
	// input tokens, and of those the ones from the prompt cache.
	CacheInput, CacheRead int
	Started               time.Time // when the session started; zero = not yet
	LastEnd               time.Time // when the last turn ended; zero = none yet
	Dir                   string    // the session's folder
	Branch                string    // its git branch, "" = none
	Title                 string    // the session's title, "" = none yet
	Note                  string    // short message, e.g. "interrupted" or an error
	Hint                  string    // shown instead of the state for a moment, e.g. "Esc again to stop"
}

// spinner returns the current spinner frame.
func (s statusBar) spinner() string { return spinnerFrames[s.Frame%len(spinnerFrames)] }

// Widths at which parts show, like Hermes (appChrome.tsx statusBarSegments).
const (
	compactBelow  = 72 // narrower: the context as "16.8k tok" and no bar
	durationFrom  = 76 // session age and time since the last turn
	cacheFrom     = 96
	maxFolderText = 28 // the folder and branch on the right
	timeWidth     = 7  // room a time is given ("59m 59s"), like Hermes' MAX_DURATION_WIDTH
)

// view renders the line like Hermes: state, model and context always;
// then the right side gets its room (the title is kept whole when it can
// be); then the extras in order, each only if it fits whole.
func (s statusBar) view(width int, now time.Time) string {
	sep := styleMuted.Render(" │ ")
	left := styleBorder.Render("─ ") + s.state(now)
	if model := s.model(); model != "" {
		left += sep + styleMuted.Render(modelLabel(model))
	}
	if s.Context != nil {
		left += sep + styleMuted.Render(contextUsed(*s.Context, width < compactBelow))
	}
	// While busy the state is given the room of its widest face, verb and
	// time, like Hermes: the line keeps still as they change.
	used := lipgloss.Width(left)
	if s.Busy {
		used += max(0, busyWidth-lipgloss.Width(s.state(now)))
	}
	right, rightStyle := " "+s.Title+" ", styleAccent.Bold(true)
	if s.Title == "" {
		right, rightStyle = folderLabel(s.Dir, s.Branch, maxFolderText), styleLabel
	}
	gap, divider, minLeft := 3, " ─ ", 8
	if width < 24 { // like Hermes: a narrow line has a one-column gap
		gap, divider, minLeft = 1, " ", 1
	}
	rightWidth := 0
	if strings.TrimSpace(right) != "" {
		rightWidth = max(0, min(lipgloss.Width(right), width-gap-max(minLeft, used)))
	}
	leftWidth := width
	if rightWidth > 0 {
		leftWidth = width - gap - rightWidth
	}
	budget := leftWidth - used
	// A part shows only if the room it is given fits (a time gets timeWidth,
	// so the line does not change as it counts up).
	add := func(part string, room int) {
		if w := lipgloss.Width(sep) + room; w <= budget {
			left, budget = left+sep+part, budget-w
		}
	}
	if s.Context != nil && s.Context.Window > 0 && width >= compactBelow {
		bar := contextBar(*s.Context)
		add(bar, lipgloss.Width(bar))
	}
	if width >= durationFrom && !s.Started.IsZero() {
		add(styleMuted.Render(shortDuration(now.Sub(s.Started))), timeWidth)
	}
	if width >= durationFrom && !s.Busy && !s.LastEnd.IsZero() {
		add(styleMuted.Render("✓ "+shortDuration(now.Sub(s.LastEnd))), 2+timeWidth) // idle since, like Hermes
	}
	if width >= cacheFrom && s.CacheRead > 0 && s.CacheInput > 0 {
		hit := cacheHit(s.CacheRead, s.CacheInput)
		add(hit, lipgloss.Width(hit))
	}
	left = ansi.Truncate(left, max(leftWidth, 1), "…")
	if rightWidth == 0 {
		return left
	}
	return left + strings.Repeat(" ", max(leftWidth-lipgloss.Width(left), 0)) + styleBorder.Render(divider) +
		rightStyle.Render(ansi.Truncate(right, rightWidth, "…"))
}

// state is "ready", the busy face and verb with the turn's time, a hint
// or a note.
func (s statusBar) state(now time.Time) string {
	switch {
	case s.Hint != "":
		return styleAccent.Bold(true).Render(s.Hint)
	case s.Busy:
		n := s.Frame / 25 // about every 2.5s, like Hermes
		verb := busyVerbs[n%len(busyVerbs)] + "…"
		return styleMuted.Render(fmt.Sprintf("%s %s · %s", busyFaces[n%len(busyFaces)],
			verb+strings.Repeat(" ", verbWidth-len([]rune(verb))), shortDuration(now.Sub(s.Since))))
	case s.Note != "":
		return styleWarn.Render(s.Note)
	}
	return styleOK.Render("ready")
}

// folderLabel is the folder like Hermes (domain/paths.ts fmtCwdBranch):
// under the home folder as ~, its start cut to fit max with the branch.
func folderLabel(dir, branch string, maxWidth int) string {
	if dir == "" {
		return ""
	}
	if home := os.Getenv("HOME"); home != "" && strings.HasPrefix(dir, home) {
		dir = "~" + dir[len(home):]
	}
	tag := ""
	if branch != "" {
		if r := []rune(branch); len(r) > 16 {
			branch = "…" + string(r[len(r)-15:])
		}
		tag = " (" + branch + ")"
	}
	return truncateLeft(dir, max(8, maxWidth-len([]rune(tag)))) + tag
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

// contextUsed is "57.3k/200k", or "57.3k tok" when compact or when the
// window is unknown, like Hermes.
func contextUsed(c api.Context, compact bool) string {
	if compact || c.Window <= 0 {
		return shortCount(c.Used) + " tok"
	}
	return shortCount(c.Used) + "/" + shortCount(c.Window)
}

// contextBar is "[███░░░░░░░] 28%", colored by how full the context is
// like Hermes.
func contextBar(c api.Context) string {
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
	return lipgloss.NewStyle().Foreground(color).Render(fmt.Sprintf("[%s] %d%%", bar, pct))
}

// cacheHit is "◎ 65%": how much of the session's input the prompt cache
// served, colored like Hermes.
func cacheHit(read, input int) string {
	pct := min(100, (200*read+input)/(2*input)) // rounded
	style := styleMuted
	switch {
	case pct >= 70:
		style = lipgloss.NewStyle().Foreground(colorStatusGood)
	case pct >= 40:
		style = lipgloss.NewStyle().Foreground(colorStatusWarn)
	}
	return style.Render(fmt.Sprintf("◎ %d%%", pct))
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
