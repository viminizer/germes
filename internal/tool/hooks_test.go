package tool

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/viminizer/germes/internal/api"
)

type events []api.Event

func (e events) ListEvents(context.Context, api.EventFilter) (api.Page[api.Event], error) {
	return api.Page[api.Event]{Items: e}, nil
}

func call(name, args string) api.Event {
	return api.Event{Kind: api.KindToolCall, Tool: &api.ToolCall{Name: name, Args: json.RawMessage(args)}}
}

// hookRegistry has Bash, a "make check" that passes when passes is set, and
// the session's saved calls.
func hookRegistry(t *testing.T, passes *bool, history events, hooks ...Hook) (*Registry, *int) {
	t.Helper()
	ran := 0
	r, err := NewRegistry([]Tool{{Name: "Bash", MatchArg: "command", Run: func(context.Context, Call) (string, error) { ran++; return "ok", nil }}},
		map[string]Toolset{"core": {Tools: []string{"Bash"}}}, []string{"MultiEdit(**/.env)"}, "")
	if err != nil {
		t.Fatal(err)
	}
	run := func(_ context.Context, dir, cmd string) (string, bool, error) {
		if passes == nil {
			return "", false, errors.New("no shell")
		}
		return "FAIL: TestX", *passes, nil
	}
	if err := r.UpdateHooks(Hooks{Global: hooks, Run: run, Events: history}); err != nil {
		t.Fatal(err)
	}
	return r, &ran
}

const prCreate = `{"command":"gh pr create --fill"}`

func TestRunHookMustPass(t *testing.T) {
	fail, pass := false, true
	hook := Hook{Before: "Bash(gh pr create*)", Run: "make check"}
	for name, tc := range map[string]struct {
		passes *bool
		want   string // "" = allowed
	}{
		"fails":      {&fail, "`make check` must pass"},
		"passes":     {&pass, ""},
		"cannot run": {nil, "could not run"}, // never fails open
	} {
		r, ran := hookRegistry(t, tc.passes, nil, hook)
		res := r.Run(context.Background(), "Bash", Call{Dir: t.TempDir(), Args: json.RawMessage(prCreate)})
		if blocked := res.IsError; blocked != (tc.want != "") || !strings.Contains(res.Output, tc.want) {
			t.Errorf("%s: %+v", name, res)
		}
		if name == "fails" && !strings.Contains(res.Output, "FAIL: TestX") {
			t.Errorf("%s: the model does not see the output: %q", name, res.Output)
		}
		if (*ran == 1) != (tc.want == "") {
			t.Errorf("%s: the tool ran %d times", name, *ran)
		}
	}
	// Other commands are not held.
	r, _ := hookRegistry(t, &fail, nil, hook)
	if why := r.Check(context.Background(), "Bash", Call{Args: json.RawMessage(`{"command":"git status"}`)}); why != "" {
		t.Fatalf("an unrelated call was held: %s", why)
	}
}

func TestSkillHookNeedsTheSkillSinceTheLastEdit(t *testing.T) {
	hook := Hook{Before: "Bash(gh pr create*)", Skill: "failure-cases"}
	load, edit := call("skill_view", `{"name":"failure-cases"}`), call("Edit", `{"file_path":"a.go"}`)
	claudeLoad := call("Skill", `{"skill":"failure-cases"}`)
	for name, tc := range map[string]struct {
		history events
		allowed bool
	}{
		"never loaded":         {events{edit}, false},
		"loaded":               {events{edit, load}, true},
		"loaded, then an edit": {events{load, edit}, false},
		"loaded again":         {events{load, edit, claudeLoad}, true},
	} {
		r, _ := hookRegistry(t, nil, tc.history, hook)
		why := r.Check(context.Background(), "Bash", Call{SessionID: "s1", Args: json.RawMessage(prCreate)})
		if (why == "") != tc.allowed {
			t.Errorf("%s: %q", name, why)
		}
	}
}

func TestProjectHooksFile(t *testing.T) {
	fail := false
	r, _ := hookRegistry(t, &fail, nil)
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".germes"), 0o700)
	os.WriteFile(filepath.Join(dir, HooksFile), []byte("hooks:\n  - before: Bash(gh pr create*)\n    run: make check\n"), 0o600)
	if why := r.Check(context.Background(), "Bash", Call{Dir: dir, Args: json.RawMessage(prCreate)}); !strings.Contains(why, "must pass") {
		t.Fatalf("the project's hook did not hold the call: %q", why)
	}
	os.WriteFile(filepath.Join(dir, HooksFile), []byte("hooks: [ {before: nope"), 0o600)
	if why := r.Check(context.Background(), "Bash", Call{Dir: dir, Args: json.RawMessage(`{"command":"ls"}`)}); !strings.Contains(why, "hooks cannot be read") {
		t.Fatalf("a broken hooks file let a call through: %q", why)
	}
}

// Claude's own tools are checked by the same rules, matched on their usual
// argument.
func TestCheckToolsGermesDoesNotHave(t *testing.T) {
	r, _ := hookRegistry(t, nil, nil)
	if why := r.Check(context.Background(), "MultiEdit", Call{Dir: "/proj", Args: json.RawMessage(`{"file_path":"/proj/.env","edits":[]}`)}); !strings.Contains(why, "blocked by config") {
		t.Fatalf("MultiEdit on .env was allowed: %q", why)
	}
	if why := r.Check(context.Background(), "MultiEdit", Call{Dir: "/proj", Args: json.RawMessage(`{"file_path":"/proj/main.go"}`)}); why != "" {
		t.Fatalf("MultiEdit on main.go was held: %q", why)
	}
}
