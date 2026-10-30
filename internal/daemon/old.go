package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/viminizer/germes/internal/api"
	"github.com/viminizer/germes/internal/session"
)

// OldDaemons are the daemons that handed over to this one after an update
// and still finish their turns. Each one registers a file in dir that holds
// the path of its private socket and its process id.
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

// load reads the registry. A daemon that does not answer is not followed;
// its file is removed only once it surely exited (isExited).
func (o *OldDaemons) load(ctx context.Context) {
	files, _ := filepath.Glob(filepath.Join(o.dir, "*"))
	for _, file := range files {
		socket, _, err := readEntry(file)
		if err != nil {
			continue
		}
		c := api.NewClient(socket)
		pingCtx, cancel := context.WithTimeout(ctx, time.Second)
		_, err = c.GetDaemon(pingCtx)
		cancel()
		if err != nil {
			removeIfExited(file) // a crashed old daemon
			continue
		}
		o.mu.Lock()
		o.list[file] = c
		o.mu.Unlock()
		slog.Info("an old daemon finishes its turns", "socket", socket)
	}
}

// allExited reports whether every registered old daemon surely exited. It
// reads the registry itself: the in-memory list is empty before load and
// drops a daemon that only stopped answering.
func (o *OldDaemons) allExited() bool {
	files, err := filepath.Glob(filepath.Join(o.dir, "*"))
	if err != nil {
		return false
	}
	for _, file := range files {
		if !isExited(file) {
			return false
		}
	}
	return true
}

// relay passes the live events of every old daemon on to this daemon's
// watchers, until that daemon exits or ctx ends. A dropped stream is opened
// again while the old daemon still answers. When it is gone, turns it left
// unfinished (it crashed) are marked interrupted, and exited is called.
func (o *OldDaemons) relay(ctx context.Context, sessions *session.Manager, exited func()) {
	for file, c := range o.clients() {
		go func() {
			defer func() {
				o.drop(file)
				if ctx.Err() == nil {
					removeIfExited(file) // a crashed one could not remove it
					if err := sessions.Start(ctx); err != nil {
						slog.Error("recover sessions of an old daemon", "err", err)
					}
					exited()
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

// readEntry reads a registry file: the old daemon's private socket and its
// process id (0 when an older version wrote the file).
func readEntry(file string) (socket string, pid int, err error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return "", 0, err
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return "", 0, fmt.Errorf("registry file %s is empty", file)
	}
	if len(fields) > 1 {
		if pid, err = strconv.Atoi(fields[1]); err != nil {
			return "", 0, fmt.Errorf("registry file %s: bad process id: %w", file, err)
		}
	}
	return fields[0], pid, nil
}

// isExited reports whether the old daemon of a registry file surely exited:
// its file is gone, or its process no longer exists. Not answering is no
// proof: a busy daemon can be slow. A file without a process id (written by
// an older version) or one that cannot be read proves nothing either.
func isExited(file string) bool {
	_, pid, err := readEntry(file)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return true
	case err != nil || pid <= 0:
		return false
	}
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

// removeIfExited removes a registry file and the socket it names once its
// daemon surely exited. A file from an older version, without a process id,
// is removed as before, when its daemon stops answering.
// ponytail: those older files cannot prove an exit; they last only until
// every daemon from before this change is gone.
func removeIfExited(file string) {
	socket, pid, err := readEntry(file)
	if err != nil || (pid > 0 && !isExited(file)) {
		return
	}
	os.Remove(socket)
	os.Remove(file)
}
