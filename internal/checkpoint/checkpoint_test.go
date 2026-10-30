package checkpoint

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func write(t *testing.T, path, text string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o700)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "<gone>"
	}
	return string(b)
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s", args, out)
	}
	return string(out)
}

func TestRollbackRestoresFiles(t *testing.T) {
	ctx, store, dir := context.Background(), t.TempDir(), t.TempDir()
	gitOut(t, dir, "init", "-q")
	write(t, filepath.Join(dir, "a.txt"), "one")
	write(t, filepath.Join(dir, "gone.txt"), "keep me")
	write(t, filepath.Join(dir, ".gitignore"), "secret.txt\n")
	gitOut(t, dir, "add", "-A")
	gitOut(t, dir, "commit", "-qm", "init")
	head := gitOut(t, dir, "rev-parse", "HEAD")

	before, err := Create(ctx, store, dir, "before turn 1")
	if err != nil {
		t.Fatal(err)
	}
	// The turn: changes a file, deletes one, adds one; the user has an
	// ignored file and Germes saves a plan meanwhile.
	write(t, filepath.Join(dir, "a.txt"), "two")
	os.Remove(filepath.Join(dir, "gone.txt"))
	write(t, filepath.Join(dir, "new.txt"), "new")
	write(t, filepath.Join(dir, "secret.txt"), "ignored")
	write(t, filepath.Join(dir, ".germes", "plans", "p.md"), "plan")

	if err := Restore(ctx, store, dir, before); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{"a.txt": "one", "gone.txt": "keep me", "new.txt": "<gone>",
		"secret.txt": "ignored", ".germes/plans/p.md": "plan"} {
		if got := read(filepath.Join(dir, path)); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	// The project's own git is untouched.
	// (.germes/ is the plan saved during the turn.)
	if gitOut(t, dir, "rev-parse", "HEAD") != head || gitOut(t, dir, "status", "--porcelain") != "?? .germes/\n" {
		t.Errorf("project git changed: %s", gitOut(t, dir, "status", "--porcelain"))
	}
	// The rollback can be undone: the state before it is a snapshot too.
	list, err := List(ctx, store, dir, 10)
	if err != nil || len(list) != 3 || list[1].Label != "before rollback to "+before[:8] {
		t.Fatalf("snapshots %+v, %v", list, err)
	}
	if err := Restore(ctx, store, dir, list[1].ID); err != nil {
		t.Fatal(err)
	}
	if read(filepath.Join(dir, "a.txt")) != "two" || read(filepath.Join(dir, "new.txt")) != "new" {
		t.Error("undoing the rollback did not bring the turn's work back")
	}
}

func TestPruneDropsUnusedAndOrphaned(t *testing.T) {
	ctx, store := context.Background(), t.TempDir()
	fresh, old, gone := t.TempDir(), t.TempDir(), t.TempDir()
	for _, dir := range []string{fresh, old, gone} {
		write(t, filepath.Join(dir, "a.txt"), "x")
		if _, err := Create(ctx, store, dir, "turn"); err != nil {
			t.Fatal(err)
		}
	}
	long := time.Now().Add(-60 * 24 * time.Hour)
	os.Chtimes(filepath.Join(repoPath(store, old), "index"), long, long)
	os.RemoveAll(gone)
	os.MkdirAll(filepath.Join(store, "not-a-repo"), 0o700)

	n, err := Prune(store, time.Now().Add(-30*24*time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	for dir, want := range map[string]bool{fresh: true, old: false, gone: false} {
		if _, err := os.Stat(repoPath(store, dir)); (err == nil) != want {
			t.Errorf("repo of %s kept=%v, want %v", dir, err == nil, want)
		}
	}
	if _, err := os.Stat(filepath.Join(store, "not-a-repo")); err != nil {
		t.Error("removed a folder that is not a snapshot repo")
	}
}
