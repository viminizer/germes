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
	"os/exec"
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
	GermesHome string // GERMES_HOME for that server
}

// Claude is the claude CLI engine.
type Claude struct {
	opts Options
}

// New creates the engine.
func New(opts Options) *Claude { return &Claude{opts: opts} }

// Name returns the engine name.
func (c *Claude) Name() string { return Name }

// Run starts claude for one turn and streams its events.
func (c *Claude) Run(ctx context.Context, t engine.Turn) (<-chan engine.Event, error) {
	extra := t.Extra
	if c.opts.Prompts != nil {
		p, err := c.opts.Prompts.Create(ctx, prompt.Where{Dir: t.Dir, Project: t.Project, Branch: t.Branch}, prompt.NewRole(t.Overseer))
		if err != nil {
			return nil, fmt.Errorf("build prompt: %w", err)
		}
		extra = strings.TrimSpace(p.Extra + "\n\n" + t.Extra)
	}
	ctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(ctx, "claude", c.args(t, extra)...)
	cmd.Dir = t.Dir
	cmd.Stdin = strings.NewReader(t.Prompt)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Kill the whole process group, so tools claude started stop too.
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("claude stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start claude (is it installed?): %w", err)
	}

	out := make(chan engine.Event, 64)
	go func() {
		defer close(out)
		defer cancel()
		ended := c.stream(ctx, cancel, stdout, out)
		waitErr := cmd.Wait()
		if ended || ctx.Err() != nil {
			return
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" && waitErr != nil {
			msg = waitErr.Error()
		}
		stream.Send(ctx, out, engine.Event{Kind: engine.KindError, Err: fmt.Errorf("claude stopped without a result: %s", lastLine(msg))})
	}()
	return out, nil
}

func (c *Claude) args(t engine.Turn, extra string) []string {
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
	if len(c.opts.Blocked) > 0 {
		args = append(args, "--disallowedTools", strings.Join(c.opts.Blocked, " "))
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
func (c *Claude) stream(ctx context.Context, cancel context.CancelFunc, r io.Reader, out chan<- engine.Event) bool {
	idle := time.AfterFunc(idleTimeout, func() {
		slog.Warn("claude idle timeout, killing it")
		cancel()
	})
	defer idle.Stop()

	var p parser
	ended := false
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		idle.Reset(idleTimeout)
		for _, ev := range p.parse(sc.Bytes()) {
			if ev.Kind == engine.KindDone || ev.Kind == engine.KindError {
				ended = true
			}
			stream.Send(ctx, out, ev)
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, context.Canceled) && ctx.Err() == nil {
		stream.Send(ctx, out, engine.Event{Kind: engine.KindError, Err: fmt.Errorf("read claude output: %w", err)})
		return true
	}
	return ended
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}
