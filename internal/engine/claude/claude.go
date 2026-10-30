// Package claude runs turns through the official claude CLI.
package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/viminizer/germes/internal/stream"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/viminizer/germes/internal/engine"
	"github.com/viminizer/germes/internal/prompt"
)

// Name is this engine's name in config and resume ids.
const Name = "claude-cli"

// idleTimeout kills a claude process that prints nothing for this long.
const idleTimeout = 10 * time.Minute

// Prompts builds the Germes part of the prompt (config knowledge, project,
// skills, memory) that goes into --append-system-prompt.
type Prompts interface {
	Create(ctx context.Context, w prompt.Where, role prompt.Role) (prompt.Prompt, error)
}

// Options configure the engine.
type Options struct {
	Model   string   // "" = the CLI's default
	Blocked []string // passed as --disallowedTools
	Prompts Prompts  // nil = no Germes prompt parts
	// Germes is the germes binary: claude starts `germes mcp --session <id>`
	// as an MCP server, for Germes' own tools. "" = no MCP.
	Germes     string
	GermesHome string // GERMES_HOME: for that server, and a folder Claude may edit
}

// Claude is the claude CLI engine.
type Claude struct {
	opts Options
}

// New creates the engine.
func New(opts Options) *Claude { return &Claude{opts: opts} }

// Name returns the engine name.
func (c *Claude) Name() string { return Name }

// Run starts claude for one turn and streams its events. When claude was
// refused files outside the folders it may use, Run gives it those folders
// and resumes the same conversation once, so it can finish the work (the
// block list still applies).
func (c *Claude) Run(ctx context.Context, t engine.Turn) (<-chan engine.Event, error) {
	extra := t.Extra
	if c.opts.Prompts != nil {
		p, err := c.opts.Prompts.Create(ctx, prompt.Where{Dir: t.Dir, Project: t.Project, Branch: t.Branch}, prompt.NewRole(t.Overseer))
		if err != nil {
			return nil, fmt.Errorf("build prompt: %w", err)
		}
		extra = strings.TrimSpace(p.Extra + "\n\n" + t.Extra)
	}
	if t.Plan {
		t.Prompt = strings.TrimSpace(prompt.PlanMode) + "\n\n" + t.Prompt
	}
	ctx, cancel := context.WithCancel(ctx)
	pr, err := c.start(ctx, t, extra, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	out := make(chan engine.Event, 64)
	go func() {
		defer close(out)
		defer cancel()
		var granted []string
		for {
			done, denied := c.follow(ctx, cancel, pr, out)
			if done == nil {
				return // an error was sent, or the turn was stopped
			}
			more := c.grantable(denied, t.Dir, granted)
			if len(more) == 0 || granted != nil {
				stream.Send(ctx, out, *done)
				return
			}
			slog.Info("claude was refused folders; resuming with them", "session", t.SessionID, "folders", more)
			granted = more
			again := t
			again.ResumeIDs = map[string]string{Name: done.ResumeID}
			again.Prompt = "Germes gave you access to " + strings.Join(more, ", ") +
				". Do what was refused before now, then finish the task."
			if pr, err = c.start(ctx, again, extra, granted); err != nil {
				stream.Send(ctx, out, engine.Event{Kind: engine.KindError, Err: err})
				return
			}
		}
	}()
	return out, nil
}

// proc is one running claude process.
type proc struct {
	cmd    *exec.Cmd
	stdout io.Reader
	stderr *bytes.Buffer
}

// start runs claude for t, with extra folders it may use.
func (c *Claude) start(ctx context.Context, t engine.Turn, extra string, dirs []string) (*proc, error) {
	cmd := exec.CommandContext(ctx, "claude", c.args(t, extra, dirs)...)
	cmd.Dir = t.Dir
	cmd.Stdin = strings.NewReader(t.Prompt)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Kill the whole process group, so tools claude started stop too.
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	pr := &proc{cmd: cmd, stderr: &bytes.Buffer{}}
	cmd.Stderr = pr.stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("claude stdout: %w", err)
	}
	pr.stdout = stdout
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start claude (is it installed?): %w", err)
	}
	return pr, nil
}

// follow streams the process's events until it ends. It holds back the
// Done event and returns it with the paths claude was refused; nil means
// the turn already ended with an error, or was stopped.
func (c *Claude) follow(ctx context.Context, cancel context.CancelFunc, pr *proc, out chan<- engine.Event) (*engine.Event, []string) {
	done, denied, ended := c.stream(ctx, cancel, pr.stdout, out)
	waitErr := pr.cmd.Wait()
	if done != nil || ended || ctx.Err() != nil {
		return done, denied
	}
	msg := strings.TrimSpace(pr.stderr.String())
	if msg == "" && waitErr != nil {
		msg = waitErr.Error()
	}
	stream.Send(ctx, out, engine.Event{Kind: engine.KindError, Err: fmt.Errorf("claude stopped without a result: %s", lastLine(msg))})
	return nil, nil
}

// grantable picks the folders of refused paths that claude does not have
// yet: outside the session's folder, Germes' home and earlier grants.
func (c *Claude) grantable(paths []string, dir string, granted []string) []string {
	var dirs []string
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			continue // relative: inside the session's folder, so refused by a rule
		}
		d := p
		if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
			d = filepath.Dir(p)
		}
		allowed := append([]string{dir}, granted...)
		if c.opts.GermesHome != "" {
			allowed = append(allowed, c.opts.GermesHome)
		}
		if slices.ContainsFunc(append(allowed, dirs...), func(a string) bool { return isInside(d, a) }) {
			continue
		}
		dirs = append(dirs, d)
	}
	return dirs
}

func isInside(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

func (c *Claude) args(t engine.Turn, extra string, dirs []string) []string {
	args := []string{"-p",
		"--output-format", "stream-json", "--verbose", "--include-partial-messages",
		// Yolo without --dangerously-skip-permissions: allow the tools, and
		// refuse anything that would still ask.
		"--permission-mode", "acceptEdits",
		"--allowedTools", "Bash WebFetch WebSearch Skill mcp__" + mcpServer,
		"--permission-prompts", "none",
	}
	if c.opts.Germes != "" {
		args = append(args, "--mcp-config", c.mcpConfig(t.SessionID))
	}
	// Germes' own files (config, memory, skills, workflows) live outside
	// the session's folder; without this, Claude may not read or edit them.
	if c.opts.GermesHome != "" {
		args = append(args, "--add-dir", c.opts.GermesHome)
	}
	for _, d := range dirs {
		args = append(args, "--add-dir", d)
	}
	blocked := c.opts.Blocked
	if t.Plan {
		// A plan turn changes nothing.
		blocked = append(slices.Clone(blocked), "Write", "Edit", "MultiEdit", "NotebookEdit")
	}
	if len(blocked) > 0 {
		args = append(args, "--disallowedTools", strings.Join(blocked, " "))
	}
	if c.opts.Model != "" {
		args = append(args, "--model", c.opts.Model)
	}
	if id := t.ResumeIDs[Name]; id != "" {
		args = append(args, "--resume", id)
	}
	if extra != "" {
		args = append(args, "--append-system-prompt", extra)
	}
	return args
}

// mcpConfig is the --mcp-config JSON that starts Germes' MCP server for
// the session.
func (c *Claude) mcpConfig(sessionID string) string {
	server := map[string]any{"command": c.opts.Germes, "args": []string{"mcp", "--session", sessionID}}
	if c.opts.GermesHome != "" {
		server["env"] = map[string]string{"GERMES_HOME": c.opts.GermesHome}
	}
	b, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{mcpServer: server}})
	return string(b)
}

// stream reads claude's JSON lines until EOF. It reports whether a Done or
// Error event was sent.
func (c *Claude) stream(ctx context.Context, cancel context.CancelFunc, r io.Reader, out chan<- engine.Event) (done *engine.Event, denied []string, ended bool) {
	idle := time.AfterFunc(idleTimeout, func() {
		slog.Warn("claude idle timeout, killing it")
		cancel()
	})
	defer idle.Stop()

	var p parser
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		idle.Reset(idleTimeout)
		for _, ev := range p.parse(sc.Bytes()) {
			switch ev.Kind {
			case engine.KindDone:
				done = &ev
				continue // the caller sends it, or resumes first
			case engine.KindError:
				ended = true
			}
			stream.Send(ctx, out, ev)
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, context.Canceled) && ctx.Err() == nil {
		stream.Send(ctx, out, engine.Event{Kind: engine.KindError, Err: fmt.Errorf("read claude output: %w", err)})
		return nil, nil, true
	}
	return done, p.denied, ended
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}
