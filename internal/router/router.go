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
)

// unknownReset is how long an engine stays blocked when its limit message
// has no reset time. A failed try costs little, so then it is tried again.
const unknownReset = 30 * time.Minute

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

	mu     sync.Mutex
	limits map[string]time.Time               // engine -> limited until
	usage  map[string]map[string]engine.Usage // engine -> window -> usage
}

// New creates the router and loads the saved limits and usage.
func New(engines []engine.Engine, cfg []config.Engine, store Store, now func() time.Time) (*Router, error) {
	if len(engines) == 0 {
		return nil, errors.New("router: no engines")
	}
	r := &Router{engines: engines, cfg: map[string]config.Engine{}, store: store, now: now, usage: map[string]map[string]engine.Usage{}}
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
			turn.Extra = r.handoff(ctx, t, last)
		}
		limited, ok := r.runOne(ctx, eng, turn, out)
		if ok || !limited {
			return // done, stopped, or a non-limit error the session reports
		}
		last = eng.Name()
		next, nextWhy, err := r.pick(tried)
		if err != nil {
			stream.Send(ctx, out, engine.Event{Kind: engine.KindError, Err: err})
			return
		}
		eng, why = next, last+" hit its usage limit"
		if nextWhy != "" {
			why += "; " + nextWhy
		}
	}
}

// runOne runs the turn on one engine and forwards its events. It reports
// whether the engine hit a limit, and whether the turn finished.
func (r *Router) runOne(ctx context.Context, eng engine.Engine, t engine.Turn, out chan<- engine.Event) (limited, done bool) {
	events, err := eng.Run(ctx, t)
	if err != nil {
		// The engine cannot even start (not installed, logged out): treat it
		// like a limit so the turn moves on, and tell the user why.
		slog.Warn("engine failed to start", "engine", eng.Name(), "err", err)
		stream.Send(ctx, out, engine.Event{Kind: engine.KindError, Err: fmt.Errorf("%s: %w", eng.Name(), err)})
		r.markLimited(eng.Name(), time.Time{})
		return true, false
	}
	for ev := range events {
		switch ev.Kind {
		case engine.KindUsage:
			r.saveUsage(ev.Usage)
			continue // the router's business, not the session's
		case engine.KindLimitHit:
			r.markLimited(eng.Name(), ev.ResetAt)
			limited = true
		case engine.KindDone:
			done = true
		}
		stream.Send(ctx, out, ev)
	}
	return limited, done
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
		if tried[e.Name()] {
			continue
		}
		until, why := r.blocked(e.Name(), now)
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
