package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/viminizer/germes/internal/api"
)

// pickerPage is how many sessions one load of the picker asks for.
const pickerPage = 50

// maxPickerTitle is how many characters of a title a row shows.
const maxPickerTitle = 48

// sessionPicker lists the project's sessions to open one without typing
// its id, like Hermes' session browser (hermes_cli/sessions_cmd_browse.py)
// and `codex resume`: ↑↓ move, Enter opens, typing filters by title, Esc
// clears the filter, then closes. Newest first; pages load as needed.
type sessionPicker struct {
	open     bool
	gen      int           // ignores pages loaded for an earlier opening
	sessions []api.Session // loaded so far
	next     string        // cursor of the next page; "" = all loaded
	loading  bool
	err      string
	filter   string
	selected int // index into shown()
}

// start opens the picker empty; the caller loads the first page.
func (p *sessionPicker) start() {
	*p = sessionPicker{open: true, gen: p.gen + 1, loading: true}
}

// add takes a loaded page.
func (p *sessionPicker) add(page api.Page[api.Session], err error) {
	p.loading = false
	if err != nil {
		p.err = err.Error()
		return
	}
	// Pages are cut by last use: a session used meanwhile moves to the
	// top and can come again on a later page.
	for _, s := range page.Items {
		if !slices.ContainsFunc(p.sessions, func(o api.Session) bool { return o.ID == s.ID }) {
			p.sessions = append(p.sessions, s)
		}
	}
	p.next = page.Next
}

// shown are the loaded sessions whose title (or id) holds the filter.
func (p sessionPicker) shown() []api.Session {
	if p.filter == "" {
		return p.sessions
	}
	var out []api.Session
	for _, s := range p.sessions {
		if strings.Contains(strings.ToLower(s.Title+" "+s.ID), strings.ToLower(p.filter)) {
			out = append(out, s)
		}
	}
	return out
}

func (p *sessionPicker) move(by int) {
	if n := len(p.shown()); n > 0 {
		p.selected = (p.selected + by + n) % n
	}
}

func (p *sessionPicker) setFilter(f string) { p.filter, p.selected = f, 0 }

// current is the selected session.
func (p sessionPicker) current() (api.Session, bool) {
	shown := p.shown()
	if p.selected >= len(shown) {
		return api.Session{}, false
	}
	return shown[p.selected], true
}

// hasPageToLoad reports whether the next page should load: the selection
// is near the end, or a filter must see every session.
func (p sessionPicker) hasPageToLoad() bool {
	return p.next != "" && !p.loading && p.err == "" && (p.filter != "" || p.selected >= len(p.shown())-maxSuggestRows)
}

// view renders the header, the rows around the selection and the footer.
func (p sessionPicker) view(width int, now time.Time) string {
	fit := func(s string) string { return ansi.Truncate(s, max(width, 10), "…") }
	header := styleAccent.Bold(true).Render("Resume a session")
	if p.filter != "" {
		header += styleText.Render(" · filter: " + p.filter + "█")
	} else {
		header += styleDim.Render(" · type to filter")
	}
	lines := []string{fit(header)}
	shown := p.shown()
	switch {
	case p.err != "":
		lines = append(lines, fit(styleError.Render("  "+p.err)))
	case len(shown) == 0 && p.loading:
		lines = append(lines, fit(styleDim.Render("  loading sessions…")))
	case len(shown) == 0 && p.filter != "":
		lines = append(lines, fit(styleDim.Render("  no session matches the filter")))
	case len(shown) == 0:
		lines = append(lines, fit(styleDim.Render("  no sessions in this project yet")))
	default:
		items := make([]suggestion, len(shown))
		for i, s := range shown {
			title := s.Title
			if title == "" {
				title = s.ID
			}
			title = ansi.Truncate(title, maxPickerTitle, "…") // the help column stays in view
			help := api.FormatAge(now.Sub(s.UpdatedAt)) + " · " + string(s.Status)
			if s.Branch != "" {
				help += " · " + s.Branch
			}
			items[i] = suggestion{Label: title, Help: help}
		}
		lines = append(lines, listRows(items, p.selected, width)...)
	}
	count := fmt.Sprintf("%d/%d", min(p.selected+1, len(shown)), len(shown))
	if p.next != "" {
		count += "+" // more pages to load
	}
	lines = append(lines, fit(styleDim.Render("  "+count+" sessions · ↑↓ move · Enter open · Esc close")))
	return strings.Join(lines, "\n")
}
