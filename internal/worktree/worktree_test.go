package worktree

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", args, out)
	}
}

func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run(t, dir, "git", "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644)
	run(t, dir, "git", "add", "a.txt")
	run(t, dir, "git", "commit", "-q", "-m", "init")
	return dir
}

func TestWorktreeNeverLosesWork(t *testing.T) {
	ctx := context.Background()
	project := newRepo(t)
	if !IsRepo(ctx, project) || IsRepo(ctx, t.TempDir()) {
		t.Fatal("IsRepo is wrong")
	}
	wt, err := Create(ctx, t.TempDir(), project, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if wt.Branch != "germes/abc123" {
		t.Fatalf("branch %q", wt.Branch)
	}

	// Uncommitted change: kept.
	os.WriteFile(filepath.Join(wt.Dir, "b.txt"), []byte("b"), 0o644)
	if err := Delete(ctx, project, wt); !errors.Is(err, ErrHasWork) {
		t.Fatalf("dirty worktree: %v", err)
	}
	// Committed but not merged: kept.
	run(t, wt.Dir, "git", "add", "b.txt")
	run(t, wt.Dir, "git", "commit", "-q", "-m", "b")
	if err := Delete(ctx, project, wt); !errors.Is(err, ErrHasWork) {
		t.Fatalf("unmerged worktree: %v", err)
	}
	// Merged into the project's branch: removed with its branch.
	run(t, project, "git", "merge", "-q", "--ff-only", wt.Branch)
	if err := Delete(ctx, project, wt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wt.Dir); !os.IsNotExist(err) {
		t.Fatal("worktree folder still there")
	}
}
