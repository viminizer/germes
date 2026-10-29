package session

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/viminizer/germes/internal/api"
	"github.com/viminizer/germes/internal/engine"
)

// worker runs one session's turns, one at a time. Only the worker goroutine
// touches sess and seq.
type worker struct {
	m     *Manager
	sess  api.Session
	seq   int64
	inbox chan string

	mu     sync.Mutex
	cancel context.CancelFunc // cancels the running turn
}

func (w *worker) run() {
	defer w.m.wg.Done()
	idle := time.NewTimer(idleTime)
	defer idle.Stop()
	for {
		select {
		case text := <-w.inbox:
			w.runTurn(text)
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

// exit removes the worker from the manager unless a message just arrived.
func (w *worker) exit() bool {
	w.m.mu.Lock()
	defer w.m.mu.Unlock()
	if len(w.inbox) > 0 {
		return false
	}
	delete(w.m.workers, w.sess.ID)
	return true
}

func (w *worker) stopTurn() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cancel != nil {
		w.cancel()
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
		w.emit(api.Event{Kind: api.KindTurnDone})
	}()

	w.emit(api.Event{Kind: api.KindUserMessage, Text: text})
	w.setStatus(api.StatusWorking)

	turn := engine.Turn{SessionID: w.sess.ID, Dir: w.sess.Dir, Prompt: text,
		ResumeIDs: w.sess.ResumeIDs, LastEngine: w.sess.Engine}
	events, err := w.m.runner.Run(ctx, turn)
	if err != nil {
		w.emit(api.Event{Kind: api.KindError, Text: err.Error()})
		return
	}
	for ev := range events {
		w.handle(ev)
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
		w.emit(api.Event{Kind: api.KindLimitHit, Text: "usage limit hit, resets " + ev.ResetAt.Format("Jan 2 15:04")})
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
