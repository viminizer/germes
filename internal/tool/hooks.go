package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"gopkg.in/yaml.v3"

	"github.com/viminizer/germes/internal/api"
)

// Hook runs before a matching tool call (issue #42): a command that must
// pass, or a skill the session must have loaded since its last edit.
type Hook struct {
	Before string `yaml:"before"`          // Tool(pattern), like a block rule
	Run    string `yaml:"run,omitempty"`   // a command that must pass first
	Skill  string `yaml:"skill,omitempty"` // a skill to load first
}

// Hooks are the hooks of every project (config.yaml) and what they need. A
// project adds its own in <dir>/.germes/hooks.yaml.
type Hooks struct {
	Global []Hook
	Run    func(ctx context.Context, dir, command string) (output string, passed bool, err error)
	Events api.EventLister // a session's saved tool calls: was a skill loaded since the last edit?
}

// HooksFile is a project's own hooks, next to its workflows.
const HooksFile = ".germes/hooks.yaml"

// editTools change files: a skill loaded before one of these must be loaded
// again. ponytail: Bash edits are not seen; add them if a hook needs it.
var editTools = []string{"Write", "Edit", "MultiEdit", "NotebookEdit"}

type hookRule struct {
	Hook
	rule rule
}

// UpdateHooks makes Check (and so Run) apply the hooks. Call it once, before
// any tool runs.
func (r *Registry) UpdateHooks(h Hooks) error {
	global, err := parseHooks(h.Global)
	if err != nil {
		return err
	}
	r.hooks, r.globalHooks = &h, global
	return nil
}

func parseHooks(hooks []Hook) ([]hookRule, error) {
	var out []hookRule
	for _, h := range hooks {
		rl, err := parseRule(h.Before)
		if err != nil {
			return nil, fmt.Errorf("hook: %w", err)
		}
		if (h.Run == "") == (h.Skill == "") {
			return nil, fmt.Errorf("hook before %s: give run or skill, not both or neither", h.Before)
		}
		out = append(out, hookRule{Hook: h, rule: rl})
	}
	return out, nil
}

// Check decides whether a call may run, the same way for every engine
// (issue #42): the block list, then the hooks. "" = it may; else why not,
// for the model. Run uses it for the Germes loop; `germes hook check` asks
// it for Claude's own tools. A tool Germes does not have (Claude's MultiEdit)
// is matched on its usual command or path argument.
func (r *Registry) Check(ctx context.Context, name string, c Call) string {
	t, ok := r.tools[name]
	if !ok {
		t = Tool{Name: name, MatchArg: guessArg(c.Args)}
	}
	return r.check(ctx, t, c)
}

func (r *Registry) check(ctx context.Context, t Tool, c Call) string {
	if len(c.Args) == 0 {
		c.Args = json.RawMessage(`{}`)
	}
	if rule, blocked := r.isBlocked(t, c); blocked {
		return fmt.Sprintf("blocked by config (%s). Do not try to work around it; ask the user.", rule)
	}
	return r.checkHooks(ctx, t, c)
}

func guessArg(args json.RawMessage) string {
	var m map[string]json.RawMessage
	json.Unmarshal(args, &m)
	for _, k := range []string{"command", "file_path", "notebook_path", "path", "url", "pattern"} {
		if _, ok := m[k]; ok {
			return k
		}
	}
	return ""
}

// checkHooks runs the hooks whose Before matches the call. Anything that
// stops a hook from deciding blocks the call: a hook never fails open.
func (r *Registry) checkHooks(ctx context.Context, t Tool, c Call) string {
	if r.hooks == nil {
		return ""
	}
	hooks, err := r.hooksFor(c.Dir)
	if err != nil {
		return fmt.Sprintf("blocked: the hooks cannot be read (%v); fix %s first", err, HooksFile)
	}
	for _, h := range hooks {
		if _, match := firstMatch([]rule{h.rule}, t, c); !match {
			continue
		}
		switch {
		case h.Run != "":
			out, passed, err := r.hooks.Run(ctx, c.Dir, h.Run)
			if err != nil {
				return fmt.Sprintf("blocked by a hook: `%s` could not run (%v)", h.Run, err)
			}
			if !passed {
				return fmt.Sprintf("blocked by a hook: `%s` must pass before %s. Its output:\n%s\nFix what it reports, then try again.", h.Run, h.Before, cutHookOutput(out))
			}
		case h.Skill != "":
			loaded, err := r.loadedSinceEdit(ctx, c.SessionID, h.Skill)
			if err != nil {
				return fmt.Sprintf("blocked by a hook: cannot read the session to check the skill %s (%v)", h.Skill, err)
			}
			if !loaded {
				return fmt.Sprintf("blocked by a hook: load the skill %s first (skill_view, or the Skill tool), follow it, then try again. An edit after loading it means loading it again.", h.Skill)
			}
		}
	}
	return ""
}

// hooksFor is the global hooks plus the project's own, read on each check so
// an edit to the file counts at once.
func (r *Registry) hooksFor(dir string) ([]hookRule, error) {
	b, err := os.ReadFile(filepath.Join(dir, HooksFile))
	if errors.Is(err, os.ErrNotExist) || dir == "" {
		return r.globalHooks, nil
	}
	if err != nil {
		return nil, err
	}
	var f struct {
		Hooks []Hook `yaml:"hooks"`
	}
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	project, err := parseHooks(f.Hooks)
	if err != nil {
		return nil, err
	}
	return append(slices.Clone(r.globalHooks), project...), nil
}

// loadedSinceEdit reports whether the session loaded skill after its last
// file edit.
func (r *Registry) loadedSinceEdit(ctx context.Context, sessionID, skill string) (bool, error) {
	if sessionID == "" || r.hooks.Events == nil {
		return false, nil
	}
	events, err := api.ListAllEvents(ctx, r.hooks.Events, api.EventFilter{SessionID: sessionID})
	if err != nil {
		return false, err
	}
	loaded := false
	for _, ev := range events {
		if ev.Kind != api.KindToolCall || ev.Tool == nil {
			continue
		}
		switch {
		case api.GetLoadedSkill(ev.Tool) == skill:
			loaded = true
		case slices.Contains(editTools, ev.Tool.Name):
			loaded = false
		}
	}
	return loaded, nil
}

// cutHookOutput keeps the end of a long hook output: that is where a build
// or a test run says what failed.
func cutHookOutput(s string) string {
	const max = 4000
	if r := []rune(s); len(r) > max {
		return "…" + string(r[len(r)-max:])
	}
	return s
}
