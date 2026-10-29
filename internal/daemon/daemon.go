// Package daemon is the background process. It implements api.Service and
// serves it on the unix socket. The real work is in session.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/viminizer/germes/internal/api"
	"github.com/viminizer/germes/internal/session"
)

// ErrUpdated is returned by Run when the germes binary was replaced (for
// example by `make install`) or the config changed, and no session was working. The caller starts
// the new binary in its place.
var ErrUpdated = errors.New("germes binary updated")

// updateCheck is how often the daemon looks for a new binary.
const updateCheck = 3 * time.Second

// Daemon serves the sessions.
type Daemon struct {
	sessions   *session.Manager
	version    string
	configPath string // watched: a change restarts the daemon when idle
	startedAt  time.Time
	stop       chan struct{}
	stopOnce   sync.Once
}

var _ api.Service = (*Daemon)(nil)

// New creates the daemon.
func New(sessions *session.Manager, version, configPath string) *Daemon {
	return &Daemon{sessions: sessions, version: version, configPath: configPath, startedAt: time.Now(), stop: make(chan struct{})}
}

// Run serves until SIGTERM, SIGINT, StopDaemon or ctx ends.
func (d *Daemon) Run(ctx context.Context, socketPath, lockPath string) error {
	unlock, err := lock(lockPath)
	if err != nil {
		return err
	}
	defer unlock()

	// We hold the lock, so a socket file left here is from a crashed daemon.
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove old socket: %w", err)
	}
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listen %s: %w", socketPath, err)
	}
	defer os.Remove(socketPath)
	if err := os.Chmod(socketPath, 0o600); err != nil {
		return fmt.Errorf("chmod socket: %w", err)
	}

	if err := d.sessions.Start(ctx); err != nil {
		slog.Error("recover sessions", "err", err)
	}

	// Watch streams never end on their own; cancel them on shutdown.
	serveCtx, cancelServe := context.WithCancel(ctx)
	defer cancelServe()
	srv := &http.Server{Handler: api.NewHandler(d), BaseContext: func(net.Listener) context.Context { return serveCtx }}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	slog.Info("daemon started", "version", d.version, "pid", os.Getpid(), "socket", socketPath)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sig)
	updated := d.watchFiles(serveCtx)
	result := error(nil)
	select {
	case <-ctx.Done():
	case <-d.stop:
	case <-updated:
		slog.Info("germes binary or config changed, restarting")
		result = ErrUpdated
	case s := <-sig:
		slog.Info("signal", "signal", s.String())
	case err := <-serveErr:
		return fmt.Errorf("serve: %w", err)
	}

	slog.Info("daemon stopping")
	cancelServe()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("http shutdown", "err", err)
	}
	if err := d.sessions.Stop(shutdownCtx); err != nil {
		slog.Warn("stop sessions", "err", err)
	}
	return result
}

// watchFiles closes the returned channel once the germes binary or the
// config file changes on disk and no session is working, so an update never
// cuts a turn. Run then restarts the daemon, which reloads both.
func (d *Daemon) watchFiles(ctx context.Context) <-chan struct{} {
	changed := make(chan struct{})
	exe, err := os.Executable()
	if err != nil {
		slog.Warn("cannot watch for updates", "err", err)
		return changed
	}
	paths := []string{exe, d.configPath}
	start := make([]os.FileInfo, len(paths))
	for i, p := range paths {
		start[i], _ = os.Stat(p) // nil: missing (a config may not exist yet)
	}
	go func() {
		t := time.NewTicker(updateCheck)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if !anyChanged(paths, start) {
				continue
			}
			busy, err := d.sessions.ListSessions(ctx, api.SessionFilter{Status: api.StatusWorking, Limit: 1})
			if err != nil || len(busy.Items) > 0 {
				continue // wait until no turn is running
			}
			close(changed)
			return
		}
	}()
	return changed
}

// anyChanged reports whether a file was replaced, rewritten or created. A
// file that is missing now (being written) does not count yet.
func anyChanged(paths []string, start []os.FileInfo) bool {
	for i, p := range paths {
		if p == "" {
			continue
		}
		now, err := os.Stat(p)
		if err != nil {
			continue
		}
		if start[i] == nil || !os.SameFile(start[i], now) || !now.ModTime().Equal(start[i].ModTime()) {
			return true
		}
	}
	return false
}

// lock takes an exclusive lock on path, or fails if another daemon has it.
func lock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("another germes daemon is already running")
	}
	return func() { f.Close() }, nil
}
