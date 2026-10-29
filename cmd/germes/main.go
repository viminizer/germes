// Command germes is the Germes harness: the TUI, the daemon and the small
// commands. This file is the only place that wires modules together.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"hash/crc32"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/viminizer/germes/internal/api"
	"github.com/viminizer/germes/internal/cli"
	"github.com/viminizer/germes/internal/config"
	"github.com/viminizer/germes/internal/daemon"
	"github.com/viminizer/germes/internal/engine/claude"
	"github.com/viminizer/germes/internal/session"
	"github.com/viminizer/germes/internal/store"
	"github.com/viminizer/germes/internal/tui"
)

// version is set at build time from git (see Makefile).
var version = "dev"

const usage = `usage:
  germes                start a new session in this folder
  germes -c             continue the last session in this folder
  germes attach <id>    open any session
  germes ls             list sessions
  germes stop <id>      stop a session's current step
  germes daemon stop    stop the daemon
  germes daemon restart restart the daemon (after an upgrade)`

// paths are the files under the Germes home folder.
type paths struct {
	home, config, db, socket, lock, log string
}

func newPaths() paths {
	home := os.Getenv("GERMES_HOME")
	if home == "" {
		h, _ := os.UserHomeDir()
		home = filepath.Join(h, ".germes")
	}
	socket := filepath.Join(home, "germes.sock")
	// Unix socket paths are limited to about 104 bytes (macOS). A deep home
	// gets a short socket in /tmp, unique per user and home.
	if len(socket) > 100 {
		socket = fmt.Sprintf("/tmp/germes-%d-%x.sock", os.Getuid(), crc32.ChecksumIEEE([]byte(home)))
	}
	return paths{
		home: home, config: filepath.Join(home, "config.yaml"), db: filepath.Join(home, "germes.db"),
		socket: socket, lock: filepath.Join(home, "daemon.lock"),
		log: filepath.Join(home, "logs", "daemon.log"),
	}
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "germes:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	p := newPaths()
	if err := os.MkdirAll(filepath.Dir(p.log), 0o700); err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	client := api.NewClient(p.socket)

	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "daemon":
		if len(args) > 1 {
			return daemonCommand(ctx, p, client, args[1])
		}
		return runDaemon(p)
	case "ls":
		if err := ensureDaemon(ctx, p, client); err != nil {
			return err
		}
		return cli.ListSessions(ctx, client, os.Stdout)
	case "stop":
		if len(args) < 2 {
			return errors.New("usage: germes stop <id>")
		}
		if err := ensureDaemon(ctx, p, client); err != nil {
			return err
		}
		return cli.StopSession(ctx, client, args[1], os.Stdout)
	case "attach":
		if len(args) < 2 {
			return errors.New("usage: germes attach <id>")
		}
		return runTUI(ctx, p, client, args[1], false)
	case "help", "-h", "--help":
		fmt.Println(usage)
		return nil
	}

	fs := flag.NewFlagSet("germes", flag.ContinueOnError)
	cont := fs.Bool("c", false, "continue the last session in this folder")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, usage) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unknown command %q\n%s", fs.Arg(0), usage)
	}
	return runTUI(ctx, p, client, "", *cont)
}

func runTUI(ctx context.Context, p paths, client *api.Client, sessionID string, cont bool) error {
	if _, err := exec.LookPath("claude"); err != nil {
		return errors.New("no engine found: install Claude Code (https://claude.com/claude-code) and log in with `claude`")
	}
	if err := ensureDaemon(ctx, p, client); err != nil {
		return err
	}
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	project := session.GetProjectRoot(dir)
	if cont {
		page, err := client.ListSessions(ctx, api.SessionFilter{Project: project, Limit: 1})
		if err != nil {
			return err
		}
		if len(page.Items) == 0 {
			return errors.New("no session in this folder yet; run `germes` to start one")
		}
		sessionID = page.Items[0].ID
	}
	if sessionID != "" {
		s, err := client.GetSession(ctx, sessionID)
		if err != nil {
			return err
		}
		dir, project = s.Dir, s.Project
	}
	t := tui.New(client, tui.Options{
		Dir: dir, Project: project, SessionID: sessionID, Version: version, Engine: claude.Name,
		EnsureDaemon: func(ctx context.Context) error { return ensureDaemon(ctx, p, client) },
	})
	return t.Run(ctx)
}

func daemonCommand(ctx context.Context, p paths, client *api.Client, sub string) error {
	switch sub {
	case "stop":
		return cli.StopDaemon(ctx, client, os.Stdout)
	case "restart":
		if err := client.StopDaemon(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "no daemon was running")
		}
		if err := waitDaemonGone(ctx, client); err != nil {
			return err
		}
		if err := ensureDaemon(ctx, p, client); err != nil {
			return err
		}
		fmt.Println("daemon restarted")
		return nil
	}
	return fmt.Errorf("unknown daemon command %q", sub)
}

// runDaemon wires every module and serves until stopped.
func runDaemon(p paths) error {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	cfg, err := config.Load(p.config)
	if err != nil {
		return err
	}
	if _, err := os.Stat(p.config); errors.Is(err, os.ErrNotExist) {
		if err := config.Save(p.config, cfg); err != nil {
			slog.Warn("write default config", "err", err)
		}
	}
	st, err := store.New(p.db)
	if err != nil {
		return fmt.Errorf("database: %w (the file was not changed)", err)
	}
	defer st.Close()

	eng := claude.New(claude.Options{Model: cfg.GetEngine(claude.Name).Model, Blocked: cfg.Blocked})
	sessions := session.NewManager(st, eng)
	return daemon.New(sessions, version).Run(context.Background(), p.socket, p.lock)
}

// ensureDaemon connects to the daemon, or starts it in the background and
// waits for its socket.
func ensureDaemon(ctx context.Context, p paths, client *api.Client) error {
	if d, err := ping(ctx, client); err == nil {
		warnVersion(d)
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(p.log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	cmd := exec.Command(exe, "daemon")
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // survives this terminal
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}
	cmd.Process.Release()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := ping(ctx, client); err == nil {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("the daemon did not start; see %s", p.log)
}

func ping(ctx context.Context, client *api.Client) (api.Daemon, error) {
	ctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	return client.GetDaemon(ctx)
}

func waitDaemonGone(ctx context.Context, client *api.Client) error {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := ping(ctx, client); err != nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("the old daemon did not stop")
}

var warned bool

func warnVersion(d api.Daemon) {
	if d.Version != version && !warned {
		warned = true
		fmt.Fprintf(os.Stderr, "note: the daemon runs version %s, this is %s. Run `germes daemon restart`.\n", d.Version, version)
	}
}
