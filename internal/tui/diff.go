package tui

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"

	"github.com/viminizer/germes/internal/api"
)

const (
	maxDiffLines = 80  // shown per call, like Hermes (agent/display.py); the rest is counted
	maxDiffInput = 400 // lines per side compared line by line; longer is shown as removed and added
)

// edits is what an Edit, MultiEdit or Write call changed, from its
// arguments: the file and each old and new text. A Write shows its whole
// content as added: the TUI cannot see the file before it.
type edits struct {
	path    string
	changes [][2]string
}

// readEdits reads a call's changes; none for other calls and failed ones.
func readEdits(t *api.ToolCall) edits {
	if t == nil || t.IsError {
		return edits{}
	}
	var a struct {
		FilePath  string `json:"file_path"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
		Content   string `json:"content"`
		Edits     []struct {
			OldString string `json:"old_string"`
			NewString string `json:"new_string"`
		} `json:"edits"`
	}
	if json.Unmarshal(t.Args, &a) != nil {
		return edits{}
	}
	e := edits{path: a.FilePath}
	switch t.Name {
	case "Edit":
		e.changes = [][2]string{{a.OldString, a.NewString}}
	case "MultiEdit":
		for _, x := range a.Edits {
			e.changes = append(e.changes, [2]string{x.OldString, x.NewString})
		}
	case "Write":
		e.changes = [][2]string{{"", a.Content}}
	}
	return e
}

// rows are the diff rows of all the call's changes: "-", "+" and "  "
// rows, and "@@" between the changes of a MultiEdit.
func (e edits) rows() []string {
	var rows []string
	for _, c := range e.changes {
		d := diffLines(splitLines(c[0]), splitLines(c[1]))
		if !hasChange(d) {
			continue // like Hermes: no diff when before and after are the same
		}
		if len(rows) > 0 {
			rows = append(rows, "@@")
		}
		rows = append(rows, d...)
	}
	return rows
}

// hasChange says whether diff rows remove or add a line: an edit of only
// line endings or a final newline shows nothing.
func hasChange(rows []string) bool {
	for _, r := range rows {
		if r[0] == '-' || r[0] == '+' {
			return true
		}
	}
	return false
}

// diffView draws a call's change like Hermes' inline diff: the file, then
// removed lines red, added lines green, unchanged ones dim, and `@@`
// between the changes of a MultiEdit; cut at maxDiffLines. "" when the
// call changed nothing it can show.
func diffView(t *api.ToolCall, dir string, width int) string {
	e := readEdits(t)
	rows := e.rows()
	if len(rows) == 0 {
		return ""
	}
	fit := func(s string) string { return ansi.Truncate(s, max(width, 1), "…") }
	lines := []string{styleText.Render(fit(cleanLine(shortPaths(e.path, dir))))}
	for i, r := range rows {
		if i == maxDiffLines {
			lines = append(lines, styleDim.Render(fmt.Sprintf("… %d more lines", len(rows)-i)))
			break
		}
		style := styleUnchanged
		switch r[0] {
		case '-':
			style = styleRemoved
		case '+':
			style = styleAdded
		case '@':
			style = styleMuted
		}
		lines = append(lines, style.Render(fit(r)))
	}
	return strings.Join(lines, "\n")
}

// splitLines splits text into clean lines (cleanLine).
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = cleanLine(l)
	}
	return lines
}

// cleanLine makes a line from a tool or a file safe to print: tabs become
// spaces (a tab is wider on screen than its count, and a line that wraps
// moves the live area), escape sequences and other control characters
// (a CR from a CRLF file, a terminal command in a fixture) go.
func cleanLine(s string) string {
	s = strings.ReplaceAll(ansi.Strip(s), "\t", "    ")
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
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
