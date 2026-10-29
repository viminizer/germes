// Package tui is the terminal UI. It imports only api from Germes. Each UI
// element is its own component with a View(width) that fits any width.
package tui

import (
	"context"
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"

	"github.com/viminizer/germes/internal/api"
)

// Options configure the TUI.
type Options struct {
	Dir          string // where new sessions work
	Project      string // shown in the banner and status bar
	SessionID    string // "" = start a new session on the first message
	JobID        string // attached to a job: follow its new rounds
	First        string // a message sent as soon as the TUI opens
	Worktree     bool   // a new session gets its own git worktree
	Version      string
	Engine       string
	EnsureDaemon func(context.Context) error // starts the daemon if it is gone
}

// TUI is the terminal UI.
type TUI struct {
	svc  api.Service
	opts Options
}

// New creates the TUI.
func New(svc api.Service, opts Options) *TUI { return &TUI{svc: svc, opts: opts} }

// Run shows the TUI until the user quits. The session keeps running.
// /clear ends one program and starts a fresh one on a clean screen: the
// inline renderer cannot wipe what it already printed into the scrollback.
func (t *TUI) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	opts, notice := t.opts, ""
	for {
		m := newModel(ctx, t.svc, opts)
		m.startNotice = notice
		final, err := tea.NewProgram(m, tea.WithContext(ctx)).Run()
		if err != nil {
			return err
		}
		done, ok := final.(*model)
		if !ok || !done.restart {
			return nil
		}
		// Home, clear the screen, clear the scrollback.
		fmt.Fprint(os.Stdout, "\x1b[H\x1b[2J\x1b[3J")
		opts.SessionID, opts.JobID, opts.First, notice = "", "", "", done.restartNotice
	}
}
