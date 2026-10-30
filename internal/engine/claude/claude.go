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
	"github.com/viminizer/germes/internal/text"
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
	"github.com/viminizer/germes/internal/file"
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
	// Reviews runs the background review that learns from a session; nil
	// (or no GermesHome) = no review.
	Reviews *engine.Reviews
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
	pr, err := c.start(ctx, t, c.args(t, extra, nil))
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
			done, denied := c.follow(ctx, cancel, t.SessionID, pr, out)
			if done == nil {
				return // an error was sent, or the turn was stopped
			}
			more := c.grantable(denied, t.Dir, granted)
			if len(more) == 0 || granted != nil {
				if c.reviews() && c.opts.Reviews.IsDue(t.SessionID) && done.ResumeID != "" {
					resumeID := done.ResumeID
					c.opts.Reviews.Run(t.SessionID, func(ctx context.Context) { c.review(ctx, t, resumeID, extra) })
				}
				stream.Send(ctx, out, *done)
				return
			}
			slog.Info("claude was refused folders; resuming with them", "session", t.SessionID, "folders", more)
			granted = more
			again := t
			again.ResumeIDs = map[string]string{Name: done.ResumeID}
			again.Prompt = "Germes gave you access to " + strings.Join(more, ", ") +
				". Do what was refused before now, then finish the task."
			if pr, err = c.start(ctx, again, c.args(again, extra, granted)); err != nil {
				stream.Send(ctx, out, engine.Event{Kind: engine.KindError, Err: err})
				return
			}
		}
	}()
	return out, nil
}

// titleModel is the model claude names sessions with: a title is a few
// words, not worth the session's model.
const titleModel = "haiku"

// CreateTitle names a session with a one-off claude call: no tools, no MCP
// servers, no saved conversation, and no project files (engine.Titler).
func (c *Claude) CreateTitle(ctx context.Context, message string) (string, error) {
	pr, err := c.start(ctx, engine.Turn{Dir: os.TempDir(), Prompt: message}, []string{"-p", "--model", titleModel,
		"--tools", "", "--strict-mcp-config", "--no-session-persistence", "--output-format", "text",
		"--system-prompt", strings.TrimSpace(prompt.Title)})
	if err != nil {
		return "", err
	}
	out, err := io.ReadAll(pr.stdout)
	if werr := pr.cmd.Wait(); werr != nil {
		return "", fmt.Errorf("claude title: %w: %s", werr, lastLine(pr.stderr.String()))
	}
	return string(out), err
}

// proc is one running claude process.
type proc struct {
	cmd    *exec.Cmd
	stdout io.Reader
	stderr *bytes.Buffer
}

// start runs claude with args for t.
func (c *Claude) start(ctx context.Context, t engine.Turn, args []string) (*proc, error) {
	cmd := exec.CommandContext(ctx, "claude", args...)
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
func (c *Claude) follow(ctx context.Context, cancel context.CancelFunc, sessionID string, pr *proc, out chan<- engine.Event) (*engine.Event, []string) {
	done, denied, ended := c.stream(ctx, cancel, sessionID, pr.stdout, out)
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
		if slices.ContainsFunc(append(allowed, dirs...), func(a string) bool { return file.IsInside(d, a) }) {
			continue
		}
		dirs = append(dirs, d)
	}
	return dirs
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
		args = append(args, "--mcp-config", c.mcpConfig(t.SessionID), "--settings", c.hookSettings(t.SessionID))
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
	if id := t.ResumeIDs[Name]; id != "" {
		args = append(args, "--resume", id)
	}
	return append(args, c.sharedArgs(extra)...)
}

// sharedArgs are in every claude run: Germes' home folder, the model and
// the Germes prompt.
func (c *Claude) sharedArgs(extra string) []string {
	var args []string
	// Germes' own files (config, memory, skills, workflows) live outside
	// the session's folder; without this, Claude may not read or edit them.
	if c.opts.GermesHome != "" {
		args = append(args, "--add-dir", c.opts.GermesHome)
	}
	if c.opts.Model != "" {
		args = append(args, "--model", c.opts.Model)
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

// hookTimeout is how long claude waits for `germes hook check`: a hook's
// run: command may take up to 10 minutes (make check).
const hookTimeout = 11 * 60

// hookSettings is the --settings JSON that sends every tool call claude
// makes through Germes' own check, `germes hook check` (issue #42): the
// same block list and hooks as the Germes loop, decided by one registry.
func (c *Claude) hookSettings(sessionID string) string {
	cmd := text.QuoteShell(c.opts.Germes) + " hook check --session " + text.QuoteShell(sessionID)
	if c.opts.GermesHome != "" {
		cmd = "GERMES_HOME=" + text.QuoteShell(c.opts.GermesHome) + " " + cmd
	}
	hook := map[string]any{"type": "command", "command": cmd, "timeout": hookTimeout}
	b, _ := json.Marshal(map[string]any{"hooks": map[string]any{
		"PreToolUse": []any{map[string]any{"matcher": "*", "hooks": []any{hook}}},
	}})
	return string(b)
}

// stream reads claude's JSON lines until EOF. It reports whether a Done or
// Error event was sent.
func (c *Claude) stream(ctx context.Context, cancel context.CancelFunc, sessionID string, r io.Reader, out chan<- engine.Event) (done *engine.Event, denied []string, ended bool) {
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
			case engine.KindToolCall:
				if c.reviews() {
					c.opts.Reviews.Update(sessionID, c.savesLearning(ev.Tool))
				}
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

// reviews reports whether this engine learns from its sessions.
func (c *Claude) reviews() bool { return c.opts.Reviews != nil && c.opts.GermesHome != "" }

// savesLearning reports whether a tool call saved memory or a skill: claude
// edits them as files.
func (c *Claude) savesLearning(tc *engine.ToolCall) bool {
	var a struct {
		FilePath string `json:"file_path"`
	}
	if c.opts.GermesHome == "" || json.Unmarshal(tc.Args, &a) != nil || a.FilePath == "" {
		return false
	}
	return file.IsInside(a.FilePath, filepath.Join(c.opts.GermesHome, "memories")) ||
		file.IsInside(a.FilePath, filepath.Join(c.opts.GermesHome, "skills"))
}

// review learns from a session in the background (Hermes' learning loop):
// claude resumes a fork of the conversation, so the session itself does not
// change, and may only read files and edit memory and skills.
func (c *Claude) review(ctx context.Context, t engine.Turn, resumeID, extra string) {
	t.Prompt = strings.TrimSpace(prompt.Review)
	pr, err := c.start(ctx, t, c.reviewArgs(resumeID, extra))
	if err == nil {
		_, err = io.Copy(io.Discard, pr.stdout)
		if werr := pr.cmd.Wait(); werr != nil {
			err = fmt.Errorf("%w: %s", werr, lastLine(pr.stderr.String()))
		}
	}
	if err != nil {
		slog.Warn("review", "session", t.SessionID, "err", err)
		return
	}
	slog.Info("review done", "session", t.SessionID)
}

// reviewArgs runs the review: without acceptEdits, an edit outside the
// allowed folders would ask, and is refused.
func (c *Claude) reviewArgs(resumeID, extra string) []string {
	home := "/" + c.opts.GermesHome // "//abs/path" is an absolute path in a rule
	args := []string{"-p", "--output-format", "json",
		"--resume", resumeID, "--fork-session",
		"--allowedTools", "Read Glob Grep Edit(" + home + "/memories/**) Edit(" + home + "/skills/**)",
		"--disallowedTools", strings.Join(append(slices.Clone(c.opts.Blocked), "Bash", "WebFetch", "WebSearch", "NotebookEdit"), " "),
		"--permission-prompts", "none",
	}
	return append(args, c.sharedArgs(extra)...)
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}
