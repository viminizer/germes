package session

import (
	"context"
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
	m    *Manager
	sess api.Session
	seq  int64
	wake chan struct{} // a message was queued

	mu     sync.Mutex
	queue  []string           // messages waiting for the next turn
	cancel context.CancelFunc // cancels the running turn
}

func (w *worker) run() {
	defer w.m.wg.Done()
	idle := time.NewTimer(idleTime)
	defer idle.Stop()
	for {
		select {
		case <-w.wake:
			// Everything queued while a turn ran becomes one turn.
			for texts := w.take(); len(texts) > 0; texts = w.take() {
				w.runTurn(strings.Join(texts, "\n\n"))
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

// enqueue adds a message for the next turn and wakes the worker.
func (w *worker) enqueue(text string) error {
	w.mu.Lock()
	if len(w.queue) >= maxQueue {
		w.mu.Unlock()
		return api.NewError(api.CodeUnavailable, "session %s has too many queued messages", w.sess.ID)
	}
	w.queue = append(w.queue, text)
	q := w.queuedLocked()
	w.mu.Unlock()
	w.publishQueue(q)
	select {
	case w.wake <- struct{}{}:
	default: // already woken
	}
	return nil
}

// take empties the queue and returns what was in it.
func (w *worker) take() []string {
	w.mu.Lock()
	texts := w.queue
	w.queue = nil
	w.mu.Unlock()
	if len(texts) > 0 {
		w.publishQueue(nil)
	}
	return texts
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
		w.queue = nil
	}
	if w.cancel != nil {
		w.cancel()
	}
	w.mu.Unlock()
	if dropped {
		w.publishQueue(nil)
	}
}

func (w *worker) runTurn(text string) {
	ctx, cancel := context.WithCancel(w.m.ctx)
	w.mu.Lock()
	w.cancel = cancel
	w.mu.Unlock()
	defer func() {
		stopped := ctx.Err() != nil && w.m.ctx.Err() == nil
		cancel()
		w.mu.Lock()
		w.cancel = nil
		w.mu.Unlock()
		if r := recover(); r != nil {
			slog.Error("turn panicked", "session", w.sess.ID, "panic", r, "stack", string(debug.Stack()))
			w.emit(api.Event{Kind: api.KindError, Text: fmt.Sprintf("internal error: %v", r)})
		}
		if stopped {
			w.emit(api.Event{Kind: api.KindError, Text: "stopped"})
		}
		// Status first: a client that sees turn_done must read idle.
		w.setStatus(api.StatusIdle)
		w.emit(api.Event{Kind: api.KindTurnDone, Engine: w.sess.Engine})
	}()

	w.emit(api.Event{Kind: api.KindUserMessage, Text: text})
	w.setStatus(api.StatusWorking)

	for {
		turn := engine.Turn{SessionID: w.sess.ID, Dir: w.sess.Dir, Prompt: text,
			ResumeIDs: w.sess.ResumeIDs, LastEngine: w.sess.Engine}
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
	w.emit(api.Event{Kind: api.KindLimitHit, Text: "every engine is limited; paused until " + until.Format("Jan 2 15:04") + " (Esc Esc to stop waiting)"})
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
		w.emit(api.Event{Kind: api.KindText, Text: ev.Text})
	case engine.KindToolCall:
		w.emit(api.Event{Kind: api.KindToolCall, Tool: toolOf(ev.Tool)})
	case engine.KindToolResult:
		w.emit(api.Event{Kind: api.KindToolResult, Tool: toolOf(ev.Tool)})
	case engine.KindLimitHit:
		text := "usage limit hit"
		if !ev.ResetAt.IsZero() {
			text += ", resets " + ev.ResetAt.Format("Jan 2 15:04")
		}
		w.emit(api.Event{Kind: api.KindLimitHit, Text: text})
	case engine.KindCompacted:
		w.emit(api.Event{Kind: api.KindCompacted, Text: ev.Text, KeepFrom: ev.KeepFrom})
	case engine.KindSwitched:
		w.sess.Engine = ev.Engine
		w.emit(api.Event{Kind: api.KindEngineSwitched, Text: ev.Text, Engine: ev.Engine})
	case engine.KindError:
		w.emit(api.Event{Kind: api.KindError, Text: ev.Err.Error()})
	case engine.KindDone:
		w.sess.Engine = ev.Engine
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
// Save first: a client never sees an event that a crash could lose.
func (w *worker) emit(ev api.Event) {
	ev.SessionID = w.sess.ID
	ev.Time = time.Now()
	if ev.Kind != api.KindTextDelta {
		w.seq++
		ev.Seq = w.seq
		if err := w.m.store.CreateEvent(context.Background(), ev); err != nil {
			slog.Error("save event", "session", w.sess.ID, "err", err)
		}
	}
	w.m.hub.publish(ev)
}

// setStatus saves the status and the fields the worker owns. It reads the
// row first, so a rename done meanwhile is kept.
func (w *worker) setStatus(st api.Status) {
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
