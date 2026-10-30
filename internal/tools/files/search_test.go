package files

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// search runs Glob or Grep in dir.
func search(t *testing.T, name, dir, pattern string) string {
	t.Helper()
	out, err := run(t, New(), dir, name, map[string]any{"pattern": pattern})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// project is a small git project with a build folder git ignores.
func project(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	exec.Command("git", "-C", dir, "init", "-q").Run()
	os.MkdirAll(filepath.Join(dir, "dist"), 0o700)
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("dist/\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n// TODO: real\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "dist", "app.js"), []byte("// TODO: built\n"), 0o600)
	return dir
}

func TestRipgrepGrepSkipsIgnoredFiles(t *testing.T) {
	if rgPath() == "" {
		t.Skip("no rg")
	}
	dir := project(t)
	if got := search(t, "Grep", dir, "TODO"); got != "main.go:2: // TODO: real" {
		t.Fatalf("Grep: %q", got)
	}
}

func TestRipgrepGlobSkipsIgnoredFiles(t *testing.T) {
	if rgPath() == "" {
		t.Skip("no rg")
	}
	if got := search(t, "Glob", project(t), "**/*.js"); got != "no files match" {
		t.Fatalf("Glob: %q", got)
	}
}

func TestRipgrepKeepsColonsInPaths(t *testing.T) {
	if rgPath() == "" {
		t.Skip("no rg")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a:b.txt"), []byte("hit\n"), 0o600)
	if got := search(t, "Grep", dir, "hit"); got != "a:b.txt:1: hit" {
		t.Fatalf("Grep: %q", got)
	}
}

func TestBackendsPrintTheSame(t *testing.T) {
	if rgPath() == "" {
		t.Skip("no rg")
	}
	dir := project(t)
	os.RemoveAll(filepath.Join(dir, "dist")) // the walk does not read .gitignore
	withRg := search(t, "Grep", dir, "TODO") + search(t, "Glob", dir, "*.go")
	t.Setenv("PATH", "")
	if walked := search(t, "Grep", dir, "TODO") + search(t, "Glob", dir, "*.go"); walked != withRg {
		t.Fatalf("rg %q, walk %q", withRg, walked)
	}
}

func TestCutResultsSaySo(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte(strings.Repeat("hit\n", maxMatches+50)), 0o600)
	want := map[bool]string{true: "[showing 200 of 250 matches", false: "[showing 200 of more than 200 matches"}
	for _, withRg := range []bool{true, false} {
		if withRg && rgPath() == "" {
			continue
		}
		if !withRg {
			t.Setenv("PATH", "")
		}
		if got := search(t, "Grep", dir, "hit"); !strings.Contains(got, want[withRg]) {
			t.Errorf("rg %v: no note in %q", withRg, got[len(got)-120:])
		}
	}
}
