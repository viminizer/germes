package tui

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/viminizer/germes/internal/api"
)

const (
	maxSuggestRows = 8
	maxPathEntries = 200
)

// suggestion is one entry of the suggestion list.
type suggestion struct {
	Insert string // replaces the word being typed
	Label  string
	Help   string
}

// suggestBox lists completions for the word being typed: slash commands at
// the start of the input, @ references anywhere (like Hermes).
type suggestBox struct {
	items    []suggestion
	selected int
	closed   string      // input the user closed the box for (Esc); reopens on change
	skills   []api.Skill // installed skills, after the commands; none named like one
}

// update recomputes the list for the composer's text.
func (s *suggestBox) update(input, dir string) {
	if input != s.closed {
		s.closed = ""
	}
	items := suggestionsFor(input, dir, s.skills)
	if s.closed != "" {
		items = nil
	}
	if len(items) != len(s.items) {
		s.selected = 0
	}
	s.items = items
	s.selected = min(s.selected, max(len(items)-1, 0))
}

func (s *suggestBox) move(by int) {
	if n := len(s.items); n > 0 {
		s.selected = (s.selected + by + n) % n
	}
}

// complete returns the input with the selected suggestion in place of the
// word being typed, and whether that changed anything.
func (s suggestBox) complete(input string) (string, bool) {
	if len(s.items) == 0 {
		return input, false
	}
	word := lastWord(input)
	done := input[:len(input)-len(word)] + s.items[s.selected].Insert
	return done, done != input
}

// view renders up to maxSuggestRows rows around the selected one in a
// double-line box, like Hermes.
func (s suggestBox) view(width int) string {
	if len(s.items) == 0 {
		return ""
	}
	inner := max(width-3, 1) // the box's two sides and one spare column
	rows := listRows(s.items, s.selected, inner)
	if len(s.items) > maxSuggestRows {
		rows = append(rows, styleDim.Render(ansi.Truncate(" ↑↓ more · Tab complete · Esc close", inner, "…")))
	}
	box := lipgloss.NewStyle().Border(lipgloss.DoubleBorder()).BorderForeground(colorPrimary).Width(inner + 2)
	return box.Render(strings.Join(rows, "\n"))
}

// listRows renders up to maxSuggestRows items around the selected one,
// help in one column, each row as wide as width and the selected one
// highlighted: the suggestion list and the session picker look the same.
func listRows(items []suggestion, selected, width int) []string {
	start := max(0, min(selected-maxSuggestRows/2, len(items)-maxSuggestRows))
	end := min(len(items), start+maxSuggestRows)
	labelWidth := 0 // help text lines up in one column
	for _, it := range items[start:end] {
		labelWidth = max(labelWidth, len([]rune(it.Label)))
	}
	var rows []string
	for i := start; i < end; i++ {
		it := items[i]
		head := ansi.Truncate(" "+it.Label+strings.Repeat(" ", labelWidth-len([]rune(it.Label))), max(width, 1), "…")
		rest := ""
		if it.Help != "" {
			rest = "  " + it.Help
		}
		room := max(width-lipgloss.Width(head), 0)
		rest = ansi.Truncate(rest, room, "…")
		rest += strings.Repeat(" ", room-lipgloss.Width(rest))
		label, help := styleLabel, styleText
		if i == selected {
			label, help = label.Background(colorSelect).Bold(true), help.Background(colorSelect)
		}
		rows = append(rows, label.Render(head)+help.Render(rest))
	}
	return rows
}

// suggestionsFor returns the completions for the end of input.
func suggestionsFor(input, dir string, skills []api.Skill) []suggestion {
	if strings.HasPrefix(input, "/") && !strings.ContainsAny(input, " \n") {
		var out []suggestion
		for _, c := range commands {
			if strings.HasPrefix("/"+c.Name, input) {
				insert := "/" + c.Name
				if c.Args != "" {
					insert += " "
				}
				out = append(out, suggestion{Insert: insert, Label: strings.TrimSpace("/" + c.Name + " " + c.Args), Help: c.Help})
			}
		}
		for _, s := range skills {
			if strings.HasPrefix("/"+s.Name, input) {
				out = append(out, suggestion{Insert: "/" + s.Name + " ", Label: "/" + s.Name, Help: s.Description})
			}
		}
		return out
	}
	word := lastWord(input)
	if !strings.HasPrefix(word, "@") {
		return nil
	}
	for _, kind := range []string{"@file:", "@folder:"} {
		if path, ok := strings.CutPrefix(word, kind); ok {
			return pathSuggestions(kind, path, dir)
		}
	}
	var out []suggestion
	for _, k := range api.RefKinds {
		if strings.HasPrefix(k.Token, word) && k.Token != word {
			out = append(out, suggestion{Insert: k.Token, Label: k.Token, Help: k.Help})
		}
	}
	return out
}

// pathSuggestions lists entries under dir that start with the typed path.
// Folders end with "/" so Tab goes into them; @folder: lists only folders.
func pathSuggestions(kind, typed, dir string) []suggestion {
	base, prefix := filepath.Split(typed)
	entries, err := os.ReadDir(filepath.Join(dir, base))
	if err != nil {
		return nil
	}
	var out []suggestion
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) || (strings.HasPrefix(name, ".") && !strings.HasPrefix(prefix, ".")) || name == ".git" {
			continue
		}
		if kind == "@folder:" && !e.IsDir() {
			continue
		}
		if e.IsDir() {
			name += "/"
		}
		out = append(out, suggestion{Insert: kind + base + name, Label: base + name})
		if len(out) == maxPathEntries {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

// lastWord is the text after the last space or newline.
func lastWord(s string) string {
	if i := strings.LastIndexAny(s, " \n\t"); i >= 0 {
		return s[i+1:]
	}
	return s
}
