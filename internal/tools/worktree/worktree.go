// Package worktree has worktree_exit: move the session from its own git
// worktree back to the project folder.
package worktree

import (
	"context"
	"encoding/json"

	"github.com/viminizer/germes/internal/api"
	"github.com/viminizer/germes/internal/tool"
)

// Updater changes sessions (the daemon, reached through api.Client).
type Updater interface {
	UpdateSession(ctx context.Context, u api.SessionUpdate) error
}

// New returns the worktree_exit tool.
func New(svc Updater) []tool.Tool {
	return []tool.Tool{{
		Name: "worktree_exit", Toolset: "worktree",
		Description: "Leave this session's own git worktree: the session moves to the project's main checkout from the next turn, " +
			"and the worktree and its branch are removed. Refused while the worktree has uncommitted changes or commits not merged into the project. " +
			"Use it after merging, or when the user wants to work in the main checkout.",
		Schema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		Run: tool.NewRun(func(ctx context.Context, c tool.Call, _ struct{}) (string, error) {
			if err := svc.UpdateSession(ctx, api.SessionUpdate{ID: c.SessionID, LeaveWorktree: true}); err != nil {
				return "", err
			}
			return "left the worktree; from the next turn this session works in the project folder", nil
		}),
	}}
}
