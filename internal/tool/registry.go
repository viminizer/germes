package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/viminizer/germes/internal/file"
)

const (
	defaultTimeout = 2 * time.Minute
	maxOutput      = 50_000 // characters sent back to the model
)

// Registry is every tool, by name. It is the one place that checks names,
// the block list, timeouts, errors and output size.
type Registry struct {
	tools    map[string]Tool
	toolsets map[string]Toolset
	blocked  []rule
	spillDir string // long outputs are saved here in full; "" = cut them
}

// NewRegistry checks and stores the tools, toolsets and block list. Outputs
// too long for the model are saved in spillDir ("" = cut instead).
func NewRegistry(tools []Tool, toolsets map[string]Toolset, blocked []string, spillDir string) (*Registry, error) {
	r := &Registry{tools: map[string]Tool{}, toolsets: toolsets, spillDir: spillDir}
	for _, t := range tools {
		if _, dup := r.tools[t.Name]; dup {
			return nil, fmt.Errorf("tool %q registered twice", t.Name)
		}
		r.tools[t.Name] = t
	}
	for name, ts := range toolsets {
		for _, t := range ts.Tools {
			if _, ok := r.tools[t]; !ok {
				return nil, fmt.Errorf("toolset %q: unknown tool %q", name, t)
			}
		}
		for _, inc := range ts.Includes {
			if _, ok := toolsets[inc]; !ok {
				return nil, fmt.Errorf("toolset %q: unknown include %q", name, inc)
			}
		}
	}
	for _, b := range blocked {
		rule, err := parseRule(b)
		if err != nil {
			return nil, err
		}
		r.blocked = append(r.blocked, rule)
	}
	return r, nil
}

// List returns the tools of the given toolsets (with their includes),
// sorted by name.
func (r *Registry) List(toolsets ...string) []Tool {
	names := map[string]bool{}
	seen := map[string]bool{}
	var add func(string)
	add = func(ts string) {
		if seen[ts] {
			return
		}
		seen[ts] = true
		for _, t := range r.toolsets[ts].Tools {
			names[t] = true
		}
		for _, inc := range r.toolsets[ts].Includes {
			add(inc)
		}
	}
	for _, ts := range toolsets {
		add(ts)
	}
	var out []Tool
	for n := range names {
		out = append(out, r.tools[n])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Run runs a tool. It never panics and never returns a Go error: every
// problem becomes an error result the model can read and react to.
func (r *Registry) Run(ctx context.Context, name string, c Call) (res Result) {
	t, ok := r.tools[name]
	if !ok {
		return Result{IsError: true, Output: fmt.Sprintf("unknown tool %q. Valid tools: %s", name, strings.Join(r.names(), ", "))}
	}
	if len(c.Args) == 0 {
		c.Args = json.RawMessage(`{}`)
	}
	if rule, blocked := r.isBlocked(t, c); blocked {
		return Result{IsError: true, Output: fmt.Sprintf("blocked by config (%s). Do not try to work around it; ask the user.", rule)}
	}
	defer func() {
		if p := recover(); p != nil {
			slog.Error("tool panicked", "tool", name, "panic", p, "stack", string(debug.Stack()))
			res = Result{IsError: true, Output: fmt.Sprintf("internal error in %s: %v", name, p)}
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()
	c.IsReadBlocked = func(path string) bool { return r.isReadBlocked(path, c.Dir) }
	out, err := t.Run(ctx, c)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Result{IsError: true, Output: fmt.Sprintf("%s timed out after %s", name, defaultTimeout)}
		}
		return Result{IsError: true, Output: r.shorten(c.SessionID, name, err.Error())}
	}
	return Result{Output: r.shorten(c.SessionID, name, out)}
}

// PruneOutputs deletes saved outputs older than before, and session
// folders left empty. It returns how many files it deleted.
func (r *Registry) PruneOutputs(before time.Time) (int, error) {
	if r.spillDir == "" {
		return 0, nil
	}
	sessions, err := os.ReadDir(r.spillDir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, s := range sessions {
		if !s.IsDir() {
			continue
		}
		dir := filepath.Join(r.spillDir, s.Name())
		n, err := file.DeleteOld(dir, before)
		removed += n
		if err != nil {
			return removed, err
		}
		os.Remove(dir) // only succeeds once it is empty
	}
	return removed, nil
}

// shorten keeps an output within maxOutput characters. The full output is
// saved to a file the model can read in parts, so nothing is lost; without
// a spill folder (or when saving fails) the middle is cut.
func (r *Registry) shorten(sessionID, name, out string) string {
	runes := []rune(out)
	if len(runes) <= maxOutput || r.spillDir == "" {
		return cut(out)
	}
	dir := filepath.Join(r.spillDir, sessionID)
	path := filepath.Join(dir, fmt.Sprintf("%d-%s.txt", time.Now().UnixNano(), name))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return cut(out)
	}
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
		return cut(out)
	}
	part := maxOutput / 2
	return string(runes[:part]) + fmt.Sprintf("\n\n[Germes] The output is %d characters; the middle is not shown. The full output is in %s: "+
		"read parts of it with Read (offset, limit) or search it with Grep.\n\n", len(runes), path) + string(runes[len(runes)-part:])
}

func (r *Registry) names() []string {
	var names []string
	for n := range r.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// cut keeps output within maxOutput characters, keeping head and tail.
func cut(s string) string {
	r := []rune(s)
	if len(r) <= maxOutput {
		return s
	}
	half := maxOutput / 2
	return string(r[:half]) + fmt.Sprintf("\n\n[... %d characters cut ...]\n\n", len(r)-maxOutput) + string(r[len(r)-half:])
}
