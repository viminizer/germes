package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/viminizer/germes/internal/api"
)

// Like Hermes: state, model and context always; then the title on the
// right, kept whole when it can be; then the extras that fit, in order.
func TestStatusLineKeepsTheTitleBeforeTheExtras(t *testing.T) {
	now := time.Now()
	s := statusBar{
		Engine:     "codex-api",
		Context:    &api.Context{Model: "gpt-5.5", Used: 57_000, Window: 200_000},
		CacheInput: 400, CacheRead: 300,
		Started: now.Add(-2*time.Hour - 5*time.Minute),
		LastEnd: now.Add(-13 * time.Second),
		Title:   "Fix the footer under the input",
	}
	for _, c := range []struct {
		width      int
		has, lacks []string
	}{
		{160, []string{"─ ready │ gpt 5.5 │ 57k/200k │ [███░░░░░░░] 28% │ 2h 5m │ ✓ 13s │ ◎ 75%", " ─  Fix the footer under the input "}, nil},
		{100, []string{"57k/200k │ [███░░░░░░░] 28% │ 2h 5m │ ◎ 75%", " Fix the footer under the input "}, []string{"✓"}},
		{70, []string{"ready │ gpt 5.5 │ 57k tok", "─  Fix the footer"}, []string{"[", "2h 5m"}},
		{20, []string{"ready"}, nil},
	} {
		got := ansi.Strip(s.view(c.width, now))
		if strings.Contains(got, "\n") || lipgloss.Width(got) > c.width {
			t.Errorf("width %d: %d wide or wraps: %q", c.width, lipgloss.Width(got), got)
		}
		for _, want := range c.has {
			if !strings.Contains(got, want) {
				t.Errorf("width %d: %q lacks %q", c.width, got, want)
			}
		}
		for _, no := range c.lacks {
			if strings.Contains(got, no) {
				t.Errorf("width %d: %q still has %q", c.width, got, no)
			}
		}
	}
}

// Before a title, the folder and its branch are on the right, like Hermes:
// under the home folder as ~, cut at the start to 28 characters.
func TestStatusLineShowsTheFolderAndBranch(t *testing.T) {
	t.Setenv("HOME", "/Users/kevin")
	s := statusBar{Engine: "codex-api", Dir: "/Users/kevin/dev/github/germes/germes", Branch: "main"}
	if got := ansi.Strip(s.view(100, time.Now())); !strings.Contains(got, "codex api") || !strings.HasSuffix(got, "─ …github/germes/germes (main)") {
		t.Errorf("got %q", got)
	}
}

func TestStatusLineWritesShortNumbers(t *testing.T) {
	for in, want := range map[int]string{950: "950", 57_257: "57.3k", 999_950: "1M", 1_234_567: "1.2M"} {
		if got := shortCount(in); got != want {
			t.Errorf("shortCount(%d) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[time.Duration]string{45 * time.Second: "45s", 12*time.Minute + 5*time.Second: "12m 5s", 3*time.Hour + 5*time.Minute: "3h 5m", 51 * time.Hour: "2d 3h"} {
		if got := shortDuration(in); got != want {
			t.Errorf("shortDuration(%s) = %q, want %q", in, got, want)
		}
	}
	if got := ansi.Strip(contextBar(api.Context{Used: 300_000, Window: 200_000})); got != "[██████████] 100%" {
		t.Errorf("over the window: %q", got)
	}
	for in, want := range map[string]string{"claude-opus-5-5": "opus 5.5", "gpt-6.1-sol": "gpt 6.1 sol", "openai/gpt-5.5": "gpt 5.5"} {
		if got := modelLabel(in); got != want {
			t.Errorf("modelLabel(%q) = %q, want %q", in, got, want)
		}
	}
	if got := contextUsed(api.Context{Used: 4000}, false); got != "4k tok" {
		t.Errorf("unknown window: %q", got)
	}
}

func TestReplayedTurnKeepsItsEnd(t *testing.T) {
	m := newModel(context.Background(), nil, Options{Engine: "codex-api"})
	start := time.Now().Add(-time.Hour) // an old turn, replayed when the TUI attaches
	m.track(api.Event{Kind: api.KindUserMessage, Time: start})
	m.track(api.Event{Kind: api.KindTurnDone, Time: start.Add(3 * time.Second), Context: &api.Context{Used: 6000, Window: 200_000}})
	if !m.status.LastEnd.Equal(start.Add(3*time.Second)) || m.status.Busy || m.status.Context == nil {
		t.Fatalf("after replay: %+v", m.status)
	}
}

// Every view fits a narrow screen (R43).
func TestViewsFitNarrowScreens(t *testing.T) {
	runs := []toolRun{{Call: &api.ToolCall{Name: "mcp__claude-in-chrome__navigate", Output: "failed badly", IsError: true}, Done: true}}
	todos := parseTodos("- [>] 1. A rather long task that will not fit")
	sug := suggestBox{items: []suggestion{{Label: "/resume", Help: "pick a session of this project from a list"}}}
	for _, width := range []int{10, 20, 30} {
		for name, view := range map[string]string{
			"banner":      banner{Version: "1.0.0", Project: "/a/long/project/path", Engine: "codex-api"}.view(width),
			"tree":        treeView(runs, 1234, "", width, "", time.Time{}, 0),
			"todo":        todoView(todos, width, false, true, 0),
			"suggestions": sug.view(width),
		} {
			for _, line := range strings.Split(view, "\n") {
				if w := lipgloss.Width(line); w > width {
					t.Errorf("%s at %d: a line is %d wide: %q", name, width, w, ansi.Strip(line))
				}
			}
		}
	}
}

// Like Hermes, the status line shows how much of the session's input the
// prompt cache served, once it served any.
func TestStatusLineShowsTheCacheHitRate(t *testing.T) {
	s := statusBar{Engine: "codex-api", Context: &api.Context{Used: 6000, Window: 200_000}}
	if got := ansi.Strip(s.view(120, time.Now())); strings.Contains(got, "◎") {
		t.Fatalf("no cache read yet: %q", got)
	}
	s.CacheInput, s.CacheRead = 400, 300
	if got := ansi.Strip(s.view(120, time.Now())); !strings.HasSuffix(strings.TrimSpace(got), "│ ◎ 75%") {
		t.Fatalf("got %q, want ◎ 75%%", got)
	}
}

// Like Hermes, the line keeps still while a turn runs: the busy state is
// given the room of its widest face, so the faces do not move the rest.
func TestBusyStatusLineKeepsStill(t *testing.T) {
	now := time.Now()
	s := statusBar{Busy: true, Since: now.Add(-3 * time.Second), Engine: "codex-api",
		Started: now.Add(-30 * time.Minute), Title: "Fix the footer under the input"}
	var lines []string
	first := ansi.Strip(s.view(100, now))
	for frame := 0; frame < 25*len(busyFaces); frame += 25 {
		s.Frame = frame
		got := ansi.Strip(s.view(100, now))
		lines = append(lines, got[strings.Index(got, " ─ "):]) // the divider and the title
		if strings.Contains(got, "30m") != strings.Contains(first, "30m") {
			t.Fatalf("frame %d: the session age comes and goes: %q", frame, got)
		}
	}
	for _, l := range lines[1:] {
		if lipgloss.Width(l) != lipgloss.Width(lines[0]) {
			t.Fatalf("the right side moves: %q vs %q", l, lines[0])
		}
	}
}
