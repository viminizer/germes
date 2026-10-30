// Command germes is the Germes harness: the TUI, the daemon and the small
// commands. This file is the only place that wires modules together.
package main

import (
	"bufio"
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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/viminizer/germes/internal/api"
	"github.com/viminizer/germes/internal/checkpoint"
	"github.com/viminizer/germes/internal/cli"
	"github.com/viminizer/germes/internal/config"
	"github.com/viminizer/germes/internal/daemon"
	"github.com/viminizer/germes/internal/engine"
	"github.com/viminizer/germes/internal/engine/claude"
	"github.com/viminizer/germes/internal/engine/loop"
	"github.com/viminizer/germes/internal/env"
	"github.com/viminizer/germes/internal/file"
	"github.com/viminizer/germes/internal/job"
	"github.com/viminizer/germes/internal/llm/codex"
	"github.com/viminizer/germes/internal/mcp"
	"github.com/viminizer/germes/internal/memory"
	"github.com/viminizer/germes/internal/prompt"
	"github.com/viminizer/germes/internal/refs"
	"github.com/viminizer/germes/internal/router"
	"github.com/viminizer/germes/internal/session"
	"github.com/viminizer/germes/internal/skill"
	"github.com/viminizer/germes/internal/store"
	"github.com/viminizer/germes/internal/telegram"
	"github.com/viminizer/germes/internal/tool"
	"github.com/viminizer/germes/internal/tools/files"
	jobstool "github.com/viminizer/germes/internal/tools/jobs"
	memorytool "github.com/viminizer/germes/internal/tools/memory"
	"github.com/viminizer/germes/internal/tools/search"
	sessionstool "github.com/viminizer/germes/internal/tools/sessions"
	"github.com/viminizer/germes/internal/tools/shell"
	"github.com/viminizer/germes/internal/tools/skills"
	"github.com/viminizer/germes/internal/tools/todo"
	"github.com/viminizer/germes/internal/tools/web"
	worktreetool "github.com/viminizer/germes/internal/tools/worktree"
	"github.com/viminizer/germes/internal/tui"
)

// version is set at build time from git (see Makefile).
var version = "dev"

const usage = `usage:
  germes                start a new session in this folder
  germes -c             continue the last session in this folder
  germes --worktree     start a session in its own git worktree (default: in this folder)
  germes attach <id>    open any session, or a job's latest round (follows new rounds)
  germes setup telegram talk to Germes from a Telegram group (topics bound to projects)
  germes overseer       open the overseer: one session that controls Germes (sessions, jobs) for you
  germes mcp --session <id>  MCP server with Germes' tools (claude starts it on its own)
  germes ls             list sessions
  germes insights [--days 30]  how Germes was used: sessions, turns, tools, skills
  germes stop <id>      stop a session's current step
  germes job add [--every 1h] [--max-runs n] [--name x] [--no-worktree] [--repeat] "<prompt>"
                        run a prompt again and again in this folder, a new session each round (--repeat: until you stop it)
  germes job add --workflow <name> [--every ...] ["<goal>"]
                        run a workflow round after round (steps in ~/.germes/workflows/<name>.yaml)
  germes job ls|start|stop|rm <id>  list, start, stop or delete jobs
  germes workflow ls|check <name>|new  list, check or write workflows
  germes reserve on|off <engine>  keep or drop an engine's usage reserve
  germes doctor         check config, engines, database and daemon, and say how to fix them
  germes daemon stop    stop the daemon
  germes daemon kill    stop a hung daemon (SIGTERM, then SIGKILL)
  germes daemon restart restart the daemon, cutting running turns (updates hand over on their own without cutting)`

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
	case "insights":
		fs := flag.NewFlagSet("insights", flag.ContinueOnError)
		days := fs.Int("days", 30, "how many days back")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if err := ensureDaemon(ctx, p, client); err != nil {
			return err
		}
		return cli.GetInsights(ctx, client, time.Now().AddDate(0, 0, -*days), os.Stdout)
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
		return runTUI(ctx, p, client, tuiStart{sessionID: args[1]})
	case "setup":
		if len(args) != 2 || args[1] != "telegram" {
			return errors.New("usage: germes setup telegram")
		}
		return setupTelegram(p)
	case "overseer":
		return runTUI(ctx, p, client, tuiStart{sessionID: api.OverseerID})
	case "mcp":
		fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
		sessionID := fs.String("session", "", "the session whose tools to serve")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *sessionID == "" {
			return errors.New("usage: germes mcp --session <id>")
		}
		if err := ensureDaemon(ctx, p, client); err != nil {
			return err
		}
		return mcp.Run(ctx, client, *sessionID, version)
	case "job":
		if err := ensureDaemon(ctx, p, client); err != nil {
			return err
		}
		return jobCommand(ctx, client, args[1:])
	case "workflow":
		return workflowCommand(ctx, p, client, args[1:])
	case "reserve":
		return reserveCommand(p, args[1:])
	case "doctor":
		return doctor(ctx, p, client)
	case "help", "-h", "--help":
		fmt.Println(usage)
		return nil
	}

	fs := flag.NewFlagSet("germes", flag.ContinueOnError)
	cont := fs.Bool("c", false, "continue the last session in this folder")
	wt := fs.Bool("worktree", false, "work in the session's own git worktree, not in this folder")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, usage) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unknown command %q\n%s", fs.Arg(0), usage)
	}
	return runTUI(ctx, p, client, tuiStart{cont: *cont, worktree: *wt})
}

// tuiStart says how the TUI opens.
type tuiStart struct {
	sessionID string // a session or job to open; "" = a new session
	cont      bool   // open the last session in this folder
	worktree  bool   // a new session gets its own git worktree
	first     string // a message to send right away
}

func runTUI(ctx context.Context, p paths, client *api.Client, start tuiStart) error {
	sessionID := start.sessionID
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
	if start.cont {
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
		if api.IsNotFound(err) {
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
		Dir: dir, Project: project, SessionID: sessionID, JobID: jobID, Version: version, Engine: engineName, Worktree: start.worktree, First: start.first,
		EnsureDaemon: func(ctx context.Context) error { return ensureDaemon(ctx, p, client) },
	})
	return t.Run(ctx)
}

func daemonCommand(ctx context.Context, p paths, client *api.Client, sub string) error {
	switch sub {
	case "stop":
		return cli.StopDaemon(ctx, client, os.Stdout)
	case "kill":
		return killDaemon(p)
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

	// Background reviews end with the daemon, before the store closes.
	ctx, cancel := context.WithCancel(context.Background())
	reviews := engine.NewReviews(ctx)
	defer reviews.Wait()
	defer cancel()
	rt, reg, err := newRouter(p, cfg, st, reviews)
	if err != nil {
		return err
	}
	old := daemon.NewOldDaemons(filepath.Join(p.home, "old"))
	sessions := session.NewManager(st, rt, session.Options{Worktrees: filepath.Join(p.home, "worktrees"), Refs: refs.New(reg), Elsewhere: old,
		Checkpoints: filepath.Join(p.home, "checkpoints")})
	tools := registryTools{reg: reg, sessions: sessions}
	jobs := job.NewScheduler(st, sessions, job.Options{Tools: tools, Workflows: filepath.Join(p.home, "workflows")})
	return daemon.New(sessions, jobs, old, daemon.Options{
		Version: version, ConfigPath: p.config, SocketPath: p.socket, LockPath: p.lock, OldDir: filepath.Join(p.home, "old"),
		StartNew: func() error { return startDaemon(p) }, Tools: tools, Gateway: newTelegram(p, cfg),
		Cleanup: func(ctx context.Context) {
			before := time.Now().Add(-cfg.GetCleanupAge())
			outputs, err := reg.PruneOutputs(before)
			if err != nil {
				slog.Warn("clean up tool outputs", "err", err)
			}
			snapshots, err := checkpoint.Prune(filepath.Join(p.home, "checkpoints"), before)
			if err != nil {
				slog.Warn("clean up snapshots", "err", err)
			}
			tgFiles, err := file.DeleteOld(telegramFiles(p), before)
			if err != nil {
				slog.Warn("clean up telegram files", "err", err)
			}
			slog.Info("cleanup", "tool_outputs", outputs, "snapshot_repos", snapshots, "telegram_files", tgFiles)
		},
		Insights: st.GetInsights, RunContractMigrations: st.RunContractMigrations,
		Jobs: []api.JobRequest{{Name: "curator", Dir: p.home, Prompt: strings.TrimSpace(prompt.Curator), NoWorktree: true, Repeat: true, Every: 7 * 24 * time.Hour}},
	}).Run(ctx)
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

	if waitUntil(5*time.Second, func() bool { _, err := ping(ctx, client); return err == nil }) {
		return nil
	}
	if pid, err := daemon.GetLockHolder(p.lock); err == nil && pid != 0 {
		return fmt.Errorf("the daemon (pid %d) runs but does not answer; run `germes doctor`", pid)
	}
	return fmt.Errorf("the daemon did not start; run `germes doctor`, or see %s", p.log)
}

// newTelegram builds the Telegram bot when it is set up. It talks to the
// daemon through the socket, like any client. nil = off.
func newTelegram(p paths, cfg config.Config) daemon.Gateway {
	if !cfg.Telegram.IsOn() {
		return nil
	}
	codeDirs := cfg.CodeDirs
	if len(codeDirs) == 0 {
		home, _ := os.UserHomeDir()
		codeDirs = []string{filepath.Join(home, "dev")}
	}
	return telegram.New(api.NewClient(p.socket), telegram.Options{Token: cfg.Telegram.Token, ChatID: cfg.Telegram.ChatID,
		UserID: cfg.Telegram.UserID, CodeDirs: codeDirs, StatePath: filepath.Join(p.home, "telegram.json"),
		FilesDir: telegramFiles(p), APIBase: cfg.Telegram.APIBase})
}

// telegramFiles is where files sent to the bot are saved, until cleanup.
func telegramFiles(p paths) string { return filepath.Join(p.home, "telegram", "files") }

// setupTelegram asks for the bot's token, the group chat and the user, and
// saves them in the config. The daemon picks the change up on its own.
func setupTelegram(p paths) error {
	cfg, err := config.Load(p.config)
	if err != nil {
		return err
	}
	fmt.Println(`Telegram setup. You need:
  1. A bot from @BotFather (/newbot gives the token). Turn its privacy mode off (/setprivacy -> Disable), or make it a group admin.
  2. A group with topics turned on, with the bot added. Its chat id starts with -100 (forward a message to @RawDataBot to see it).
  3. Your own user id (the same bot shows it).`)
	in := bufio.NewScanner(os.Stdin)
	ask := func(q string) string {
		fmt.Print(q)
		in.Scan()
		return strings.TrimSpace(in.Text())
	}
	token := ask("Bot token: ")
	chatID, err1 := strconv.ParseInt(ask("Group chat id: "), 10, 64)
	userID, err2 := strconv.ParseInt(ask("Your user id: "), 10, 64)
	if token == "" || err1 != nil || err2 != nil {
		return errors.New("setup cancelled: the token and both ids are needed (ids are numbers)")
	}
	cfg.Telegram = config.Telegram{Token: token, ChatID: chatID, UserID: userID}
	if err := config.Save(p.config, cfg); err != nil {
		return err
	}
	fmt.Println("Saved. The daemon starts the bot within a few seconds. In a topic, /bind <project> binds it; General talks to the overseer.")
	return nil
}

// registryTools runs tools for the daemon and jobs, with the block list,
// in the session's folder unless the request names one.
type registryTools struct {
	reg      *tool.Registry
	sessions *session.Manager
}

func (t registryTools) ListTools(toolset string) []api.ToolInfo {
	var infos []api.ToolInfo
	for _, tl := range t.reg.List(toolset) {
		infos = append(infos, api.ToolInfo{Name: tl.Name, Description: tl.Description, Schema: tl.Schema})
	}
	return infos
}

func (t registryTools) RunTool(ctx context.Context, r api.ToolRequest) (api.ToolResult, error) {
	if r.Name == "" {
		return api.ToolResult{}, api.NewError(api.CodeInvalid, "no tool name")
	}
	if r.Dir == "" {
		if r.SessionID == "" {
			return api.ToolResult{}, api.NewError(api.CodeInvalid, "no session and no folder")
		}
		s, err := t.sessions.GetSession(ctx, r.SessionID)
		if err != nil {
			return api.ToolResult{}, err
		}
		r.Dir = s.Dir
	}
	res := t.reg.Run(ctx, r.Name, tool.Call{SessionID: r.SessionID, Dir: r.Dir, Args: r.Args})
	return api.ToolResult{Output: res.Output, IsError: res.IsError}, nil
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
	if waitUntil(15*time.Second, func() bool { _, err := ping(ctx, client); return err != nil }) {
		return nil
	}
	return errors.New("the old daemon did not stop")
}

// waitUntil checks ok every few milliseconds until it holds (true) or the
// time is up (false).
func waitUntil(d time.Duration, ok func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if ok() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return ok()
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
func newRouter(p paths, cfg config.Config, st *store.Store, reviews *engine.Reviews) (*router.Router, *tool.Registry, error) {
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
	todos := todo.NewLists(filepath.Join(p.home, "todos"))
	tools = append(tools, todo.New(todos)...)
	tools = append(tools, search.New(st)...)
	tools = append(tools, web.New()...)
	// These reach the daemon through its socket, like any client.
	daemonClient := api.NewClient(p.socket)
	tools = append(tools, worktreetool.New(daemonClient)...)
	tools = append(tools, sessionstool.New(daemonClient)...)
	tools = append(tools, jobstool.New(daemonClient)...)
	control := []string{"session_list", "session_read", "session_send", "session_stop", "session_rename", "session_delete",
		"job_create", "job_list", "job_start", "job_stop", "job_delete"}
	toolsets := map[string]tool.Toolset{
		"core":    {Tools: []string{"Read", "Write", "Edit", "Glob", "Grep", "Bash"}},
		"learn":   {Tools: []string{"memory", "skill_view", "skill_save", "skill_patch"}},
		"web":     {Tools: []string{"WebSearch", "WebFetch"}}, // not in the MCP toolsets: Claude has its own
		"project": {Tools: []string{"session_search", "worktree_enter", "worktree_exit", "todo_list", api.ToolSendFile}, Includes: []string{"core", "learn", "web"}},
		"review":  {Includes: []string{"learn"}},
		// A plan turn reads and searches; it changes nothing.
		"plan": {Tools: []string{"Read", "Glob", "Grep", "session_search"}, Includes: []string{"web"}},
		// Subagents work on one task: files, shell, search and the web; no memory.
		"subagent": {Tools: []string{"session_search"}, Includes: []string{"core", "web"}},
		// The overseer controls Germes too; it has no worktree to leave.
		"overseer": {Tools: append([]string{"session_search", "todo_list", api.ToolSendFile}, control...), Includes: []string{"core", "learn", "web"}},
		// Claude gets only what it lacks through MCP: it has its own files and
		// shell, and edits memory and skills as files.
		daemon.ToolsetMCP:         {Tools: []string{"session_search", "worktree_enter", "worktree_exit", api.ToolSendFile}},
		daemon.ToolsetMCPOverseer: {Tools: append([]string{"session_search", api.ToolSendFile}, control...)},
	}
	reg, err := tool.NewRegistry(tools, toolsets, cfg.Blocked, filepath.Join(p.home, "tool-output"))
	if err != nil {
		return nil, nil, err
	}

	var engines []engine.Engine
	for _, name := range names {
		ec := cfg.GetEngine(name)
		if name == claude.Name {
			exe, err := os.Executable()
			if err != nil {
				return nil, nil, err
			}
			engines = append(engines, claude.New(claude.Options{Model: ec.Model, Blocked: cfg.Blocked, Prompts: prompts,
				Germes: exe, GermesHome: p.home, Reviews: reviews}))
			continue
		}
		client := codex.New(codex.Options{Model: ec.Model, Effort: ec.Effort, Version: version, GermesDir: p.home, CodexDir: codexDir()})
		engines = append(engines, loop.New(client, reg, prompts, st, loop.Options{
			Name: name, Toolsets: []string{"project"}, OverseerToolsets: []string{"overseer"}, ReviewToolsets: []string{"review"}, Reviews: reviews, SubagentToolsets: []string{"subagent"}, PlanToolsets: []string{"plan"}, Todos: todos}))
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
		workflow := fs.String("workflow", "", "follow this workflow each round")
		noWorktree := fs.Bool("no-worktree", false, "work in this folder, not in the job's own git worktree")
		repeat := fs.Bool("repeat", false, "run until stopped: [DONE] or workflow done only means nothing to do now (waits, then tries again)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() > 1 || (fs.NArg() == 0 && *workflow == "") {
			return errors.New(`usage: germes job add [--every 1h] [--max-runs n] [--name x] [--no-worktree] [--repeat] [--workflow w] "<prompt>"`)
		}
		dir, err := os.Getwd()
		if err != nil {
			return err
		}
		return cli.CreateJob(ctx, client, api.JobRequest{Name: *name, Dir: dir, Prompt: fs.Arg(0), Workflow: *workflow,
			NoWorktree: *noWorktree, Repeat: *repeat, Every: *every, MaxRuns: *maxRuns}, os.Stdout)
	}
	if len(args) != 2 {
		return fmt.Errorf("usage: germes job %s <id>", args[0])
	}
	return cli.UpdateJob(ctx, client, args[0], args[1], os.Stdout)
}

// workflowCommand runs `germes workflow ...`. ls and check read the files
// here; new opens a session that writes one with the user.
func workflowCommand(ctx context.Context, p paths, client *api.Client, args []string) error {
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	userDir := filepath.Join(p.home, "workflows")
	dirs := job.ListWorkflowDirs(userDir, session.GetProjectRoot(dir))
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch {
	case sub == "ls":
		names := job.ListWorkflows(dirs)
		if len(names) == 0 {
			fmt.Printf("no workflows in %s; write one with `germes workflow new`\n", strings.Join(dirs, " or "))
		}
		for _, n := range names {
			fmt.Println(n)
		}
		return nil
	case sub == "check" && len(args) == 2:
		if _, err := job.LoadWorkflow(dirs, args[1]); err != nil {
			return fmt.Errorf("workflow %s: %w", args[1], err)
		}
		fmt.Printf("workflow %s is fine\n", args[1])
		return nil
	case sub == "new":
		if err := os.MkdirAll(userDir, 0o700); err != nil {
			return err
		}
		return runTUI(ctx, p, client, tuiStart{first: fmt.Sprintf(
			"I want a new Germes workflow. Ask me what it should do, then write it to %s/<name>.yaml (the format is in your Germes notes), show it to me, and save it only after I agree. Check it with `germes workflow check <name>`.", userDir)})
	}
	return errors.New("usage: germes workflow ls|check <name>|new")
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
