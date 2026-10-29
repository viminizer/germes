package files

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/viminizer/germes/internal/tool"
)

// run calls one of the file tools by name.
func run(t *testing.T, tools []tool.Tool, dir, name string, args map[string]any) (string, error) {
	t.Helper()
	b, _ := json.Marshal(args)
	for _, tl := range tools {
		if tl.Name == name {
			return tl.Run(context.Background(), tool.Call{SessionID: "s1", Dir: dir, Args: b})
		}
	}
	t.Fatalf("no tool %s", name)
	return "", nil
}

func TestWriteNeedsAFreshRead(t *testing.T) {
	dir, tools := t.TempDir(), New()
	if _, err := run(t, tools, dir, "Write", map[string]any{"file_path": "new.txt", "content": "a"}); err != nil {
		t.Fatalf("a new file: %v", err)
	}
	os.WriteFile(filepath.Join(dir, "old.txt"), []byte("theirs"), 0o644)
	if _, err := run(t, tools, dir, "Write", map[string]any{"file_path": "old.txt", "content": "mine"}); err == nil {
		t.Fatal("overwrote a file it never read")
	}
	run(t, tools, dir, "Read", map[string]any{"file_path": "old.txt"})
	if _, err := run(t, tools, dir, "Write", map[string]any{"file_path": "old.txt", "content": "mine"}); err != nil {
		t.Fatalf("after reading: %v", err)
	}
	// Someone else changes it: a fresh read is needed again.
	later := time.Now().Add(time.Second)
	os.WriteFile(filepath.Join(dir, "old.txt"), []byte("changed"), 0o644)
	os.Chtimes(filepath.Join(dir, "old.txt"), later, later)
	if _, err := run(t, tools, dir, "Write", map[string]any{"file_path": "old.txt", "content": "mine"}); err == nil {
		t.Fatal("overwrote a file changed since the read")
	}
}

func TestEditUndoesAddedEscaping(t *testing.T) {
	dir, tools := t.TempDir(), New()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte(`say "hi"`), 0o644)
	if _, err := run(t, tools, dir, "Edit", map[string]any{"file_path": "a.txt", "old_string": `say \"hi\"`, "new_string": `say \"bye\"`}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(b) != `say "bye"` {
		t.Fatalf("file: %s", b)
	}
}

func TestEditReportsBrokenSyntax(t *testing.T) {
	if _, err := exec.LookPath("gofmt"); err != nil {
		t.Skip("no gofmt")
	}
	dir, tools := t.TempDir(), New()
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n\nfunc A() {}\n"), 0o644)
	out, err := run(t, tools, dir, "Edit", map[string]any{"file_path": "a.go", "old_string": "func A() {}", "new_string": "func A() {"})
	if err != nil || !strings.Contains(out, "syntax check failed") {
		t.Fatalf("out %q, err %v", out, err)
	}
}
