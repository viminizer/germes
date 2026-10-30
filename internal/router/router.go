// Package router runs each turn on the best available engine. When an engine
// hits a usage limit, it marks it and moves the turn to the next engine with
// a handoff, so the context is kept. The session does not know engines exist.
package router

import (
	"context"
	"errors"
	"fmt"
	"github.com/viminizer/germes/internal/stream"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/viminizer/germes/internal/api"
	"github.com/viminizer/germes/internal/config"
	"github.com/viminizer/germes/internal/engine"
	"github.com/viminizer/germes/internal/text"
)

// unknownReset is how long an engine stays blocked when its limit message
// has no reset time. A failed try costs little, so then it is tried again.
const unknownReset = 30 * time.Minute

// maxOutageWait caps the cooldown of an engine whose provider is down: 1,
// 2, 4 ... minutes while it stays down (issue #40).
// ponytail: no jitter; add it if many Germes share one provider.
const maxOutageWait = 30 * time.Minute

// Store keeps limits and usage across restarts, and gives the history the
// handoff needs.
type Store interface {
	ListLimits(ctx context.Context) (map[string]time.Time, error)
	UpdateLimit(ctx context.Context, name string, until time.Time) error
	ListUsage(ctx context.Context) ([]engine.Usage, error)
	UpdateUsage(ctx context.Context, u engine.Usage) error
	ListEvents(ctx context.Context, f api.EventFilter) (api.Page[api.Event], error)
}

// Router picks engines. It satisfies session.Runner.
type Router struct {
	engines []engine.Engine // config order = preference
	cfg     map[string]config.Engine
	store   Store
	now     func() time.Time

	mu      sync.Mutex
	limits  map[string]time.Time               // engine -> limited until
	usage   map[string]map[string]engine.Usage // engine -> window -> usage
	outages map[string]int                     // engine -> outages in a row (in memory: a restart starts over)
}

// New creates the router and loads the saved limits and usage.
func New(engines []engine.Engine, cfg []config.Engine, store Store, now func() time.Time) (*Router, error) {
	if len(engines) == 0 {
		return nil, errors.New("router: no engines")
	}
	r := &Router{engines: engines, cfg: map[string]config.Engine{}, store: store, now: now,
		usage: map[string]map[string]engine.Usage{}, outages: map[string]int{}}
	for _, c := range cfg {
		r.cfg[c.Name] = c
	}
	ctx := context.Background()
	limits, err := store.ListLimits(ctx)
	if err != nil {
		return nil, err
	}
	r.limits = limits
	usage, err := store.ListUsage(ctx)
	if err != nil {
		return nil, err
	}
	for _, u := range usage {
		r.setUsage(u)
	}
	return r, nil
}

// Run runs the turn on the first engine that is not blocked. If every engine
// is blocked it returns an *engine.UnavailableError.
func (r *Router) Run(ctx context.Context, t engine.Turn) (<-chan engine.Event, error) {
	first, why, err := r.pick(nil)
	if err != nil {
		return nil, err
	}
	out := make(chan engine.Event, 64)
	go func() {
		defer close(out)
		r.run(ctx, t, first, why, out)
	}()
	return out, nil
}

func (r *Router) run(ctx context.Context, t engine.Turn, eng engine.Engine, why string, out chan<- engine.Event) {
	tried := map[string]bool{}
	last := t.LastEngine
	cut := false // an engine stopped in the middle of this turn
	for {
		tried[eng.Name()] = true
		if last != "" && last != eng.Name() {
			text := "now on " + eng.Name()
			switch {
			case why != "":
				text += ": " + why
			case eng == r.engines[0]:
				text += " (first choice is free again)"
			}
			stream.Send(ctx, out, engine.Event{Kind: engine.KindSwitched, Engine: eng.Name(), Text: text})
		}
		turn := t
		if last != "" && last != eng.Name() {
			turn.Extra = r.handoff(ctx, t, last, cut)
		}
		blocked, ok := r.runOne(ctx, eng, turn, out)
		if ok || blocked == "" {
			return // done, stopped, or another error the session reports
		}
		last, cut = eng.Name(), true
		next, nextWhy, err := r.pick(tried)
		if err != nil {
			stream.Send(ctx, out, engine.Event{Kind: engine.KindError, Err: err})
			return
		}
		eng, why = next, blocked
		if nextWhy != "" {
			why += "; " + nextWhy
		}
	}
}

// runOne runs the turn on one engine and forwards its events. It reports
// why the engine is blocked now ("" = it is not: a usage limit, or its
// provider is down), and whether the turn finished.
func (r *Router) runOne(ctx context.Context, eng engine.Engine, t engine.Turn, out chan<- engine.Event) (blocked string, done bool) {
	events, err := eng.Run(ctx, t)
	if err != nil {
		// The engine cannot even start (not installed, logged out): treat it
		// like a limit so the turn moves on, and tell the user why.
		slog.Warn("engine failed to start", "engine", eng.Name(), "err", err)
		stream.Send(ctx, out, engine.Event{Kind: engine.KindError, Err: fmt.Errorf("%s: %w", eng.Name(), err)})
		r.markLimited(eng.Name(), time.Time{})
		return eng.Name() + " could not start", false
	}
	for ev := range events {
		switch ev.Kind {
		case engine.KindUsage:
			r.saveUsage(ev.Usage)
			continue // the router's business, not the session's
		case engine.KindError:
			if blocked != "" {
				// The engine's failure after its limit or outage (Claude's
				// "stopped without a result"): the turn moves on, so the
				// session must not see an error, nor a job count one.
				slog.Info("engine error after its block", "engine", eng.Name(), "err", ev.Err)
				continue
			}
		case engine.KindLimitHit:
			if ev.Outage {
				ev.ResetAt = r.outageUntil(eng.Name(), ev.ResetAt) // the session shows when it is tried again
				r.markLimited(eng.Name(), ev.ResetAt)
				blocked = eng.Name() + " is unavailable"
				if ev.Err != nil {
					blocked += " (" + text.Shorten(ev.Err.Error(), 120) + ")"
				}
			} else {
				r.markLimited(eng.Name(), ev.ResetAt)
				blocked = eng.Name() + " hit its usage limit"
			}
		case engine.KindDone:
			done = true
			r.mu.Lock()
			delete(r.outages, eng.Name()) // it answers again
			r.mu.Unlock()
		}
		stream.Send(ctx, out, ev)
	}
	return blocked, done
}

// outageUntil is how long an engine whose provider is down waits before it
// is tried again: 1, 2, 4 ... minutes while it stays down, at most
// maxOutageWait, and never less than the provider's own retry_after.
func (r *Router) outageUntil(name string, resetAt time.Time) time.Time {
	r.mu.Lock()
	r.outages[name]++
	n := r.outages[name]
	r.mu.Unlock()
	until := r.now().Add(min(time.Minute<<min(n-1, 5), maxOutageWait))
	if resetAt.After(until) {
		until = resetAt
	}
	return until
}

// pick returns the first engine (in config order) that is not limited, not
// reserved and not tried yet, with a short reason when it skipped others.
func (r *Router) pick(tried map[string]bool) (engine.Engine, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	var earliest time.Time
	var skipped []string
	for _, e := range r.engines {
		until, why := r.blocked(e.Name(), now)
		if tried[e.Name()] {
			// Tried in this turn, but its block still counts for when to
			// try again: else the wait is always unknownReset.
			if !until.IsZero() && (earliest.IsZero() || until.Before(earliest)) {
				earliest = until
			}
			continue
		}
		if until.IsZero() {
			reason := ""
			if len(skipped) > 0 {
				reason = strings.Join(skipped, "; ")
			}
			return e, reason, nil
		}
		skipped = append(skipped, e.Name()+" "+why)
		if earliest.IsZero() || until.Before(earliest) {
			earliest = until
		}
	}
	if earliest.IsZero() {
		earliest = now.Add(unknownReset)
	}
	return nil, "", &engine.UnavailableError{ResetAt: earliest}
}

// blocked returns until when an engine is blocked (zero = free) and why.
// Caller holds mu.
func (r *Router) blocked(name string, now time.Time) (time.Time, string) {
	if until := r.limits[name]; until.After(now) {
		return until, "limited until " + until.Format("15:04")
	}
	c := r.cfg[name]
	if c.ReserveOff {
		return time.Time{}, ""
	}
	for window, keep := range c.Reserve {
		u, ok := r.usage[name][window]
		if !ok || !u.ResetAt.After(now) {
			continue // unknown, or the window already reset
		}
		if left := 100 - u.UsedPercent; left <= keep {
			return u.ResetAt, fmt.Sprintf("reserved (%.0f%% of %s left, keeping %.0f%%)", left, window, keep)
		}
	}
	return time.Time{}, ""
}

func (r *Router) markLimited(name string, resetAt time.Time) {
	if resetAt.IsZero() || !resetAt.After(r.now()) {
		resetAt = r.now().Add(unknownReset)
	}
	r.mu.Lock()
	r.limits[name] = resetAt
	r.mu.Unlock()
	if err := r.store.UpdateLimit(context.Background(), name, resetAt); err != nil {
		slog.Error("save limit", "engine", name, "err", err)
	}
}

func (r *Router) saveUsage(u *engine.Usage) {
	if u == nil {
		return
	}
	r.mu.Lock()
	r.setUsage(*u)
	r.mu.Unlock()
	if err := r.store.UpdateUsage(context.Background(), *u); err != nil {
		slog.Error("save usage", "engine", u.Engine, "err", err)
	}
}

// setUsage stores u in memory. Caller holds mu (or is New).
func (r *Router) setUsage(u engine.Usage) {
	if r.usage[u.Engine] == nil {
		r.usage[u.Engine] = map[string]engine.Usage{}
	}
	r.usage[u.Engine][u.Window] = u
}
