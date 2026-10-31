package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/viminizer/germes/internal/api"
	"github.com/viminizer/germes/internal/engine"
)

// maxQueue is how many messages may wait while a turn runs.
const maxQueue = 100

// worker runs one session's turns, one at a time. Only the worker goroutine
// touches sess and seq.
type worker struct {
	m       *Manager
	sess    api.Session
	seq     int64
	reply   string        // the turn's last text (a plan turn saves it)
	used    *api.Context  // how full the model's context was at the turn's end; nil = unknown
	namings int           // turns that asked a model for the title (this daemon only)
	wake    chan struct{} // a message was queued

	mu      sync.Mutex
	queue   []string           // messages waiting for the next turn
	plan    bool               // a queued message asks for a plan turn
	skills  []string           // skills queued messages load first (/<skill> in the TUI)
	cancel  context.CancelFunc // cancels the running turn
	running bool               // a turn has claimed its messages
	lost    bool               // a save failed in this turn: it stopped, and the user was told
	status  api.Status         // live status; empty until this worker owns a turn
}

func (w *worker) run() {
	defer w.m.wg.Done()
	idle := time.NewTimer(idleTime)
	defer idle.Stop()
	for {
		select {
		case <-w.wake:
			if !w.waitElsewhere() {
				return
			}
			// Everything queued while a turn ran becomes one turn.
			for texts, plan, skills := w.take(); len(texts) > 0; texts, plan, skills = w.take() {
				w.runTurn(strings.Join(texts, "\n\n"), plan, skills)
			}
			idle.Reset(idleTime)
		case <-idle.C:
			if w.exit() {
				return
			}
			idle.Reset(idleTime)
		case <-w.m.ctx.Done():
			return
		}
	}
}

// elsewhereCheck is how often a worker asks whether another process still
// runs its session.
const elsewhereCheck = 300 * time.Millisecond

// waitElsewhere waits while another process (an old daemon after an
// update) runs this session; the queue keeps filling meanwhile. Then it
// reloads the session and its last seq, which that process changed. It
// reports false when the manager stops.
func (w *worker) waitElsewhere() bool {
	el := w.m.opts.Elsewhere
	if el == nil {
		return true
	}
	for el.IsBusy(w.m.ctx, w.sess.ID) {
		select {
		case <-time.After(elsewhereCheck):
		case <-w.m.ctx.Done():
			return false
		}
	}
	s, err := w.m.store.GetSession(w.m.ctx, w.sess.ID)
	if err != nil {
		slog.Error("reload session", "session", w.sess.ID, "err", err)
		return true
	}
	seq, err := w.m.store.GetLastSeq(w.m.ctx, w.sess.ID)
	if err != nil {
		slog.Error("reload session", "session", w.sess.ID, "err", err)
		return true
	}
	w.sess, w.seq = s, seq
	return true
}

// snapshotSession overlays the fields owned in memory together, rather than
// pairing a possibly older stored status with an already consumed queue.
func (w *worker) snapshotSession(s api.Session) api.Session {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status != "" {
		s.Status = w.status
		s.Queued = nil // local ownership supersedes the previous owner's snapshot
	}
	s.Queued = append(s.Queued, w.queuedLocked()...)
	return s
}

// isBusy reports whether turns run or messages wait.
func (w *worker) isBusy() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.running || len(w.queue) > 0
}

// enqueue adds a message for the next turn and wakes the worker.
func (w *worker) enqueue(text string, plan bool, skill string) error {
	w.mu.Lock()
	if len(w.queue) >= maxQueue {
		w.mu.Unlock()
		return api.NewError(api.CodeUnavailable, "session %s has too many queued messages", w.sess.ID)
	}
	w.queue = append(w.queue, text)
	w.plan = w.plan || plan
	if skill != "" {
		w.skills = append(w.skills, skill)
	}
	q := w.queuedLocked()
	w.mu.Unlock()
	w.publishQueue(q)
	select {
	case w.wake <- struct{}{}:
	default: // already woken
	}
	return nil
}

// take empties the queue and returns what was in it, whether one of the
// messages asked for a plan, and the skills they load.
func (w *worker) take() ([]string, bool, []string) {
	w.mu.Lock()
	texts, plan, skills := w.queue, w.plan, w.skills
	w.queue, w.plan, w.skills = nil, false, nil
	if len(texts) > 0 {
		w.running, w.status = true, api.StatusWorking
	}
	w.mu.Unlock()
	if len(texts) > 0 {
		w.publishQueue(nil)
	}
	return texts, plan, skills
}

// queued returns a copy of the waiting messages.
func (w *worker) queued() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.queuedLocked()
}

func (w *worker) queuedLocked() []string { return append([]string(nil), w.queue...) }

// publishQueue tells watchers what is waiting. Live only: the queue lives in
// memory, and GetSession reports it to clients that attach later.
func (w *worker) publishQueue(q []string) {
	w.m.hub.publish(api.Event{SessionID: w.sess.ID, Time: time.Now(), Kind: api.KindQueue, Queue: q})
}

// exit removes the worker from the manager unless a message is waiting.
func (w *worker) exit() bool {
	w.m.mu.Lock()
	defer w.m.mu.Unlock()
	if len(w.queued()) > 0 {
		return false
	}
	delete(w.m.workers, w.sess.ID)
	return true
}

// stopTurn stops the running turn. With dropQueue the queued messages are
// dropped; without it they run next, right away.
func (w *worker) stopTurn(dropQueue bool) {
	w.mu.Lock()
	dropped := dropQueue && len(w.queue) > 0
	if dropQueue {
		w.queue, w.skills = nil, nil
	}
	if w.cancel != nil {
		w.cancel()
	}
	w.mu.Unlock()
	if dropped {
		w.publishQueue(nil)
	}
}

func (w *worker) runTurn(text string, plan bool, skills []string) {
	ctx, cancel := context.WithCancel(w.m.ctx)
	w.mu.Lock()
	w.cancel, w.lost = cancel, false
	w.mu.Unlock()
	defer func() {
		cut := ctx.Err() != nil && w.m.ctx.Err() == nil // before cancel, which always sets ctx.Err
		cancel()
		w.mu.Lock()
		// A failed save stopped it: the user was told already (lose).
		stopped := cut && !w.lost
		w.cancel = nil
		w.mu.Unlock()
		if r := recover(); r != nil {
			slog.Error("turn panicked", "session", w.sess.ID, "panic", r, "stack", string(debug.Stack()))
			w.emit(api.Event{Kind: api.KindError, Text: fmt.Sprintf("internal error: %v", r)})
		}
		if stopped {
			w.emit(api.Event{Kind: api.KindError, Text: "stopped"})
		} else if plan && w.reply != "" {
			w.savePlan(text, w.reply)
		}
		// Finish before turn_done; a queued successor may already be working
		// when a client reads the session in response to that event.
		w.setStatus(api.StatusIdle)
		w.emit(api.Event{Kind: api.KindTurnDone, Engine: w.sess.Engine, Context: w.used})
		// ListBusy is the cross-daemon ownership signal, not the live status.
		w.mu.Lock()
		w.running = false
		w.mu.Unlock()
	}()

	w.emit(api.Event{Kind: api.KindUserMessage, Text: text})
	w.reply, w.used = "", nil
	w.setStatus(api.StatusWorking)
	w.refresh()
	w.nameSession(text)
	w.snapshot(ctx, text)
	prompt := text
	if w.m.opts.Refs != nil {
		// Saved apart from the message, so the TUI shows what was typed and
		// the history still gives the model the attached text.
		if att := w.m.opts.Refs.Expand(ctx, text, w.sess.ID, w.sess.Dir); att != "" {
			w.emit(api.Event{Kind: api.KindAttachment, Text: att})
			prompt += "\n\n" + att
		}
		for _, name := range skills {
			prompt += "\n\n" + w.loadSkill(ctx, name)
		}
	}

	for {
		turn := engine.Turn{SessionID: w.sess.ID, Dir: w.sess.Dir, Project: w.sess.Project, Branch: w.sess.Branch,
			Prompt: prompt, ResumeIDs: w.sess.ResumeIDs, LastEngine: w.sess.Engine, Overseer: w.sess.ID == api.OverseerID, Plan: plan,
			Saves: true}
		events, err := w.m.runner.Run(ctx, turn)
		var unavail *engine.UnavailableError
		if errors.As(err, &unavail) {
			if !w.pause(ctx, unavail.ResetAt) {
				return
			}
			continue
		}
		if err != nil {
			w.emit(api.Event{Kind: api.KindError, Text: err.Error()})
			return
		}
		var retryAt time.Time // every engine got limited during the turn
		for ev := range events {
			if ev.Kind == engine.KindError && errors.As(ev.Err, &unavail) {
				retryAt = unavail.ResetAt
				continue
			}
			w.handle(ev)
		}
		if retryAt.IsZero() || ctx.Err() != nil || !w.pause(ctx, retryAt) {
			return
		}
	}
}

// pause waits until every engine may be free again. It reports false when
// the turn was stopped (Esc Esc) while waiting.
func (w *worker) pause(ctx context.Context, until time.Time) bool {
	w.emit(api.Event{Kind: api.KindLimitHit, Text: "every engine is limited or unavailable; paused until " + until.Format("Jan 2 15:04") + " (Esc Esc to stop waiting)"})
	w.setStatus(api.StatusPaused)
	defer w.setStatus(api.StatusWorking)
	select {
	case <-time.After(time.Until(until)):
		return true
	case <-ctx.Done():
		return false
	}
}

// handle turns one engine event into session events.
func (w *worker) handle(ev engine.Event) {
	switch ev.Kind {
	case engine.KindTextDelta:
		w.emit(api.Event{Kind: api.KindTextDelta, Text: ev.Text})
	case engine.KindText:
		w.reply = ev.Text
		w.emit(api.Event{Kind: api.KindText, Text: ev.Text})
	case engine.KindToolCall, engine.KindToolResult:
		kind := api.KindToolCall
		if ev.Kind == engine.KindToolResult {
			kind = api.KindToolResult
		}
		err := w.emit(api.Event{Kind: kind, Tool: toolOf(ev.Tool)})
		if ev.Saved != nil {
			ev.Saved <- err // the engine runs the tool (or goes on) only once it is saved
		}
	case engine.KindLimitHit:
		text := "usage limit hit"
		if ev.Outage {
			text = "provider unavailable"
			if ev.Err != nil {
				text = strings.TrimRight(ev.Err.Error(), ". ")
			}
			text += "; tried again"
			if !ev.ResetAt.IsZero() {
				text += " after " + ev.ResetAt.Format("15:04")
			}
			w.emit(api.Event{Kind: api.KindLimitHit, Text: text})
			return
		}
		if !ev.ResetAt.IsZero() {
			text += ", resets " + ev.ResetAt.Format("Jan 2 15:04")
		}
		w.emit(api.Event{Kind: api.KindLimitHit, Text: text})
	case engine.KindCompacted:
		w.emit(api.Event{Kind: api.KindCompacted, Text: ev.Text, KeepFrom: ev.KeepFrom})
	case engine.KindReasoning:
		w.emit(api.Event{Kind: api.KindReasoning, Text: ev.Text})
	case engine.KindSwitched:
		w.sess.Engine = ev.Engine
		w.emit(api.Event{Kind: api.KindEngineSwitched, Text: ev.Text, Engine: ev.Engine})
	case engine.KindError:
		w.emit(api.Event{Kind: api.KindError, Text: ev.Err.Error()})
	case engine.KindStatus:
		// Live only, like a text delta: a retry status is no use in the history.
		w.m.SendEvent(api.Event{SessionID: w.sess.ID, Time: time.Now(), Kind: api.KindStatus, Text: ev.Text})
	case engine.KindDone:
		w.sess.Engine = ev.Engine
		if c := ev.Context; c != nil {
			w.used = &api.Context{Model: c.Model, Used: c.Used, Window: c.Window}
		}
		if ev.ResumeID != "" {
			if w.sess.ResumeIDs == nil {
				w.sess.ResumeIDs = map[string]string{}
			}
			w.sess.ResumeIDs[ev.Engine] = ev.ResumeID
		}
	}
}

func toolOf(t *engine.ToolCall) *api.ToolCall {
	if t == nil {
		return nil
	}
	return &api.ToolCall{ID: t.ID, Name: t.Name, Args: t.Args, Output: t.Output, IsError: t.IsError}
}

// emit saves an event (except live text deltas), then publishes it.
// Save first: a client never sees an event that a crash could lose. A failed
// save is handled here (lost); the error is returned for the engine waiting
// on it.
// loadSkill loads a skill the user called by name, saved as a skill_view
// call: the Germes loop sees it in the history, and insights and skill hooks
// count it. It returns the output for an engine that takes the prompt.
func (w *worker) loadSkill(ctx context.Context, name string) string {
	args, _ := json.Marshal(map[string]string{"name": name})
	call := &api.ToolCall{ID: fmt.Sprintf("skill-%d", w.seq+1), Name: "skill_view", Args: args}
	w.emit(api.Event{Kind: api.KindToolCall, Tool: call})
	out, isErr := w.m.opts.Refs.LoadSkill(ctx, name, w.sess.ID, w.sess.Dir)
	w.emit(api.Event{Kind: api.KindToolResult, Tool: &api.ToolCall{ID: call.ID, Name: call.Name, Output: out, IsError: isErr}})
	return out
}

func (w *worker) emit(ev api.Event) error {
	ev.SessionID = w.sess.ID
	ev.Time = time.Now()
	if ev.Kind != api.KindTextDelta {
		w.seq++
		ev.Seq = w.seq
		if err := w.m.store.CreateEvent(context.Background(), ev); err != nil {
			w.seq-- // not saved: the next event takes its number, no gap
			w.lose(ev, err)
			return err
		}
	}
	w.m.hub.publish(ev)
	return nil
}

// lose handles an event that could not be saved (a full disk, a broken
// database; issue #25). It is not shown as saved: only the end of the turn
// and errors still go out, live (Seq 0), so watchers know the turn ended.
// The turn stops, and the user is told once.
func (w *worker) lose(ev api.Event, err error) {
	slog.Error("save event", "session", w.sess.ID, "kind", ev.Kind, "err", err)
	live := func(ev api.Event) {
		ev.Seq = 0
		w.m.hub.publish(ev)
	}
	w.mu.Lock()
	first, cancel := !w.lost, w.cancel
	w.lost = true
	w.mu.Unlock()
	if first {
		live(api.Event{SessionID: w.sess.ID, Time: time.Now(), Kind: api.KindError,
			Text: fmt.Sprintf("could not save the conversation (%v), so the turn stopped; what follows is not kept. Free disk space or run `germes doctor`.", err)})
	}
	if ev.Kind == api.KindTurnDone || ev.Kind == api.KindError {
		live(ev)
	}
	if cancel != nil {
		cancel()
	}
}

// setStatus saves the status and the fields the worker owns. It reads the
// row first, so a rename done meanwhile is kept.
func (w *worker) setStatus(st api.Status) {
	w.mu.Lock()
	w.status = st
	w.mu.Unlock()
	w.sess.Status = st
	w.m.saveMu.Lock()
	defer w.m.saveMu.Unlock()
	ctx := context.Background()
	s, err := w.m.store.GetSession(ctx, w.sess.ID)
	if err != nil {
		slog.Error("load session", "session", w.sess.ID, "err", err)
		return
	}
	s.Status, s.Engine, s.ResumeIDs, s.UpdatedAt = st, w.sess.Engine, w.sess.ResumeIDs, time.Now()
	if err := w.m.store.UpdateSession(ctx, s); err != nil {
		slog.Error("save session", "session", w.sess.ID, "err", err)
	}
}

// refresh reloads what others may change between turns: the folder after
// leaving a worktree, and the title.
func (w *worker) refresh() {
	s, err := w.m.store.GetSession(context.Background(), w.sess.ID)
	if err != nil {
		slog.Error("reload session", "session", w.sess.ID, "err", err)
		return
	}
	w.sess.Dir, w.sess.Branch, w.sess.Project, w.sess.Title = s.Dir, s.Branch, s.Project, s.Title
}
