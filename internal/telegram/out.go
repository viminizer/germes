package telegram

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/viminizer/germes/internal/api"
)

// scanDepth is how deep /bind looks for git repos under the code folders.
const scanDepth = 3

// watch follows every session: answers and files from send_file go back to
// the topic that asked, job notices to the project's topic (or General). A
// dropped watch comes back after a short wait.
func (t *Telegram) watch(ctx context.Context) {
	last := map[string]string{}  // session -> its last text this turn
	sending := map[string]bool{} // send_file calls waiting for their result (session + call id)
	for ctx.Err() == nil {
		events, err := t.svc.Watch(ctx, api.WatchFilter{})
		if err != nil {
			slog.Warn("telegram watch", "err", err)
			wait(ctx, retryWait)
			continue
		}
		for ev := range events {
			t.mu.Lock()
			thread, asked := t.state.ReplyTo[ev.SessionID]
			t.mu.Unlock()
			switch {
			case ev.Kind == api.KindNotice:
				t.reply(ctx, t.threadFor(ctx, ev.SessionID), "🔔 "+ev.Text)
			case !asked:
			case ev.Kind == api.KindText:
				last[ev.SessionID] = ev.Text
			case ev.Kind == api.KindToolCall && ev.Tool != nil && ev.Tool.Name == api.ToolSendFile:
				sending[callKey(ev)] = true
			case ev.Kind == api.KindToolResult && ev.Tool != nil && sending[callKey(ev)]:
				delete(sending, callKey(ev))
				if !ev.Tool.IsError {
					t.upload(ctx, thread, ev.Tool.Output)
				}
			case ev.Kind == api.KindError || ev.Kind == api.KindLimitHit:
				t.reply(ctx, thread, "⚠ "+ev.Text)
			case ev.Kind == api.KindTurnDone:
				if text := last[ev.SessionID]; text != "" {
					t.reply(ctx, thread, text)
				}
				delete(last, ev.SessionID)
				// Messages queued during the turn run next: keep listening.
				if s, err := t.svc.GetSession(ctx, ev.SessionID); err != nil || (s.Status == api.StatusIdle && len(s.Queued) == 0) {
					t.mu.Lock()
					delete(t.state.ReplyTo, ev.SessionID)
					t.mu.Unlock()
					t.save()
				}
			}
		}
		wait(ctx, time.Second) // the stream ended: the daemon is going away
	}
}

// callKey names a tool call across sessions: call ids are only unique in one.
func callKey(ev api.Event) string { return ev.SessionID + " " + ev.Tool.ID }

// upload sends a file to a topic, and tells the topic if it fails.
func (t *Telegram) upload(ctx context.Context, thread int64, path string) {
	if err := t.bot.sendFile(ctx, t.opts.ChatID, thread, path); err != nil {
		slog.Warn("telegram send file", "err", err)
		t.reply(ctx, thread, "Could not send the file "+filepath.Base(path)+": "+err.Error())
	}
}

// threadFor finds the topic of a session: the topic it was asked from, the
// topic bound to it or to its project, else General.
func (t *Telegram) threadFor(ctx context.Context, sessionID string) int64 {
	t.mu.Lock()
	if thread, ok := t.state.ReplyTo[sessionID]; ok {
		t.mu.Unlock()
		return thread
	}
	for thread, tp := range t.state.Topics {
		if tp.SessionID == sessionID {
			t.mu.Unlock()
			return thread
		}
	}
	t.mu.Unlock()
	s, err := t.svc.GetSession(ctx, sessionID)
	if err != nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for thread, tp := range t.state.Topics {
		if tp.Project == s.Project {
			return thread
		}
	}
	return 0
}

// showTyping keeps "typing…" on in every topic waiting for an answer.
func (t *Telegram) showTyping(ctx context.Context) {
	tick := time.NewTicker(typingEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		t.mu.Lock()
		threads := map[int64]bool{}
		for _, thread := range t.state.ReplyTo {
			threads[thread] = true
		}
		t.mu.Unlock()
		for thread := range threads {
			params := map[string]any{"chat_id": t.opts.ChatID, "action": "typing"}
			if thread != 0 {
				params["message_thread_id"] = thread
			}
			// Not logged: it repeats every few seconds, and a failure
			// only loses the "typing…" hint.
			t.bot.call(ctx, "sendChatAction", params, nil)
		}
	}
}

// recentProjects lists the projects of recent sessions, newest first.
func (t *Telegram) recentProjects(ctx context.Context) []string {
	page, err := t.svc.ListSessions(ctx, api.SessionFilter{Limit: 200})
	if err != nil {
		return nil
	}
	home, _ := os.UserHomeDir()
	var projects []string
	for _, s := range page.Items {
		if s.Project != home && !slices.Contains(projects, s.Project) {
			projects = append(projects, s.Project)
		}
	}
	return projects
}

// findProjects finds the project folders name means: a path, else known
// projects and git repos under the code folders whose folder name is name
// (or, if none is, contains it).
func (t *Telegram) findProjects(ctx context.Context, name string) []string {
	if strings.HasPrefix(name, "~/") {
		home, _ := os.UserHomeDir()
		name = filepath.Join(home, name[2:])
	}
	if filepath.IsAbs(name) {
		if fi, err := os.Stat(name); err == nil && fi.IsDir() {
			return []string{filepath.Clean(name)}
		}
		return nil
	}
	all := t.recentProjects(ctx)
	for _, dir := range t.opts.CodeDirs {
		for _, repo := range scanRepos(dir) {
			if !slices.Contains(all, repo) {
				all = append(all, repo)
			}
		}
	}
	var exact, partial []string
	want := strings.ToLower(name)
	for _, p := range all {
		base := strings.ToLower(filepath.Base(p))
		switch {
		case base == want:
			exact = append(exact, p)
		case strings.Contains(base, want):
			partial = append(partial, p)
		}
	}
	if len(exact) > 0 {
		return exact
	}
	return partial
}

// scanRepos lists the git repos under dir, a few levels deep, skipping
// hidden folders and dependencies.
func scanRepos(dir string) []string {
	var repos []string
	root := strings.Count(filepath.Clean(dir), string(filepath.Separator))
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		name := d.Name()
		if path != dir && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor") {
			return filepath.SkipDir
		}
		if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
			repos = append(repos, path)
			return filepath.SkipDir
		}
		if strings.Count(path, string(filepath.Separator))-root >= scanDepth {
			return filepath.SkipDir
		}
		return nil
	})
	return repos
}
