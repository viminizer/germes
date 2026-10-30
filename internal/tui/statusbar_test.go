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

func TestStatusLineFitsAndDropsTheLeastImportantFirst(t *testing.T) {
	s := statusBar{
		Engine:   "codex-api",
		Context:  &api.Context{Model: "gpt-5.5", Used: 57_000, Window: 200_000},
		Started:  time.Now().Add(-2*time.Hour - 5*time.Minute),
		LastTurn: 13 * time.Second,
		Branch:   "germes/abc123",
		Title:    "Fix the footer under the input",
		Project:  "/p/germes",
	}
	for _, c := range []struct {
		width      int
		has, lacks []string
	}{
		{160, []string{"ready", "gpt 5.5", "57k/200k", "28%", "2h 5m", "✓ 13s", "Fix the footer under the input (germes/abc123)"}, nil},
		{90, []string{"ready", "gpt 5.5", "57k/200k", "2h 5m", "─ Fix the footer under …"}, []string{"germes/abc123"}},
		{50, []string{"ready", "gpt 5.5", "57k/200k"}, []string{"2h 5m", "germes/abc123"}},
		{20, []string{"ready"}, nil},
	} {
		got := ansi.Strip(s.view(c.width))
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
	// Before a turn ends there is no context: the engine stands in for the
	// model; before a title the project is on the right.
	if got := ansi.Strip(statusBar{Engine: "codex-api", Project: "/p/germes"}.view(80)); !strings.Contains(got, "codex api") || !strings.HasSuffix(got, "─ /p/germes") {
		t.Errorf("no context, no title: %q", got)
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
	if got := ansi.Strip(contextUse(api.Context{Used: 300_000, Window: 200_000})); !strings.Contains(got, "[██████████] 100%") {
		t.Errorf("over the window: %q", got)
	}
	for in, want := range map[string]string{"claude-opus-5-5": "opus 5.5", "gpt-6.1-sol": "gpt 6.1 sol", "openai/gpt-5.5": "gpt 5.5"} {
		if got := modelLabel(in); got != want {
			t.Errorf("modelLabel(%q) = %q, want %q", in, got, want)
		}
	}
	if got := ansi.Strip(contextUse(api.Context{Used: 4000})); got != "4k" {
		t.Errorf("unknown window: %q", got)
	}
}

func TestReplayedTurnKeepsItsLength(t *testing.T) {
	m := newModel(context.Background(), nil, Options{Engine: "codex-api"})
	start := time.Now().Add(-time.Hour) // an old turn, replayed when the TUI attaches
	m.track(api.Event{Kind: api.KindUserMessage, Time: start})
	m.track(api.Event{Kind: api.KindTurnDone, Time: start.Add(3 * time.Second), Context: &api.Context{Used: 6000, Window: 200_000}})
	if m.status.LastTurn != 3*time.Second || m.status.Busy || m.status.Context == nil {
		t.Fatalf("after replay: %+v", m.status)
	}
}
