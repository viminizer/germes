// Package daemon is the background process. It implements api.Service and
// serves it on the unix socket. The real work is in session and job.
//
// Update: when the germes binary or the config changes, the daemon hands
// over at once. It starts a new daemon, which takes the socket and every new
// request, and finishes its own running turns on a private socket before it
// exits. The new daemon passes those turns' events on, and holds a session's
// next turn until the old daemon is done with it. Nobody waits, nothing is cut.
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
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/viminizer/germes/internal/api"
	"github.com/viminizer/germes/internal/job"
	"github.com/viminizer/germes/internal/session"
)

const (
	updateCheck  = 3 * time.Second        // how often the daemon looks for a new binary or config
	handOverTime = 15 * time.Second       // how long the new daemon may take to come up
	finishCheck  = 200 * time.Millisecond // how often an old daemon checks whether its turns are done
)

// Options configure the daemon.
type Options struct {
	Version    string
	ConfigPath string // watched like the binary: a change hands over to a new daemon
	SocketPath string
	LockPath   string
	OldDir     string       // where an old daemon registers its private socket
	StartNew   func() error // starts a new daemon process (detached)
}

// Daemon serves the sessions and jobs.
type Daemon struct {
	sessions  *session.Manager
	jobs      *job.Scheduler
	old       *OldDaemons
	opts      Options
	startedAt time.Time
	stop      chan struct{}
	stopOnce  sync.Once
	update    chan struct{} // a new binary or config: hand over (watchFiles, or a test)
}

var _ api.Service = (*Daemon)(nil)

// New creates the daemon. old must be the one the session manager asks.
func New(sessions *session.Manager, jobs *job.Scheduler, old *OldDaemons, opts Options) *Daemon {
	return &Daemon{sessions: sessions, jobs: jobs, old: old, opts: opts, startedAt: time.Now(),
		stop: make(chan struct{}), update: make(chan struct{}, 1)}
}

// Run serves until SIGTERM, SIGINT, StopDaemon or ctx ends, or until it has
// handed over to a new daemon and finished its turns.
func (d *Daemon) Run(ctx context.Context) error {
	unlock, err := lock(d.opts.LockPath)
	if err != nil {
		return err
	}
	defer func() { unlock() }()

	// We hold the lock, so a socket file here is from a crashed daemon or
	// from an old one that handed over (it listens on its private socket).
	if err := os.Remove(d.opts.SocketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove old socket: %w", err)
	}
	ln, err := listen(d.opts.SocketPath)
	if err != nil {
		return err
	}
	ownSocket := true
	defer func() {
		ln.Close()
		if ownSocket {
			os.Remove(d.opts.SocketPath)
		}
	}()

	// Old daemons first: their running sessions are not interrupted.
	d.old.load(ctx)
	if err := d.sessions.Start(ctx); err != nil {
		slog.Error("recover sessions", "err", err)
	}
	if err := d.jobs.Start(ctx); err != nil {
		slog.Error("resume jobs", "err", err)
	}

	// Watch streams never end on their own; cancel them on shutdown or
	// hand-over, so clients reconnect to the new daemon.
	serveCtx, cancelServe := context.WithCancel(ctx)
	defer cancelServe()
	d.old.relay(serveCtx, d.sessions)
	srv := &http.Server{Handler: api.NewHandler(d), BaseContext: func(net.Listener) context.Context { return serveCtx }}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	slog.Info("daemon started", "version", d.opts.Version, "pid", os.Getpid(), "socket", d.opts.SocketPath)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sig)
	go d.watchFiles(serveCtx)
	for {
		select {
		case <-ctx.Done():
		case <-d.stop:
		case s := <-sig:
			slog.Info("signal", "signal", s.String())
		case err := <-serveErr:
			return fmt.Errorf("serve: %w", err)
		case <-d.update:
			slog.Info("germes binary or config changed, handing over to a new daemon")
			private, err := d.handOver(ctx, unlock)
			if err != nil {
				slog.Error("hand-over failed; this daemon keeps serving", "err", err)
				if unlock, err = d.keepServing(ctx); err == nil {
					continue
				}
				// Another daemon took the lock meanwhile: it serves now.
				slog.Error("cannot keep serving", "err", err)
			}
			ownSocket = false // the new daemon's now
			cancelServe()
			shutdown(srv)
			return d.finishTurns(private, sig)
		}
		break
	}

	slog.Info("daemon stopping")
	cancelServe()
	shutdown(srv)
	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Jobs first: a job goroutine must not start a round in a stopped manager.
	if err := d.jobs.Stop(stopCtx); err != nil {
		slog.Warn("stop jobs", "err", err)
	}
	if err := d.sessions.Stop(stopCtx); err != nil {
		slog.Warn("stop sessions", "err", err)
	}
	return nil
}

// handOver starts the new daemon and waits until it serves the socket. This
// daemon's jobs stop (the new daemon resumes them); its turns go on. Before
// the new daemon starts, this one already serves a private socket and lists
// it in OldDir, so the new daemon finds it.
func (d *Daemon) handOver(ctx context.Context, unlock func()) (*privateServer, error) {
	stopCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := d.jobs.Stop(stopCtx); err != nil {
		return nil, fmt.Errorf("stop jobs: %w", err)
	}
	p, err := d.servePrivate()
	if err != nil {
		return nil, err
	}
	unlock()
	if err := d.opts.StartNew(); err != nil {
		p.close()
		return nil, fmt.Errorf("start the new daemon: %w", err)
	}
	deadline := time.Now().Add(handOverTime)
	for time.Now().Before(deadline) {
		// A new client each time: a kept-alive connection would still
		// reach this daemon.
		pingCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		other, err := api.NewClient(d.opts.SocketPath).GetDaemon(pingCtx)
		cancel()
		if err == nil && !other.StartedAt.Equal(d.startedAt) {
			slog.Info("handed over", "new_version", other.Version, "new_pid", other.PID)
			return p, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	p.close()
	return nil, errors.New("the new daemon did not come up; see the log")
}

// privateServer is an old daemon's socket for the new daemon.
type privateServer struct {
	srv              *http.Server
	cancel           context.CancelFunc
	socket, registry string
}

// servePrivate serves the Service on a new private socket and lists it in
// OldDir.
func (d *Daemon) servePrivate() (*privateServer, error) {
	socket := fmt.Sprintf("/tmp/germes-%d-old-%s.sock", os.Getuid(), api.NewID()) // short: socket paths are limited
	ln, err := listen(socket)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &privateServer{socket: socket, registry: filepath.Join(d.opts.OldDir, filepath.Base(socket)), cancel: cancel,
		srv: &http.Server{Handler: api.NewHandler(d), BaseContext: func(net.Listener) context.Context { return ctx }}}
	go p.srv.Serve(ln)
	if err := os.MkdirAll(d.opts.OldDir, 0o700); err != nil {
		p.close()
		return nil, err
	}
	if err := os.WriteFile(p.registry, []byte(socket), 0o600); err != nil {
		p.close()
		return nil, err
	}
	return p, nil
}

// close unlists the socket first, so the new daemon stops asking, then
// stops serving it.
func (p *privateServer) close() {
	os.Remove(p.registry)
	p.cancel()
	shutdown(p.srv)
	os.Remove(p.socket)
}

// keepServing takes the lock back after a failed hand-over and resumes the
// jobs. It returns the new unlock.
func (d *Daemon) keepServing(ctx context.Context) (func(), error) {
	unlock, err := lock(d.opts.LockPath)
	if err != nil {
		return nil, fmt.Errorf("lock after a failed hand-over: %w", err)
	}
	if err := d.jobs.Start(ctx); err != nil {
		slog.Error("resume jobs", "err", err)
	}
	return unlock, nil
}

// finishTurns waits until no turn runs here, then stops. The new daemon
// watches and steers these turns through the private socket. A nil one (a
// failed hand-over) just lets the turns end. StopDaemon or a signal cuts
// them.
func (d *Daemon) finishTurns(p *privateServer, sig <-chan os.Signal) error {
	if p != nil {
		defer p.close()
	}
	t := time.NewTicker(finishCheck)
	defer t.Stop()
wait:
	for len(d.sessions.ListBusy()) > 0 {
		select {
		case <-t.C:
		case <-d.stop:
			break wait
		case <-sig:
			break wait
		}
	}
	slog.Info("old daemon exiting")
	stopCtx, cancelStop := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelStop()
	return d.sessions.Stop(stopCtx)
}

// shutdown stops a server: no new requests, open ones get a few seconds.
func shutdown(srv *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Warn("http shutdown", "err", err)
	}
}

// watchFiles asks Run to hand over once the germes binary or the config
// file changes on disk.
func (d *Daemon) watchFiles(ctx context.Context) {
	exe, err := os.Executable()
	if err != nil {
		slog.Warn("cannot watch for updates", "err", err)
		return
	}
	paths := []string{exe, d.opts.ConfigPath}
	start := make([]os.FileInfo, len(paths))
	for i, p := range paths {
		start[i], _ = os.Stat(p) // nil: missing (a config may not exist yet)
	}
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
		for i, p := range paths {
			start[i], _ = os.Stat(p) // after a failed hand-over, wait for the next change
		}
		select {
		case d.update <- struct{}{}:
		default:
		}
	}
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

// listen opens an owner-only unix socket. Closing it never removes the
// file: after a hand-over the path belongs to the new daemon.
func listen(path string) (net.Listener, error) {
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", path, err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("chmod socket: %w", err)
	}
	return ln, nil
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
	var once sync.Once
	return func() { once.Do(func() { f.Close() }) }, nil
}
