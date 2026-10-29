package session

import (
	"context"
	"sync"

	"github.com/viminizer/germes/internal/api"
)

// watcherBuffer is how many live events a watcher may fall behind before it
// is dropped. A dropped client reconnects with AfterSeq and catches up.
const watcherBuffer = 1024

type watcher struct {
	sessionID string // "" = all sessions
	ch        chan api.Event
}

// hub fans live events out to watchers.
type hub struct {
	mu       sync.Mutex
	watchers map[*watcher]struct{}
}

func (h *hub) subscribe(sessionID string) *watcher {
	w := &watcher{sessionID: sessionID, ch: make(chan api.Event, watcherBuffer)}
	h.mu.Lock()
	h.watchers[w] = struct{}{}
	h.mu.Unlock()
	return w
}

func (h *hub) unsubscribe(w *watcher) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.watchers[w]; ok {
		delete(h.watchers, w)
		close(w.ch)
	}
}

// publish never blocks: a watcher that cannot keep up is dropped.
func (h *hub) publish(ev api.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for w := range h.watchers {
		if w.sessionID != "" && w.sessionID != ev.SessionID {
			continue
		}
		select {
		case w.ch <- ev:
		default:
			delete(h.watchers, w)
			close(w.ch)
		}
	}
}

// Watch streams a session's events: stored events after f.AfterSeq first,
// then live ones. It subscribes before reading history, so no event falls
// into the gap. The channel closes when ctx ends or the watcher is dropped.
func (m *Manager) Watch(ctx context.Context, f api.WatchFilter) (<-chan api.Event, error) {
	live := m.hub.subscribe(f.SessionID)
	out := make(chan api.Event, 64)
	go func() {
		defer close(out)
		defer m.hub.unsubscribe(live)
		last := f.AfterSeq
		send := func(ev api.Event) bool {
			select {
			case out <- ev:
				return true
			case <-ctx.Done():
				return false
			}
		}
		if f.SessionID != "" {
			for {
				page, err := m.store.ListEvents(ctx, api.EventFilter{SessionID: f.SessionID, AfterSeq: last})
				if err != nil {
					return
				}
				for _, ev := range page.Items {
					if !send(ev) {
						return
					}
					last = ev.Seq
				}
				if page.Next == "" {
					break
				}
			}
		}
		for {
			select {
			case ev, ok := <-live.ch:
				if !ok {
					return
				}
				// Seq is per session, so only a one-session watch can skip.
				if f.SessionID != "" && ev.Seq != 0 && ev.Seq <= last {
					continue // already sent from history
				}
				if !send(ev) {
					return
				}
				if f.SessionID != "" && ev.Seq != 0 {
					last = ev.Seq
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}
