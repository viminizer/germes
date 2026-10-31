package tui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/viminizer/germes/internal/api"
)

// treeView draws a step like Hermes (hermes/ui-tui/src/components/
// thinking.tsx): `├─ ▸ Thinking  ~51 tokens` when the model reasoned (its
// text is in /details), then `└─ ▾ Tool calls (3)` and one
// `├─ ● Read("main.go") (0.2s)` row per call. A running call has the
// spinner and its time so far; a failed one is red with its error under
// it; subagent reports show under their call. maxRows > 0 keeps only the
// last rows under the header (the live area).
func treeView(runs []toolRun, thinking int, dir string, width int, spin string, now time.Time, maxRows int) string {
	fit := func(s string) string { return ansi.Truncate(s, max(width, 1), "…") }
	var heads []string
	if thinking > 0 {
		branch := "├─ "
		if len(runs) == 0 {
			branch = "└─ "
		}
		heads = append(heads, fit(styleDim.Render(branch)+styleAccent.Render("▸ ")+styleDim.Render("Thinking")+
			styleTime.Render(fmt.Sprintf("  ~%s tokens", shortCount(thinking)))))
	}
	if len(runs) == 0 {
		return strings.Join(heads, "\n")
	}
	head := styleDim.Render("└─ ") + styleAccent.Faint(true).Render("▾ ") + styleDim.Render(fmt.Sprintf("Tool calls (%d)", len(runs)))
	var rows []string
	for i, r := range runs {
		branch, rail := "  ├─ ", "  │  "
		if i == len(runs)-1 {
			branch, rail = "  └─ ", "     "
		}
		dot, took := styleAccent.Render("● "), r.Took
		if !r.Done && spin != "" {
			dot += styleAccent.Render(spin + " ")
		}
		if !r.Done {
			took = now.Sub(r.Start)
		}
		row := styleDim.Render(branch) + dot + toolStyle(r.Call).Render(toolLabel(r.Call, dir))
		if r.Done || !now.IsZero() {
			row += styleTime.Render(" (" + tookText(took) + ")")
		}
		rows = append(rows, fit(row))
		var sub string
		switch {
		case !r.Done:
		case r.Call.IsError:
			// Escape codes go first: a line holding only a color code is not the first line.
			sub = styleError.Render("└─ " + cleanLine(firstLine(ansi.Strip(shortPaths(r.Call.Output, dir)))))
		case r.Call.Name == "delegate_task":
			sub = subagentList(r.Call.Output, width-5)
		}
		if sub != "" {
			for _, l := range strings.Split(sub, "\n") {
				rows = append(rows, fit(styleDim.Render(rail)+l))
			}
		}
	}
	if maxRows > 0 && len(rows) > maxRows {
		cut := len(rows) - maxRows + 1 // one row says how many are hidden
		rows = append([]string{fit(styleDim.Render(fmt.Sprintf("  │ … %d rows above", cut)))}, rows[cut:]...)
	}
	return strings.Join(append(append(heads, fit(head)), rows...), "\n")
}

// tookText writes a call's time as 0.2s, 12.5s or 2m.
func tookText(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", max(d.Seconds(), 0))
	}
	return shortDuration(d)
}

// toolStyle is a call's label style, like Hermes: red for a failure.
func toolStyle(t *api.ToolCall) lipgloss.Style {
	if t.IsError {
		return styleError
	}
	return styleText
}

// toolName writes a snake_case tool name in words, like Hermes
// (lib/text.ts toolTrailLabel): skill_view is "Skill View",
// mcp__chrome__navigate is "Mcp Chrome Navigate"; Read stays Read.
func toolName(name string) string {
	var words []string
	for _, w := range strings.Split(name, "_") {
		if w != "" {
			words = append(words, strings.ToUpper(w[:1])+w[1:])
		}
	}
	if len(words) == 0 {
		return name
	}
	return strings.Join(words, " ")
}
