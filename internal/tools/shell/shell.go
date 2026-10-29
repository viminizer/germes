// Package shell has the Bash tool. Commands run through env.Environment.
package shell

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/viminizer/germes/internal/env"
	"github.com/viminizer/germes/internal/tool"
)

const (
	defaultTimeout = 2 * time.Minute
	maxTimeout     = 10 * time.Minute
)

// New returns the Bash tool, running in e.
func New(e env.Environment) []tool.Tool {
	return []tool.Tool{{
		Name: "Bash", Toolset: "shell", MatchArg: "command",
		Description: "Run a bash command in the project folder. Each call is a fresh shell: cd and export do not carry over. Default timeout 2 minutes, max 10.",
		Schema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"},"timeout_seconds":{"type":"integer"}},` +
			`"required":["command"],"additionalProperties":false}`),
		Run: func(ctx context.Context, c tool.Call) (string, error) {
			var a struct {
				Command        string `json:"command"`
				TimeoutSeconds int    `json:"timeout_seconds"`
			}
			if err := json.Unmarshal(c.Args, &a); err != nil {
				return "", fmt.Errorf("bad arguments: %w", err)
			}
			if strings.TrimSpace(a.Command) == "" {
				return "", fmt.Errorf("empty command")
			}
			timeout := defaultTimeout
			if a.TimeoutSeconds > 0 {
				timeout = min(time.Duration(a.TimeoutSeconds)*time.Second, maxTimeout)
			}
			res, err := e.Run(ctx, a.Command, c.Dir, timeout)
			if err != nil {
				return "", err
			}
			out := strings.TrimRight(res.Output, "\n")
			switch {
			case res.TimedOut:
				out += fmt.Sprintf("\n[timed out after %s]", timeout)
			case res.ExitCode != 0:
				out += fmt.Sprintf("\n[exit code %d]", res.ExitCode)
			}
			if strings.TrimSpace(out) == "" {
				out = "(no output)"
			}
			return out, nil
		},
	}}
}
