package job

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/viminizer/germes/internal/api"
)

func TestParseWorkflowFindsMistakes(t *testing.T) {
	_, err := ParseWorkflow([]byte(`
name: bad
steps:
  - id: find
    agent: "x"
    next: { ISSUE: fix, none: done }
  - id: check
    run: "go test"
    agent: "y"
    next: nowhere
  - id: find
    run: notify
    next: done
`))
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{`"find" is used twice`, `unknown step "fix"`, `token "none"`, "exactly one of agent or run", `unknown step "nowhere"`, "notify needs a text"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

func TestParseResult(t *testing.T) {
	tok, val, ok := parseResult("I looked.\n\n**RESULT: ISSUE 42**\n")
	if !ok || tok != "ISSUE" || val != "42" {
		t.Fatalf("got %q %q %v", tok, val, ok)
	}
	if _, _, ok := parseResult("no result here"); ok {
		t.Fatal("found a result in none")
	}
}

func TestFillQuotesForTheShell(t *testing.T) {
	got := fill("gh issue view {ISSUE}", map[string]string{"ISSUE": "1; rm -rf ~ '"}, true)
	if got != `gh issue view '1; rm -rf ~ '\'''` {
		t.Fatalf("got %s", got)
	}
}

// fakeTools fails each command whose text is in fail (once), and records
// every command.
type fakeTools struct {
	mu       sync.Mutex
	fail     map[string]bool
	commands []string
}

func (f *fakeTools) RunTool(ctx context.Context, r api.ToolRequest) (api.ToolResult, error) {
	var a struct{ Command string }
	json.Unmarshal(r.Args, &a)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, a.Command)
	if f.fail[a.Command] {
		delete(f.fail, a.Command)
		return api.ToolResult{Output: "FAIL: TestX", IsError: true}, nil
	}
	return api.ToolResult{Output: "ok"}, nil
}

const fixIssues = `
name: fix-issues
steps:
  - id: find
    agent: "Pick the most important open issue."
    next: { ISSUE: fix, NONE: done }
  - id: fix
    agent: "Fix issue {ISSUE}."
    next: check
  - id: check
    run: "test {ISSUE}"
    on_fail: fix
    next: find
`

func writeWorkflow(t *testing.T, dir, name, body string) {
	t.Helper()
	wd := filepath.Join(dir, ".germes", "workflows")
	os.MkdirAll(wd, 0o700)
	if err := os.WriteFile(filepath.Join(wd, name+".yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWorkflowFollowsSteps(t *testing.T) {
	tools := &fakeTools{fail: map[string]bool{"test '42'": true}}
	s, r := newTestSchedulerWith(t, Options{Tools: tools},
		"Found it.\nRESULT: ISSUE 42", // round 1: find
		"fixed",                       // fix
		"fixed again",                 // fix, after check failed
		"Nothing left.\nRESULT: NONE", // round 2: find
	)
	dir := t.TempDir()
	writeWorkflow(t, dir, "fix-issues", fixIssues)
	j, err := s.CreateJob(context.Background(), api.JobRequest{Name: "w", Dir: dir, Workflow: "fix-issues"})
	if err != nil {
		t.Fatal(err)
	}
	j = waitStatus(t, s, j.ID)
	if j.Status != api.JobDone || j.Runs != 2 || j.Failures != 0 {
		t.Fatalf("job: %s, %d runs, %d failures (%s)", j.Status, j.Runs, j.Failures, j.Error)
	}
	if got := strings.Join(tools.commands, " | "); got != "test '42' | test '42'" {
		t.Fatalf("commands: %s", got)
	}
	// One session per round; the steps of a round share it.
	if r.turns[0].SessionID != r.turns[2].SessionID || r.turns[0].SessionID == r.turns[3].SessionID {
		t.Fatal("rounds and sessions do not line up")
	}
	if !strings.Contains(r.turns[1].Prompt, "Fix issue 42.") || !strings.Contains(r.turns[2].Prompt, "FAIL: TestX") {
		t.Fatalf("fix prompts:\n%s\n---\n%s", r.turns[1].Prompt, r.turns[2].Prompt)
	}
	if !strings.Contains(r.turns[0].Prompt, "RESULT: <TOKEN>") {
		t.Fatalf("find prompt lacks the RESULT rule:\n%s", r.turns[0].Prompt)
	}
}

func TestWorkflowRoundFailsWithoutResult(t *testing.T) {
	s, r := newTestSchedulerWith(t, Options{Tools: &fakeTools{}}, "I am not sure.")
	dir := t.TempDir()
	writeWorkflow(t, dir, "fix-issues", fixIssues)
	j, _ := s.CreateJob(context.Background(), api.JobRequest{Dir: dir, Workflow: "fix-issues", MaxRuns: 1})
	j = waitStatus(t, s, j.ID)
	if j.Failures != 1 || !strings.Contains(j.Error, "no RESULT line with one of ISSUE, NONE") {
		t.Fatalf("job: %s, %d failures (%s)", j.Status, j.Failures, j.Error)
	}
	if len(r.turns) != 2 || !strings.Contains(r.turns[1].Prompt, "must end with the RESULT line") {
		t.Fatalf("turns: %d", len(r.turns))
	}
}

func TestWorkflowResumesSavedStep(t *testing.T) {
	tools := &fakeTools{}
	s, r := newTestSchedulerWith(t, Options{Tools: tools}, "RESULT: NONE")
	ctx := context.Background()
	dir := t.TempDir()
	writeWorkflow(t, dir, "fix-issues", fixIssues)
	// A job cut by a restart while step check was about to run.
	j := api.Job{ID: "resume", Name: "resume", Dir: dir, Workflow: "fix-issues", Status: api.JobStopped, Runs: 1,
		Step: "check", Values: map[string]string{"ISSUE": "7"}, Visits: map[string]int{"find": 1, "fix": 1, "check": 1}}
	if err := s.store.CreateJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	if err := s.StartJob(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	j = waitStatus(t, s, j.ID)
	if j.Status != api.JobDone || j.Runs != 2 {
		t.Fatalf("job: %s after %d runs (%s)", j.Status, j.Runs, j.Error)
	}
	if got := strings.Join(tools.commands, " | "); got != "test '7'" || len(r.turns) != 1 {
		t.Fatalf("commands %q, %d turns", got, len(r.turns))
	}
}
