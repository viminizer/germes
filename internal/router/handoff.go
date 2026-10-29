package router

import (
	"context"
	"fmt"
	"github.com/viminizer/germes/internal/text"
	"github.com/viminizer/germes/internal/worktree"
	"os"
	"path/filepath"
	"strings"

	"github.com/viminizer/germes/internal/api"
	"github.com/viminizer/germes/internal/engine"
)

const (
	handoffEvents = 40     // recent events shown to the next engine
	handoffChars  = 12_000 // size limit of the recent-events part
)

// handoff tells the next engine what happened before it took over: recent
// conversation, PROGRESS.md and uncommitted changes. Germes owns the
// context, not the provider.
func (r *Router) handoff(ctx context.Context, t engine.Turn, from string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Handoff from %s\n\nYou are taking over this session from another agent (%s). Germes keeps the shared history; here is what you need.\n", from, from)
	if recent := r.recent(ctx, t.SessionID); recent != "" {
		fmt.Fprintf(&b, "\n## Recent conversation\n\n%s\n", recent)
	}
	if progress, err := os.ReadFile(filepath.Join(t.Dir, "PROGRESS.md")); err == nil && len(progress) > 0 {
		fmt.Fprintf(&b, "\n## PROGRESS.md\n\n%s\n", text.Shorten(string(progress), 5000))
	}
	if changes := gitChanges(ctx, t.Dir); changes != "" {
		fmt.Fprintf(&b, "\n## Uncommitted changes\n\n%s\n", changes)
	}
	b.WriteString("\nContinue from where it stopped. The user's latest message is your task.")
	return b.String()
}

// recent formats the session's last events, oldest first.
func (r *Router) recent(ctx context.Context, sessionID string) string {
	events, err := api.ListAllEvents(ctx, r.store, api.EventFilter{SessionID: sessionID})
	if err != nil {
		return ""
	}
	if len(events) > handoffEvents {
		events = events[len(events)-handoffEvents:]
	}
	out := text.Transcript(events).String()
	if runes := []rune(out); len(runes) > handoffChars {
		out = "[... older events cut ...]\n" + string(runes[len(runes)-handoffChars:])
	}
	return out
}

// gitChanges is `git status --short` and `git diff --stat`, or "" when the
// folder is not a git repo.
func gitChanges(ctx context.Context, dir string) string {
	run := func(args ...string) string {
		out, err := worktree.RunGit(ctx, dir, args...)
		if err != nil {
			return ""
		}
		return out
	}
	status := run("status", "--short")
	if status == "" {
		return ""
	}
	return text.Shorten(status+"\n\n"+run("diff", "--stat"), 3000)
}
