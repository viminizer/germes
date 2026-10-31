package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/viminizer/germes/internal/api"
)

// Diff colors of the Hermes dark theme (hermes/ui-tui/src/theme.ts DIFF_DARK).
var (
	styleRemoved = lipgloss.NewStyle().Foreground(lipgloss.Color("#CF222E")).Background(lipgloss.Color("#FFDCDC"))
	styleAdded   = lipgloss.NewStyle().Foreground(lipgloss.Color("#248A3D")).Background(lipgloss.Color("#DCFFDC"))
)

const (
	maxDiffLines = 30  // shown per change; the rest is counted
	maxDiffInput = 400 // lines per side compared line by line; longer is shown as removed and added
)

// change is the old and new text of one edit.
type change struct{ old, new string }

// changes reads what an Edit, MultiEdit or Write call changed from its
// arguments; none for other calls and failed ones.
func changes(t *api.ToolCall) []change {
	if t == nil || t.IsError {
		return nil
	}
	var a struct {
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
		Content   string `json:"content"`
		Edits     []struct {
			OldString string `json:"old_string"`
			NewString string `json:"new_string"`
		} `json:"edits"`
	}
	if json.Unmarshal(t.Args, &a) != nil {
		return nil
	}
	switch t.Name {
	case "Edit":
		return []change{{a.OldString, a.NewString}}
	case "MultiEdit":
		var out []change
		for _, e := range a.Edits {
			out = append(out, change{e.OldString, e.NewString})
		}
		return out
	case "Write":
		return []change{{"", a.Content}}
	}
	return nil
}

// diffView draws a call's change like Hermes' inline diff: the file, then
// removed lines red, added lines green and unchanged ones plain, cut at
// maxDiffLines. "" when the call changed nothing it can show.
func diffView(t *api.ToolCall, dir string, width int) string {
	cs := changes(t)
	if len(cs) == 0 {
		return ""
	}
	var path struct {
		FilePath string `json:"file_path"`
	}
	json.Unmarshal(t.Args, &path)
	var rows []string
	for _, c := range cs {
		rows = append(rows, diffLines(splitLines(c.old), splitLines(c.new))...)
	}
	if len(rows) == 0 {
		return ""
	}
	fit := func(s string) string { return ansi.Truncate(s, max(width, 1), "…") }
	lines := []string{fit(styleText.Render(shortPaths(path.FilePath, dir)))}
	for i, r := range rows {
		if i == maxDiffLines {
			lines = append(lines, styleDim.Render(fmt.Sprintf("… %d more lines", len(rows)-i)))
			break
		}
		r = strings.ReplaceAll(r, "\t", "    ")
		switch r[0] {
		case '-':
			r = styleRemoved.Render(ansi.Truncate(r, max(width, 1), "…"))
		case '+':
			r = styleAdded.Render(ansi.Truncate(r, max(width, 1), "…"))
		default:
			r = fit(styleText.Render(r))
		}
		lines = append(lines, r)
	}
	return strings.Join(lines, "\n")
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// diffLines compares old and new line by line (longest common
// subsequence) and returns "-", "+" and " " rows. Long sides are shown
// whole as removed, then added.
func diffLines(old, new []string) []string {
	var rows []string
	if len(old) > maxDiffInput || len(new) > maxDiffInput {
		for _, l := range old {
			rows = append(rows, "- "+l)
		}
		for _, l := range new {
			rows = append(rows, "+ "+l)
		}
		return rows
	}
	// lcs[i][j] is the common length of old[i:] and new[j:].
	lcs := make([][]int, len(old)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(new)+1)
	}
	for i := len(old) - 1; i >= 0; i-- {
		for j := len(new) - 1; j >= 0; j-- {
			if old[i] == new[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	i, j := 0, 0
	for i < len(old) || j < len(new) {
		switch {
		case i < len(old) && j < len(new) && old[i] == new[j]:
			rows = append(rows, "  "+old[i])
			i, j = i+1, j+1
		case i < len(old) && (j == len(new) || lcs[i+1][j] >= lcs[i][j+1]):
			rows = append(rows, "- "+old[i]) // removed lines first, like a diff
			i++
		default:
			rows = append(rows, "+ "+new[j])
			j++
		}
	}
	return rows
}
