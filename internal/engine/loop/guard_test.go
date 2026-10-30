package loop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/viminizer/germes/internal/llm"
	"github.com/viminizer/germes/internal/tool"
)

func bash(cmd string) llm.ToolCall {
	args, _ := json.Marshal(map[string]string{"command": cmd})
	return llm.ToolCall{Name: "Bash", Args: args}
}

func TestGuardStopsRepeatedFailures(t *testing.T) {
	g := newGuard(t.TempDir())
	c := bash("go run ./broken")
	g.note(c, tool.Result{IsError: true})
	if g.refuse(c) != "" {
		t.Fatal("refused after one failure")
	}
	if note := g.note(c, tool.Result{IsError: true}); !strings.Contains(note, "failed twice") {
		t.Fatalf("no warning: %q", note)
	}
	if g.refuse(c) == "" {
		t.Fatal("ran a call that failed twice")
	}
	if g.refuse(bash("go run ./other")) != "" {
		t.Fatal("refused a different call")
	}
}

func TestGuardSendsBackUncheckedChanges(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o600)
	g := newGuard(dir)
	if g.finishNudge() != "" {
		t.Fatal("nudged without changes")
	}
	g.note(llm.ToolCall{Name: "Edit"}, tool.Result{})
	g.note(bash("go test ./... | tail -5"), tool.Result{}) // piped: its exit code is hidden
	if g.finishNudge() == "" {
		t.Fatal("a piped check counted")
	}
	g = newGuard(dir)
	g.note(llm.ToolCall{Name: "Write"}, tool.Result{})
	g.note(bash("go build ./... && go test ./..."), tool.Result{})
	if g.finishNudge() != "" {
		t.Fatal("nudged after a passing check")
	}
	g.note(llm.ToolCall{Name: "Edit"}, tool.Result{})
	if n := g.finishNudge(); !strings.Contains(n, "go build ./... && go vet ./... && go test ./...") {
		t.Fatalf("nudge: %q", n)
	}
	if g.finishNudge() != "" {
		t.Fatal("nudged twice in one turn")
	}
}

func TestGuardRunsAFailedCallAgainAfterAnEdit(t *testing.T) {
	g := newGuard(t.TempDir())
	test := bash("go test ./...")
	g.note(test, tool.Result{IsError: true})
	g.note(test, tool.Result{IsError: true})
	g.note(llm.ToolCall{Name: "Edit"}, tool.Result{})
	if g.refuse(test) != "" {
		t.Fatal("refused a check after the code changed")
	}
}
