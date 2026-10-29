package daemon

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/viminizer/germes/internal/api"
	"github.com/viminizer/germes/internal/session"
)

// OldDaemons are the daemons that handed over to this one after an update
// and still finish their turns. Each one registers a file in dir that holds
// the path of its private socket.
type OldDaemons struct {
	dir string

	mu   sync.Mutex
	list map[string]*api.Client // registry file -> client
}

// NewOldDaemons creates the list. It is empty until the daemon starts.
func NewOldDaemons(dir string) *OldDaemons {
	return &OldDaemons{dir: dir, list: map[string]*api.Client{}}
}

// IsBusy reports whether an old daemon still runs a session. An old daemon
// that does not answer is gone and leaves the list.
func (o *OldDaemons) IsBusy(ctx context.Context, sessionID string) bool {
	for file, c := range o.clients() {
		ctx, cancel := context.WithTimeout(ctx, time.Second)
		d, err := c.GetDaemon(ctx)
		cancel()
		if err != nil {
			o.drop(file)
			continue
		}
		if slices.Contains(d.Busy, sessionID) {
			return true
		}
	}
	return false
}

// load reads the registry. Files of daemons that do not answer are removed.
func (o *OldDaemons) load(ctx context.Context) {
	files, _ := filepath.Glob(filepath.Join(o.dir, "*"))
	for _, file := range files {
		b, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		c := api.NewClient(strings.TrimSpace(string(b)))
		pingCtx, cancel := context.WithTimeout(ctx, time.Second)
		_, err = c.GetDaemon(pingCtx)
		cancel()
		if err != nil {
			removeEntry(file) // a crashed old daemon
			continue
		}
		o.mu.Lock()
		o.list[file] = c
		o.mu.Unlock()
		slog.Info("an old daemon finishes its turns", "socket", strings.TrimSpace(string(b)))
	}
}

// relay passes the live events of every old daemon on to this daemon's
// watchers, until that daemon exits or ctx ends. A dropped stream is opened
// again while the old daemon still answers. When it is gone, turns it left
// unfinished (it crashed) are marked interrupted.
func (o *OldDaemons) relay(ctx context.Context, sessions *session.Manager) {
	for file, c := range o.clients() {
		go func() {
			defer func() {
				o.drop(file)
				if ctx.Err() == nil {
					removeEntry(file) // gone; a crashed one could not remove it
					if err := sessions.Start(ctx); err != nil {
						slog.Error("recover sessions of an old daemon", "err", err)
					}
				}
			}()
			for ctx.Err() == nil {
				events, err := c.Watch(ctx, api.WatchFilter{})
				if err != nil {
					return // gone
				}
				for ev := range events {
					sessions.SendEvent(ev)
				}
			}
		}()
	}
}

// stopSession stops the session's turn in every old daemon (a no-op where
// it does not run).
func (o *OldDaemons) stopSession(ctx context.Context, r api.StopRequest) {
	for _, c := range o.clients() {
		if err := c.StopSession(ctx, r); err != nil {
			slog.Warn("stop a session in an old daemon", "session", r.ID, "err", err)
		}
	}
}

func (o *OldDaemons) clients() map[string]*api.Client {
	o.mu.Lock()
	defer o.mu.Unlock()
	clients := make(map[string]*api.Client, len(o.list))
	for file, c := range o.list {
		clients[file] = c
	}
	return clients
}

func (o *OldDaemons) drop(file string) {
	o.mu.Lock()
	delete(o.list, file)
	o.mu.Unlock()
}

// removeEntry removes a registry file and the socket it names.
func removeEntry(file string) {
	if b, err := os.ReadFile(file); err == nil {
		os.Remove(strings.TrimSpace(string(b)))
	}
	os.Remove(file)
}
