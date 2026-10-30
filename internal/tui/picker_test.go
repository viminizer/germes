package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/viminizer/germes/internal/api"
)

// pagedSessions has n sessions in one project, newest first, in pages.
type pagedSessions struct {
	api.Service // calls not used here panic
	n           int
	filters     *[]api.SessionFilter
}

func (s pagedSessions) ListSessions(_ context.Context, f api.SessionFilter) (api.Page[api.Session], error) {
	*s.filters = append(*s.filters, f)
	from, _ := strconv.Atoi(f.Cursor)
	var page api.Page[api.Session]
	for i := from; i < min(from+f.Limit, s.n); i++ {
		page.Items = append(page.Items, api.Session{ID: fmt.Sprintf("s%d", i), Title: fmt.Sprintf("task %d", i), Status: api.StatusIdle})
	}
	if from+f.Limit < s.n {
		page.Next = strconv.Itoa(from + f.Limit)
	}
	return page, nil
}

// run applies a command's messages to the model until none is left.
func run(m *model, cmd tea.Cmd) tea.Msg {
	var last tea.Msg
	for cmd != nil {
		last = cmd()
		if last == nil {
			return nil
		}
		if _, ok := last.(switchMsg); ok {
			return last
		}
		_, cmd = m.Update(last)
	}
	return last
}

func press(m *model, key string) tea.Msg {
	k := tea.KeyPressMsg{Text: key}
	switch key {
	case "down":
		k = tea.KeyPressMsg{Code: tea.KeyDown}
	case "esc":
		k = tea.KeyPressMsg{Code: tea.KeyEscape}
	case "enter":
		k = tea.KeyPressMsg{Code: tea.KeyEnter}
	default:
		k.Code = []rune(key)[0]
	}
	_, cmd := m.Update(k)
	return run(m, cmd)
}

func TestPickerOpensTheChosenSessionOfTheProject(t *testing.T) {
	filters := &[]api.SessionFilter{}
	m := newModel(context.Background(), pagedSessions{n: pickerPage + 10, filters: filters}, Options{Project: "/p", Pick: true})
	run(m, m.openPicker())
	if len(m.picker.sessions) != pickerPage || (*filters)[0].Project != "/p" {
		t.Fatalf("first load: %d sessions, filter %+v", len(m.picker.sessions), (*filters)[0])
	}
	// Near the end of the list, the next page loads.
	for range pickerPage - maxSuggestRows {
		press(m, "down")
	}
	if len(m.picker.sessions) != pickerPage+10 {
		t.Fatalf("next page not loaded: %d sessions", len(m.picker.sessions))
	}
	// Typing filters by title; Esc clears the filter before it closes.
	for _, r := range "task 57" {
		press(m, string(r))
	}
	if s, _ := m.picker.current(); s.ID != "s57" || len(m.picker.shown()) != 1 {
		t.Fatalf("filtered to %v", m.picker.shown())
	}
	press(m, "esc")
	if !m.picker.open || m.picker.filter != "" {
		t.Fatal("first Esc did not only clear the filter")
	}
	press(m, "down")
	if msg, ok := press(m, "enter").(switchMsg); !ok || msg.id != "s1" || m.picker.open {
		t.Fatalf("Enter gave %v, picker open %v", msg, m.picker.open)
	}
}

func TestPickerSaysWhenTheProjectHasNoSessions(t *testing.T) {
	m := newModel(context.Background(), pagedSessions{filters: &[]api.SessionFilter{}}, Options{})
	run(m, m.openPicker())
	if v := m.picker.view(80, time.Now()); !strings.Contains(v, "no sessions in this project yet") {
		t.Fatalf("view: %q", v)
	}
	for _, line := range strings.Split(m.picker.view(20, time.Now()), "\n") {
		if w := ansi.StringWidth(line); w > 20 {
			t.Fatalf("line %q is %d wide", line, w)
		}
	}
}
