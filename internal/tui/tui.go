// Package tui is the terminal UI. It imports only api from Germes. Each UI
// element is its own component with a View(width) that fits any width.
package tui

import (
	"context"

	tea "charm.land/bubbletea/v2"

	"github.com/viminizer/germes/internal/api"
)

// Options configure the TUI.
type Options struct {
	Dir          string // where new sessions work
	Project      string // shown in the banner and status bar
	SessionID    string // "" = start a new session on the first message
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
func (t *TUI) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	_, err := tea.NewProgram(newModel(ctx, t.svc, t.opts), tea.WithContext(ctx)).Run()
	return err
}
