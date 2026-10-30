package checkpoint

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
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

func TestPrunePreservesUncertainSnapshots(t *testing.T) {
	for _, scenario := range []string{"folder stat failure", "missing index", "index stat failure", "index directory", "empty folder", "relative folder", "mismatched folder", "folder metadata failure", "cutoff equality", "whitespace in folder"} {
		t.Run(scenario, func(t *testing.T) {
			store, dir := t.TempDir(), filepath.Join(t.TempDir(), "project")
			if scenario == "whitespace in folder" {
				dir += " "
			}
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(dir, "a.txt"), "saved")
			if _, err := Create(context.Background(), store, dir, "turn"); err != nil {
				t.Fatal(err)
			}
			repo := repoPath(store, dir)
			index := filepath.Join(repo, "index")
			cutoff := time.Unix(1700000000, 0)
			ageRepo(t, repo, cutoff)
			wantError := true
			switch scenario {
			case "folder stat failure":
				if err := os.RemoveAll(dir); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(dir, dir); err != nil {
					t.Fatal(err)
				}
			case "index directory":
				if err := os.Remove(index); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(index, 0o700); err != nil {
					t.Fatal(err)
				}
			case "missing index", "index stat failure":
				if err := os.Remove(index); err != nil {
					t.Fatal(err)
				}
				if scenario == "index stat failure" {
					if err := os.Symlink(index, index); err != nil {
						t.Fatal(err)
					}
				}
			case "empty folder":
				write(t, filepath.Join(repo, "folder"), "\n")
			case "relative folder":
				write(t, filepath.Join(repo, "folder"), "missing-relative-project\n")
			case "mismatched folder":
				write(t, filepath.Join(repo, "folder"), filepath.Join(t.TempDir(), "gone")+"\n")
			case "folder metadata failure":
				if err := os.Remove(filepath.Join(repo, "folder")); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(repo, "folder"), 0o700); err != nil {
					t.Fatal(err)
				}
			case "cutoff equality", "whitespace in folder":
				wantError = false
			}
			n, err := Prune(store, cutoff)
			if n != 0 || (err != nil) != wantError {
				t.Errorf("pruned %d, error %v; want 0, error present %v", n, err, wantError)
			}
			if scenario == "missing index" && !errors.Is(err, os.ErrNotExist) {
				t.Errorf("missing index error = %v, want os.ErrNotExist", err)
			}
			if got := read(filepath.Join(repo, "HEAD")); got == "<gone>" {
				t.Error("snapshot history was deleted")
			}
		})
	}
}

func ageRepo(t *testing.T, repo string, stamp time.Time) {
	t.Helper()
	if err := filepath.WalkDir(repo, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			return os.Chtimes(path, stamp, stamp)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPruneSkipsLockedRepo(t *testing.T) {
	store, dir := t.TempDir(), t.TempDir()
	if _, err := Create(context.Background(), store, dir, "saved"); err != nil {
		t.Fatal(err)
	}
	repo := repoPath(store, dir)
	cutoff := time.Unix(1700000000, 0)
	ageRepo(t, repo, cutoff.Add(-time.Hour))
	// A distinct file descriptor models another daemon holding the sidecar.
	lock, err := os.OpenFile(repo+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if n, err := Prune(store, cutoff); n != 0 || err != nil {
		t.Fatalf("locked prune = %d, %v", n, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Create(ctx, store, dir, "cancelled"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled create = %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if n, err := Prune(store, cutoff); n != 1 || err != nil {
		t.Fatalf("unlocked prune = %d, %v", n, err)
	}
}

func TestPruneRetainsRecentCommitWithOldIndex(t *testing.T) {
	for _, packed := range []bool{false, true} {
		t.Run(map[bool]string{false: "loose", true: "packed"}[packed], func(t *testing.T) {
			store, dir := t.TempDir(), t.TempDir()
			ctx := context.Background()
			if _, err := Create(ctx, store, dir, "first"); err != nil {
				t.Fatal(err)
			}
			repo := repoPath(store, dir)
			cutoff := time.Unix(1700000000, 0)
			ageRepo(t, repo, cutoff.Add(-time.Hour))
			if _, err := Create(ctx, store, dir, "unchanged"); err != nil {
				t.Fatal(err)
			}
			if packed {
				if _, err := git(ctx, repo, dir, "pack-refs", "--all"); err != nil {
					t.Fatal(err)
				}
			}
			// Isolate the case where Git does not rewrite the index.
			if err := os.Chtimes(filepath.Join(repo, "index"), cutoff.Add(-time.Hour), cutoff.Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
			if n, err := Prune(store, cutoff); n != 0 || err != nil {
				t.Fatalf("recent commit prune = %d, %v", n, err)
			}
		})
	}
}

func TestPruneContinuesAfterInvalidRecord(t *testing.T) {
	store := t.TempDir()
	bad := filepath.Join(store, "a-invalid")
	write(t, filepath.Join(bad, "folder"), "invalid\n")
	dir := filepath.Join(t.TempDir(), "z-project")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(context.Background(), store, dir, "saved"); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Unix(1700000000, 0)
	ageRepo(t, repoPath(store, dir), cutoff.Add(-time.Hour))
	if n, err := Prune(store, cutoff); n != 1 || err == nil {
		t.Fatalf("partial cleanup = %d, %v; want 1 and metadata error", n, err)
	}
	if _, err := os.Stat(bad); err != nil {
		t.Fatalf("invalid entry removed: %v", err)
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
	cutoff := time.Unix(1700000000, 0)
	for dir, modified := range map[string]time.Time{fresh: cutoff.Add(time.Hour), old: cutoff.Add(-time.Hour)} {
		ageRepo(t, repoPath(store, dir), modified)
	}
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(store, "not-a-repo"), 0o700); err != nil {
		t.Fatal(err)
	}

	n, err := Prune(store, cutoff)
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
