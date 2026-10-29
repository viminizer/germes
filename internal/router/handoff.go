package router

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

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
		fmt.Fprintf(&b, "\n## PROGRESS.md\n\n%s\n", clip(string(progress), 5000))
	}
	if changes := gitChanges(ctx, t.Dir); changes != "" {
		fmt.Fprintf(&b, "\n## Uncommitted changes\n\n%s\n", changes)
	}
	b.WriteString("\nContinue from where it stopped. The user's latest message is your task.")
	return b.String()
}

// recent formats the session's last events, oldest first.
func (r *Router) recent(ctx context.Context, sessionID string) string {
	var events []api.Event
	var after int64
	for {
		page, err := r.store.ListEvents(ctx, api.EventFilter{SessionID: sessionID, AfterSeq: after, Limit: 1000})
		if err != nil || len(page.Items) == 0 {
			break
		}
		events = append(events, page.Items...)
		if page.Next == "" {
			break
		}
		after = page.Items[len(page.Items)-1].Seq
	}
	if len(events) > handoffEvents {
		events = events[len(events)-handoffEvents:]
	}
	var lines []string
	for _, ev := range events {
		switch ev.Kind {
		case api.KindUserMessage:
			lines = append(lines, "User: "+clip(ev.Text, 1500))
		case api.KindText:
			lines = append(lines, "Assistant: "+clip(ev.Text, 1500))
		case api.KindToolCall:
			if ev.Tool != nil {
				lines = append(lines, fmt.Sprintf("Tool call: %s %s", ev.Tool.Name, clip(string(ev.Tool.Args), 300)))
			}
		case api.KindToolResult:
			if ev.Tool != nil {
				lines = append(lines, "Tool result: "+clip(ev.Tool.Output, 400))
			}
		case api.KindError, api.KindLimitHit:
			lines = append(lines, "Note: "+ev.Text)
		}
	}
	text := strings.Join(lines, "\n")
	if r := []rune(text); len(r) > handoffChars {
		text = "[... older events cut ...]\n" + string(r[len(r)-handoffChars:])
	}
	return text
}

// gitChanges is `git status --short` and `git diff --stat`, or "" when the
// folder is not a git repo.
func gitChanges(ctx context.Context, dir string) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	run := func(args ...string) string {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		out, err := cmd.Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	status := run("status", "--short")
	if status == "" {
		return ""
	}
	return clip(status+"\n\n"+run("diff", "--stat"), 3000)
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
