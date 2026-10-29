// Package worktree has worktree_enter and worktree_exit: move the session
// into its own git worktree, and back to the project folder.
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

// New returns the worktree tools.
func New(svc Updater) []tool.Tool {
	return []tool.Tool{
		newTool(svc, "worktree_enter", "Move this session into its own new git worktree and branch (from the project's last commit): "+
			"from the next turn it works there, apart from the main checkout. Use it when the user asks for a worktree or to work apart from other sessions. "+
			"Uncommitted changes in the main checkout do not come along.",
			api.SessionUpdate{EnterWorktree: true}, "made a worktree; from the next turn this session works there"),
		newTool(svc, "worktree_exit", "Leave this session's own git worktree: the session moves to the project's main checkout from the next turn, "+
			"and the worktree and its branch are removed. Refused while the worktree has uncommitted changes or commits not merged into the project. "+
			"Use it after merging, or when the user wants to work in the main checkout.",
			api.SessionUpdate{LeaveWorktree: true}, "left the worktree; from the next turn this session works in the project folder"),
	}
}

// newTool is a tool without arguments that applies u to the calling session.
func newTool(svc Updater, name, description string, u api.SessionUpdate, done string) tool.Tool {
	return tool.Tool{
		Name: name, Toolset: "worktree", Description: description,
		Schema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		Run: tool.NewRun(func(ctx context.Context, c tool.Call, _ struct{}) (string, error) {
			upd := u // a copy per call: calls run at the same time
			upd.ID = c.SessionID
			if err := svc.UpdateSession(ctx, upd); err != nil {
				return "", err
			}
			return done, nil
		}),
	}
}
