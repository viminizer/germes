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
	"github.com/viminizer/germes/internal/engine"
	"github.com/viminizer/germes/internal/engine/claude"
	"github.com/viminizer/germes/internal/engine/loop"
	"github.com/viminizer/germes/internal/env"
	"github.com/viminizer/germes/internal/job"
	"github.com/viminizer/germes/internal/llm/codex"
	"github.com/viminizer/germes/internal/memory"
	"github.com/viminizer/germes/internal/prompt"
	"github.com/viminizer/germes/internal/refs"
	"github.com/viminizer/germes/internal/router"
	"github.com/viminizer/germes/internal/session"
	"github.com/viminizer/germes/internal/skill"
	"github.com/viminizer/germes/internal/store"
	"github.com/viminizer/germes/internal/tool"
	"github.com/viminizer/germes/internal/tools/files"
	memorytool "github.com/viminizer/germes/internal/tools/memory"
	"github.com/viminizer/germes/internal/tools/search"
	"github.com/viminizer/germes/internal/tools/shell"
	"github.com/viminizer/germes/internal/tools/skills"
	worktreetool "github.com/viminizer/germes/internal/tools/worktree"
	"github.com/viminizer/germes/internal/tui"
)

// version is set at build time from git (see Makefile).
var version = "dev"

const usage = `usage:
  germes                start a new session in this folder
  germes -c             continue the last session in this folder
  germes --no-worktree  start a session in this folder instead of its own git worktree
  germes attach <id>    open any session, or a job's latest round (follows new rounds)
  germes ls             list sessions
  germes stop <id>      stop a session's current step
  germes job add [--every 1h] [--max-runs n] [--name x] [--no-worktree] "<prompt>"
                        run a prompt again and again in this folder, a new session each round
  germes job ls|start|stop|rm <id>  list, start, stop or delete jobs
  germes reserve on|off <engine>  keep or drop an engine's usage reserve
  germes daemon stop    stop the daemon
  germes daemon restart restart the daemon (it also updates itself when idle)`

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
	if abs, err := filepath.Abs(home); err == nil {
		home = abs // the same socket and files from any folder
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
		return runTUI(ctx, p, client, args[1], false, false)
	case "job":
		if err := ensureDaemon(ctx, p, client); err != nil {
			return err
		}
		return jobCommand(ctx, client, args[1:])
	case "reserve":
		return reserveCommand(p, args[1:])
	case "help", "-h", "--help":
		fmt.Println(usage)
		return nil
	}

	fs := flag.NewFlagSet("germes", flag.ContinueOnError)
	cont := fs.Bool("c", false, "continue the last session in this folder")
	noWorktree := fs.Bool("no-worktree", false, "work in this folder, not in the session's own git worktree")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, usage) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unknown command %q\n%s", fs.Arg(0), usage)
	}
	return runTUI(ctx, p, client, "", *cont, *noWorktree)
}

func runTUI(ctx context.Context, p paths, client *api.Client, sessionID string, cont, noWorktree bool) error {
	cfg, err := config.Load(p.config)
	if err != nil {
		return err
	}
	engines := availableEngines(p, cfg)
	if len(engines) == 0 {
		return errors.New("no engine found: log in with `codex login`, or install Claude Code (https://claude.com/claude-code)")
	}
	engineName := engines[0] // the status bar follows the session's real engine
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
	jobID := ""
	if sessionID != "" {
		s, err := client.GetSession(ctx, sessionID)
		var e *api.Error
		if errors.As(err, &e) && e.Code == api.CodeNotFound {
			// Not a session: maybe a job. Open its latest round.
			j, jerr := client.GetJob(ctx, sessionID)
			if jerr != nil {
				return fmt.Errorf("no session or job %q", sessionID)
			}
			if j.SessionID == "" {
				return fmt.Errorf("job %s has not started a round yet; try again in a moment", j.Name)
			}
			jobID, sessionID = j.ID, j.SessionID
			s, err = client.GetSession(ctx, sessionID)
		}
		if err != nil {
			return err
		}
		dir, project = s.Dir, s.Project
	}
	t := tui.New(client, tui.Options{
		Dir: dir, Project: project, SessionID: sessionID, JobID: jobID, Version: version, Engine: engineName, NoWorktree: noWorktree,
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

	rt, reg, err := newRouter(p, cfg, st)
	if err != nil {
		return err
	}
	old := daemon.NewOldDaemons(filepath.Join(p.home, "old"))
	sessions := session.NewManager(st, rt, session.Options{Worktrees: filepath.Join(p.home, "worktrees"), Refs: refs.New(reg), Elsewhere: old})
	jobs := job.NewScheduler(st, sessions)
	return daemon.New(sessions, jobs, old, daemon.Options{
		Version: version, ConfigPath: p.config, SocketPath: p.socket, LockPath: p.lock, OldDir: filepath.Join(p.home, "old"),
		StartNew: func() error { return startDaemon(p) },
	}).Run(context.Background())
}

// ensureDaemon connects to the daemon, or starts it in the background and
// waits for its socket.
func ensureDaemon(ctx context.Context, p paths, client *api.Client) error {
	if _, err := ping(ctx, client); err == nil {
		return nil // an older daemon hands over to the new binary on its own
	}
	if err := startDaemon(p); err != nil {
		return err
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := ping(ctx, client); err == nil {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("the daemon did not start; see %s", p.log)
}

// startDaemon starts `germes daemon` in the background, logging to the
// daemon log. It does not wait for it to start; a goroutine collects its
// exit, so a daemon that ends (after handing over) is not left as a zombie.
func startDaemon(p paths) error {
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
	go cmd.Wait()
	return nil
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

// availableEngines lists the engines in config order that can run here.
func availableEngines(p paths, cfg config.Config) []string {
	var names []string
	for _, e := range cfg.Engines {
		switch e.Name {
		case "codex-api":
			if codex.IsAvailable(p.home, codexDir()) {
				names = append(names, e.Name)
			}
		case claude.Name:
			if _, err := exec.LookPath("claude"); err == nil {
				names = append(names, e.Name)
			}
		}
	}
	return names
}

// newRouter builds every available engine, in config order, behind the router.
// newRouter also returns the tool registry: @ references expand through it.
func newRouter(p paths, cfg config.Config, st *store.Store) (*router.Router, *tool.Registry, error) {
	names := availableEngines(p, cfg)
	if len(names) == 0 {
		return nil, nil, errors.New("no engine found: log in with `codex login`, or install Claude Code")
	}
	mem := memory.New(filepath.Join(p.home, "memories"))
	home, _ := os.UserHomeDir()
	skillStore := skill.New(filepath.Join(p.home, "skills"), filepath.Join(home, ".claude", "skills"))
	prompts := prompt.NewBuilder(p.home, mem, skillStore)

	tools := append(files.New(), shell.New(env.NewLocal())...)
	tools = append(tools, memorytool.New(mem)...)
	tools = append(tools, skills.New(skillStore)...)
	tools = append(tools, search.New(st)...)
	tools = append(tools, worktreetool.New(api.NewClient(p.socket))...)
	toolsets := map[string]tool.Toolset{
		"core":    {Tools: []string{"Read", "Write", "Edit", "Glob", "Grep", "Bash"}},
		"learn":   {Tools: []string{"memory", "skill_view", "skill_save"}},
		"project": {Tools: []string{"session_search", "worktree_exit"}, Includes: []string{"core", "learn"}},
		"review":  {Includes: []string{"learn"}},
	}
	reg, err := tool.NewRegistry(tools, toolsets, cfg.Blocked)
	if err != nil {
		return nil, nil, err
	}

	var engines []engine.Engine
	for _, name := range names {
		ec := cfg.GetEngine(name)
		if name == claude.Name {
			engines = append(engines, claude.New(claude.Options{Model: ec.Model, Blocked: cfg.Blocked, Prompts: prompts}))
			continue
		}
		client := codex.New(codex.Options{Model: ec.Model, Effort: ec.Effort, Version: version, GermesDir: p.home, CodexDir: codexDir()})
		engines = append(engines, loop.New(client, reg, prompts, st, loop.Options{
			Name: name, Toolsets: []string{"project"}, ReviewToolsets: []string{"review"}}))
	}
	rt, err := router.New(engines, cfg.Engines, st, time.Now)
	return rt, reg, err
}

func codexDir() string {
	if d := os.Getenv("CODEX_HOME"); d != "" {
		return d
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".codex")
}

// jobCommand runs `germes job ...`.
func jobCommand(ctx context.Context, client *api.Client, args []string) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "ls":
		return cli.ListJobs(ctx, client, os.Stdout)
	case "add":
		fs := flag.NewFlagSet("job add", flag.ContinueOnError)
		every := fs.Duration("every", 0, "time between round starts (0 = back to back)")
		maxRuns := fs.Int("max-runs", 0, "stop after this many rounds (0 = no limit)")
		name := fs.String("name", "", "a name to attach to it by")
		noWorktree := fs.Bool("no-worktree", false, "work in this folder, not in the job's own git worktree")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New(`usage: germes job add [--every 1h] [--max-runs n] [--name x] [--no-worktree] "<prompt>"`)
		}
		dir, err := os.Getwd()
		if err != nil {
			return err
		}
		return cli.CreateJob(ctx, client, api.JobRequest{Name: *name, Dir: dir, Prompt: fs.Arg(0),
			NoWorktree: *noWorktree, Every: *every, MaxRuns: *maxRuns}, os.Stdout)
	}
	if len(args) != 2 {
		return fmt.Errorf("usage: germes job %s <id>", args[0])
	}
	return cli.UpdateJob(ctx, client, args[0], args[1], os.Stdout)
}

// reserveCommand turns an engine's reserve on or off in the config. The
// daemon sees the change and reloads when no turn is running.
func reserveCommand(p paths, args []string) error {
	if len(args) != 2 || (args[0] != "on" && args[0] != "off") {
		return errors.New("usage: germes reserve on|off <engine>")
	}
	cfg, err := config.Load(p.config)
	if err != nil {
		return err
	}
	found := false
	for i := range cfg.Engines {
		if cfg.Engines[i].Name == args[1] {
			cfg.Engines[i].ReserveOff = args[0] == "off"
			found = true
		}
	}
	if !found {
		return fmt.Errorf("no engine %q in %s", args[1], p.config)
	}
	if err := config.Save(p.config, cfg); err != nil {
		return err
	}
	fmt.Printf("reserve %s for %s; the daemon applies it when no turn is running\n", args[0], args[1])
	return nil
}
