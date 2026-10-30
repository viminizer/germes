package telegram

import (
	"context"
	"errors"
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
// dropped watch comes back after a short wait. ready is called after the
// first subscription attempt.
func (t *Telegram) watch(ctx context.Context, ready func()) {
	// The first subscription needs no catch-up: the start settles every
	// waiting session itself (tellLostAnswers), after it.
	for attached := false; ctx.Err() == nil; attached = true {
		events, err := t.svc.Watch(ctx, api.WatchFilter{})
		ready()
		if err != nil {
			slog.Warn("telegram watch", "err", err)
			wait(ctx, retryWait)
			continue
		}
		if attached {
			t.catchUp(ctx)
		}
		for ev := range events {
			t.outputMu.Lock()
			switch {
			// Replay resends saved events only; live-only ones pass through.
			case !t.output.replaying[ev.SessionID] || ev.Kind == api.KindNotice || ev.Kind == api.KindLearned:
				t.sendReplyEvent(ctx, ev)
			case ev.Seq == 0 && (ev.Kind == api.KindError || ev.Kind == api.KindTurnDone):
				// Not saved (issue #25): replay cannot find it, so it
				// waits for the end of the replay.
				t.output.held[ev.SessionID] = append(t.output.held[ev.SessionID], ev)
			}
			t.outputMu.Unlock()
		}
		wait(ctx, time.Second) // the stream ended: the daemon is going away
	}
}

// replyOutput belongs to outputMu, including creation-time stored replay.
type replyOutput struct {
	last      map[string]string
	sending   map[string]bool
	limited   map[string]bool
	seq       map[string]int64
	noticeSeq map[string]int64 // globally routed notices cannot acknowledge unseen replies
	replaying map[string]bool
	held      map[string][]api.Event // unsaved completions that arrived during replay
}

// registerCreated replays only the returned session, while live output is
// excluded. Events published before acceptance returned are already stored;
// queued live copies are skipped by sequence, without buffering strangers.
func (t *Telegram) registerCreated(ctx context.Context, id string, thread int64) {
	t.outputMu.Lock()
	t.output.replaying[id] = true
	defer func() {
		for _, ev := range t.output.held[id] {
			t.sendReplyEvent(ctx, ev)
		}
		delete(t.output.held, id)
		delete(t.output.replaying, id)
		t.mu.Lock()
		_, waiting := t.state.ReplyTo[id]
		t.mu.Unlock()
		if waiting && ctx.Err() == nil {
			// A completion that could not be saved and came before the
			// creation response is gone: settle it from the session itself.
			t.settle(ctx, id, thread, "⚠ This turn ended without a saved answer, so it was lost. Send your message again.")
		}
		t.outputMu.Unlock()
	}()
	t.mu.Lock()
	t.replyGeneration[id]++
	t.state.ReplyTo[id] = thread
	t.mu.Unlock()
	t.save()
	var after int64
	for ctx.Err() == nil {
		page, err := t.svc.ListEvents(ctx, api.EventFilter{SessionID: id, AfterSeq: after, Limit: 1000})
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("telegram: replay created session", "session", id, "err", err)
				// Other sessions must keep receiving replies while history
				// is unavailable. Only this ID waits for its stored replay.
				t.outputMu.Unlock()
				wait(ctx, retryWait)
				t.outputMu.Lock()
			}
			continue
		}
		for _, ev := range page.Items {
			// Notices are already routed globally, even before registration.
			if ev.SessionID == id && ev.Kind != api.KindNotice {
				t.sendReplyEvent(ctx, ev)
			}
			after = ev.Seq
		}
		if page.Next == "" || len(page.Items) == 0 {
			return
		}
	}
}

// sendReplyEvent runs with outputMu held.
func (t *Telegram) sendReplyEvent(ctx context.Context, ev api.Event) {
	t.mu.Lock()
	thread, asked := t.state.ReplyTo[ev.SessionID]
	generation := t.replyGeneration[ev.SessionID]
	handled, known := t.handled[ev.SessionID]
	t.mu.Unlock()
	stored := ev.Seq != 0 && ev.Kind != api.KindNotice
	if asked && stored && ev.Seq <= handled {
		return // already seen: a late copy of an older turn
	}
	if !asked {
		delete(t.output.seq, ev.SessionID)
		delete(t.output.noticeSeq, ev.SessionID)
	}
	if asked && ev.Seq != 0 && ev.Kind != api.KindNotice {
		// A stored event after a gap: send what this bot missed first, or
		// an earlier unanswered turn would be skipped. With no watermark,
		// only a turn_done can tell its turn was missed.
		after, known := t.watermark(ev.SessionID)
		if (known && ev.Seq > after+1 || !known && ev.Kind == api.KindTurnDone) && !t.replayMissed(ctx, ev) {
			return // unknown: the topic keeps waiting, and the next start settles it
		}
	}
	if stored && (asked || known) {
		// Also while nobody waits here (a TUI turn). Only once the event
		// is handled: after a failed gap replay it must stay replayable.
		t.mu.Lock()
		t.handled[ev.SessionID] = max(t.handled[ev.SessionID], ev.Seq)
		t.mu.Unlock()
	}
	seq := t.output.seq
	if ev.Kind == api.KindNotice {
		seq = t.output.noticeSeq
	}
	if asked && ev.Seq != 0 {
		if ev.Seq <= seq[ev.SessionID] {
			return
		}
		seq[ev.SessionID] = ev.Seq
	}
	switch {
	case ev.Kind == api.KindNotice:
		t.reply(ctx, t.threadFor(ctx, ev.SessionID), "🔔 "+ev.Text)
	case ev.Kind == api.KindLearned:
		// Only where this session talks, like Hermes ("💾 Memory
		// updated" in its own chat): never a project topic or General.
		if thread, ok := t.ownThread(ev.SessionID); ok {
			t.reply(ctx, thread, "💾 "+ev.Text)
		}
	case !asked:
	case ev.Kind == api.KindUserMessage && ev.Seq != 0:
		t.markAnswered(ev.SessionID, ev.Seq-1, false)
	case ev.Kind == api.KindStatus:
		t.reply(ctx, thread, "⏳ "+ev.Text)
	case ev.Kind == api.KindText:
		t.output.last[ev.SessionID] = ev.Text
	case ev.Kind == api.KindToolCall && ev.Tool != nil && ev.Tool.Name == api.ToolSendFile:
		t.output.sending[callKey(ev)] = true
	case ev.Kind == api.KindToolResult && ev.Tool != nil && t.output.sending[callKey(ev)]:
		delete(t.output.sending, callKey(ev))
		if !ev.Tool.IsError {
			t.upload(ctx, thread, ev.Tool.Output)
		}
	case ev.Kind == api.KindLimitHit && t.output.limited[ev.SessionID]:
		// Once per turn: a long outage repeats it every cooldown.
	case ev.Kind == api.KindError || ev.Kind == api.KindLimitHit:
		t.output.limited[ev.SessionID] = t.output.limited[ev.SessionID] || ev.Kind == api.KindLimitHit
		t.reply(ctx, thread, "⚠ "+ev.Text)
	case ev.Kind == api.KindTurnDone:
		if text := t.output.last[ev.SessionID]; text != "" {
			t.reply(ctx, thread, text)
		}
		delete(t.output.last, ev.SessionID)
		delete(t.output.limited, ev.SessionID)
		if ev.Seq != 0 {
			t.markAnswered(ev.SessionID, ev.Seq, true)
		}
		t.finishReply(ctx, ev.SessionID, ev.Seq, generation)
	}
}

// markAnswered saves the boundary before a waiting session's first
// unanswered turn: at its first user message seen (only if none is known),
// and after each turn_done handled (later always wins).
func (t *Telegram) markAnswered(id string, seq int64, turnDone bool) {
	t.mu.Lock()
	known, has := t.state.Answered[id]
	_, waiting := t.state.ReplyTo[id]
	changed := waiting && (!has || turnDone && seq > known)
	if changed {
		if t.state.Answered == nil {
			t.state.Answered = map[string]int64{}
		}
		t.state.Answered[id] = seq
	}
	t.mu.Unlock()
	if changed {
		t.save()
	}
}

// finishReply forgets the topic waiting for a session after the turn_done
// at seq, once the session is gone, or idle with nothing queued and nothing
// stored after seq: a later turn may have finished before this one's
// answer went out, and its events are still on their way. A send in flight
// defers the check to finishSent; a send that ended during the check makes
// it check again. Runs with outputMu held.
func (t *Telegram) finishReply(ctx context.Context, id string, seq int64, generation uint64) {
	if seq == 0 {
		// A turn_done that could not be saved goes out live (issue #25):
		// the last stored event this bot saw is the watermark instead.
		seq = max(t.output.seq[id], t.output.noticeSeq[id])
	}
	for {
		s, err := t.svc.GetSession(ctx, id)
		if err == nil && (s.Status != api.StatusIdle || len(s.Queued) > 0) {
			return // messages queued during the turn run next: keep listening
		}
		if err == nil && seq != 0 {
			var later api.Page[api.Event]
			if later, err = t.svc.ListEvents(ctx, api.EventFilter{SessionID: id, AfterSeq: seq, Limit: 1}); err == nil && len(later.Items) > 0 {
				return
			}
		}
		if err != nil && !api.IsNotFound(err) {
			if !errors.Is(err, context.Canceled) {
				slog.Warn("telegram: check a finished turn", "session", id, "err", err)
			}
			return // an unknown state is not proof that queued work finished
		}
		t.mu.Lock()
		switch {
		case t.replySending[id] > 0:
			t.replyDone[id] = seq // the turn may be the one being sent
		case t.replyGeneration[id] == generation:
			delete(t.state.ReplyTo, id)
			delete(t.state.Answered, id)
			delete(t.replyDone, id)
			delete(t.output.seq, id)
			delete(t.output.noticeSeq, id)
		default:
			// A send ended during the check, too early for finishSent to
			// see this completion: check again with its outcome.
			generation = t.replyGeneration[id]
			t.mu.Unlock()
			continue
		}
		t.mu.Unlock()
		t.save()
		return
	}
}

// finishSent checks a completion that arrived while a send to the session
// was in flight, now that the send is over. Only a deferred completion
// waits for output: the poller must not stall behind slow replies.
func (t *Telegram) finishSent(ctx context.Context, id string) {
	t.mu.Lock()
	_, deferred := t.replyDone[id]
	t.mu.Unlock()
	if !deferred {
		return
	}
	t.outputMu.Lock()
	defer t.outputMu.Unlock()
	t.mu.Lock()
	seq, done := t.replyDone[id]
	done = done && t.replySending[id] == 0
	if done {
		delete(t.replyDone, id)
	}
	generation := t.replyGeneration[id]
	t.mu.Unlock()
	if done {
		t.finishReply(ctx, id, seq, generation)
	}
}

// replayMissed sends the stored events of a waiting session that this bot
// did not see live before ev: the bot attached during a hand-over, or the
// hub dropped its stream. It starts after the watermark, else (ev is a
// turn_done) at the turn's user message. Runs with outputMu held.
func (t *Telegram) replayMissed(ctx context.Context, ev api.Event) bool {
	after, known := t.watermark(ev.SessionID)
	if !known {
		after = -1
	}
	missed, err := t.storedEvents(ctx, ev.SessionID, after, ev.Seq)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			slog.Warn("telegram: read missed events", "session", ev.SessionID, "err", err)
		}
		return false
	}
	for _, ev := range missed {
		t.sendReplyEvent(ctx, ev)
	}
	return true
}

// watermark is the last stored event of a waiting session this bot has
// handled: the last one seen, else the saved answered boundary. Runs with
// outputMu held.
func (t *Telegram) watermark(id string) (int64, bool) {
	if seq := t.output.seq[id]; seq != 0 {
		return seq, true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	answered, known := t.state.Answered[id]
	return answered, known
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
	if thread, ok := t.ownThread(sessionID); ok {
		return thread
	}
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

// ownThread is the topic of the session itself: the one waiting for its
// answer, the one bound to it, or General for the overseer.
func (t *Telegram) ownThread(sessionID string) (int64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if thread, ok := t.state.ReplyTo[sessionID]; ok {
		return thread, true
	}
	if sessionID == api.OverseerID {
		return 0, true
	}
	for thread, tp := range t.state.Topics {
		if tp.SessionID == sessionID {
			return thread, true
		}
	}
	return 0, false
}
