package tui

import (
	"regexp"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Markdown in replies, drawn like the Hermes TUI (hermes/ui-tui/src/
// components/markdown.tsx): headings bold in accent, `•` bullets, inline
// code in accent, code blocks indented under a `─ lang` line, tables in
// columns with the header underlined.
var (
	reFence    = regexp.MustCompile("^\\s*(```|~~~)\\s*(\\S*)")
	reHeading  = regexp.MustCompile(`^\s{0,3}#{1,6}\s+(.*?)\s*#*\s*$`)
	reRule     = regexp.MustCompile(`^\s{0,3}([-*_])(\s*[-*_]){2,}\s*$`)
	reBullet   = regexp.MustCompile(`^(\s*)[-*+]\s+(.*)$`)
	reTask     = regexp.MustCompile(`^\[([ xX])\]\s+(.*)$`)
	reNumbered = regexp.MustCompile(`^(\s*)(\d+)[.)]\s+(.*)$`)
	reQuote    = regexp.MustCompile(`^\s*>\s?(.*)$`)
	reTableSep = regexp.MustCompile(`^\s*\|?\s*:?-+:?\s*(\|\s*:?-+:?\s*)*\|?\s*$`)
	reInline   = regexp.MustCompile("`([^`]+)`|\\*\\*([^*]+)\\*\\*|__([^_]+)__|\\[([^\\]]+)\\]\\(([^)\\s]+)\\)")
)

// renderMarkdown draws whole markdown blocks for the given width.
func renderMarkdown(text string, width int) string {
	lines := strings.Split(text, "\n")
	var out []string
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if fence, lang, ok := fenceOpen(line); ok {
			if lang != "" {
				out = append(out, styleMuted.Render("  ─ "+lang))
			}
			for i++; i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), fence); i++ {
				// Wrapped here, so a long line keeps the indent.
				code := ansi.Hardwrap(strings.ReplaceAll(lines[i], "\t", "    "), max(width-2, 10), true)
				for _, part := range strings.Split(code, "\n") {
					out = append(out, "  "+highlight(part, lang))
				}
			}
			continue
		}
		if isTableStart(lines[i:]) {
			start := i
			for i+1 < len(lines) && isTableRow(lines[i+1]) {
				i++
			}
			out = append(out, renderTable(lines[start:i+1], width)...)
			continue
		}
		out = append(out, renderLine(line))
	}
	return strings.Join(out, "\n")
}

// renderLine draws one line that is not part of a code block or table.
func renderLine(line string) string {
	if m := reHeading.FindStringSubmatch(line); m != nil {
		return inline(m[1], styleAccent.Bold(true))
	}
	if reRule.MatchString(line) {
		return styleMuted.Render(strings.Repeat("─", 36))
	}
	if m := reBullet.FindStringSubmatch(line); m != nil {
		marker, text := "•", m[2]
		if t := reTask.FindStringSubmatch(text); t != nil {
			marker, text = "☐", t[2]
			if t[1] != " " {
				marker = "☑"
			}
		}
		return m[1] + styleMuted.Render(marker+" ") + inline(text, styleText)
	}
	if m := reNumbered.FindStringSubmatch(line); m != nil {
		return m[1] + styleMuted.Render(m[2]+". ") + inline(m[3], styleText)
	}
	if m := reQuote.FindStringSubmatch(line); m != nil {
		return styleMuted.Render("│ ") + inline(m[1], styleMuted)
	}
	return inline(line, styleText)
}

// inline draws `code`, **bold** and [links](url) inside a line; the rest
// gets base.
func inline(text string, base lipgloss.Style) string {
	var b strings.Builder
	last := 0
	for _, m := range reInline.FindAllStringSubmatchIndex(text, -1) {
		b.WriteString(base.Render(text[last:m[0]]))
		part := func(n int) string { return text[m[2*n]:m[2*n+1]] }
		switch {
		case m[2] >= 0:
			b.WriteString(styleAccent.Render(part(1)))
		case m[4] >= 0:
			b.WriteString(base.Bold(true).Render(part(2)))
		case m[6] >= 0:
			b.WriteString(base.Bold(true).Render(part(3)))
		default:
			b.WriteString(styleAccent.Underline(true).Render(part(4)))
		}
		last = m[1]
	}
	b.WriteString(base.Render(text[last:]))
	return b.String()
}

func isTableRow(line string) bool { return strings.HasPrefix(strings.TrimSpace(line), "|") }

// isTableStart says whether lines start a table: a `|` row, then a
// `|---|` row. Prose that starts with `|` ("| grep foo") is not one.
func isTableStart(lines []string) bool {
	return len(lines) > 1 && isTableRow(lines[0]) && reTableSep.MatchString(lines[1])
}

// fenceOpen reads a line that opens a code block: its fence and language.
// A fence closed on the same line ("```go x```") opens nothing.
func fenceOpen(line string) (fence, lang string, ok bool) {
	m := reFence.FindStringSubmatch(line)
	if m == nil {
		return "", "", false
	}
	if t := strings.TrimSpace(line); len(t) > len(m[1]) && strings.HasSuffix(t, m[1]) {
		return "", "", false
	}
	return m[1], m[2], true
}

// tableCells splits `| a | b |` into its trimmed cells, markup removed.
func tableCells(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimSuffix(strings.TrimPrefix(line, "|"), "|")
	cells := strings.Split(line, "|")
	for i, c := range cells {
		cells[i] = ansi.Strip(inline(strings.TrimSpace(c), lipgloss.NewStyle()))
	}
	return cells
}

// renderTable draws a table in columns, the header bold in accent and
// underlined. A table wider than the screen shows each row as
// `header: cell` lines, like Hermes.
func renderTable(lines []string, width int) []string {
	var rows [][]string
	for _, l := range lines {
		if !reTableSep.MatchString(l) {
			rows = append(rows, tableCells(l))
		}
	}
	if len(rows) == 0 {
		return nil
	}
	cols := 0
	for _, r := range rows {
		cols = max(cols, len(r))
	}
	widths := make([]int, cols)
	for _, r := range rows {
		for c, cell := range r {
			widths[c] = max(widths[c], lipgloss.Width(cell))
		}
	}
	total := 2 + 2*(cols-1)
	for _, w := range widths {
		total += w
	}
	cell := func(r []string, c int) string {
		if c < len(r) {
			return r[c]
		}
		return ""
	}
	var out []string
	if total > width {
		if len(rows) == 1 { // a header alone
			return []string{"  " + styleAccent.Bold(true).Render(ansi.Truncate(strings.Join(rows[0], "  "), max(width-2, 1), "…"))}
		}
		for i, r := range rows[1:] {
			if i > 0 {
				out = append(out, styleDim.Render("  "+strings.Repeat("─", max(min(width-3, 40), 1))))
			}
			for c := range cols {
				out = append(out, "  "+styleAccent.Bold(true).Render(cell(rows[0], c)+":")+" "+styleText.Render(cell(r, c)))
			}
		}
		return out
	}
	row := func(r []string, style lipgloss.Style) string {
		var parts []string
		for c := range cols {
			parts = append(parts, style.Render(cell(r, c)+strings.Repeat(" ", widths[c]-lipgloss.Width(cell(r, c)))))
		}
		return "  " + strings.Join(parts, "  ")
	}
	var sep []string
	for _, w := range widths {
		sep = append(sep, strings.Repeat("─", w))
	}
	out = append(out, row(rows[0], styleAccent.Bold(true)), "  "+styleAccent.Render(strings.Join(sep, "  ")))
	for _, r := range rows[1:] {
		out = append(out, row(r, styleText))
	}
	return out
}

// readyBlocks splits finished lines of a streaming reply into what can be
// drawn now and what must wait: an open code block or a table that may
// get more rows is drawn whole once it ends.
func readyBlocks(text string) (ready, held string) {
	lines := strings.SplitAfter(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	hold := -1 // first line that must wait
	fence := ""
	for i, l := range lines {
		open, _, opens := fenceOpen(l)
		switch {
		case fence != "":
			if strings.HasPrefix(strings.TrimSpace(l), fence) {
				fence, hold = "", -1
			}
		case opens:
			fence, hold = open, i
		case isTableRow(l):
			if hold < 0 {
				hold = i
			}
		default:
			hold = -1
		}
	}
	if hold < 0 {
		return text, ""
	}
	return strings.Join(lines[:hold], ""), strings.Join(lines[hold:], "")
}

// Code colors, like Hermes (hermes/ui-tui/src/lib/syntax.ts): strings in
// accent, keywords in the border color, comments muted, the rest plain.
type codeLang struct {
	comment  string
	keywords map[string]bool
}

func words(s string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(s) {
		m[w] = true
	}
	return m
}

var codeLangs = map[string]codeLang{
	"go": {"//", words(`break case chan const continue default defer else fallthrough for func go goto if import
		interface map package range return select struct switch type var nil true false`)},
	"json": {"", words("true false null")},
	"py": {"#", words(`False None True and as assert async await break class continue def del elif else except
		finally for from global if import in is lambda nonlocal not or pass raise return try while with yield`)},
	"rust": {"//", words(`as async await break const continue crate dyn else enum extern false fn for if impl in let
		loop match mod move mut pub ref return self Self static struct super trait true type unsafe use where while yield`)},
	"sh": {"#", words(`if then else elif fi for in do done while until case esac function return break continue
		local export readonly declare typeset`)},
	"sql": {"--", words(`select from where and or not in is null as by group order limit offset insert into values
		update set delete create table drop alter add column primary key foreign references join left right inner outer on`)},
	"ts": {"//", words(`abstract as async await break case catch class const continue debugger default delete do
		else enum export extends false finally for from function get if implements import in instanceof interface is
		let new null of package private protected public readonly return set static super switch this throw true try
		type typeof undefined var void while with yield`)},
	"yaml": {"#", words("true false null yes no on off")},
}

var codeAliases = map[string]string{"bash": "sh", "javascript": "ts", "js": "ts", "jsx": "ts", "python": "py",
	"rs": "rust", "shell": "sh", "tsx": "ts", "typescript": "ts", "yml": "yaml", "zsh": "sh"}

var reCodeToken = regexp.MustCompile("'(?:[^'\\\\]|\\\\.)*'|\"(?:[^\"\\\\]|\\\\.)*\"|`(?:[^`\\\\]|\\\\.)*`|\\b\\d+(?:\\.\\d+)?\\b|[A-Za-z_$][\\w$]*")

// highlight colors one line of code; a language it does not know stays
// plain.
func highlight(line, lang string) string {
	if a, ok := codeAliases[lang]; ok {
		lang = a
	}
	spec, ok := codeLangs[lang]
	if !ok {
		return line
	}
	if spec.comment != "" && strings.HasPrefix(strings.TrimSpace(line), spec.comment) {
		return styleMuted.Render(line)
	}
	var b strings.Builder
	last := 0
	for _, m := range reCodeToken.FindAllStringIndex(line, -1) {
		b.WriteString(line[last:m[0]])
		tok := line[m[0]:m[1]]
		switch c := tok[0]; {
		case c == '"' || c == '\'' || c == '`':
			b.WriteString(styleAccent.Render(tok))
		case c >= '0' && c <= '9':
			b.WriteString(styleText.Render(tok))
		case spec.keywords[tok]:
			b.WriteString(styleBorder.Render(tok))
		default:
			b.WriteString(tok)
		}
		last = m[1]
	}
	return b.String() + line[last:]
}
