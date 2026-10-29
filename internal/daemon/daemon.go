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

// Daemon serves the sessions.
type Daemon struct {
	sessions  *session.Manager
	version   string
	startedAt time.Time
	stop      chan struct{}
	stopOnce  sync.Once
}

var _ api.Service = (*Daemon)(nil)

// New creates the daemon.
func New(sessions *session.Manager, version string) *Daemon {
	return &Daemon{sessions: sessions, version: version, startedAt: time.Now(), stop: make(chan struct{})}
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
	select {
	case <-ctx.Done():
	case <-d.stop:
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
	return nil
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
