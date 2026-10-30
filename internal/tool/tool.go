// Package tool holds the tool types and the registry. It imports nothing
// from Germes; the real tools live in internal/tools/...
package tool

import (
	"context"
	"encoding/json"
	"fmt"
)

// Tool is one thing the model can call.
type Tool struct {
	Name        string
	Toolset     string
	Description string
	Schema      json.RawMessage // JSON Schema of the arguments
	MatchArg    string          // the argument the block list checks ("command" for Bash)
	BlockedAs   string          // another tool whose block rules apply too (send_file: Read's)
	Run         func(ctx context.Context, c Call) (string, error)
}

// Call is one call of a tool.
type Call struct {
	SessionID string
	Dir       string // project folder; relative paths resolve here
	Args      json.RawMessage
}

// Result is what goes back to the model. Errors are results too.
type Result struct {
	Output  string
	IsError bool
}

// Toolset groups tools. Includes are other toolsets, expanded recursively.
type Toolset struct {
	Tools    []string
	Includes []string
}

// NewRun builds a tool's Run from a function that takes typed arguments:
// the JSON arguments are decoded into A first, and bad ones become an error
// the model can read.
func NewRun[A any](fn func(ctx context.Context, c Call, args A) (string, error)) func(context.Context, Call) (string, error) {
	return func(ctx context.Context, c Call) (string, error) {
		var args A
		if err := json.Unmarshal(c.Args, &args); err != nil {
			return "", fmt.Errorf("bad arguments: %w", err)
		}
		return fn(ctx, c, args)
	}
}
