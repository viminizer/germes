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

func TestDeleteDetachedWork(t *testing.T) {
	ctx := context.Background()
	project := newRepo(t)
	wt, err := Create(ctx, t.TempDir(), project, "detached")
	if err != nil {
		t.Fatal(err)
	}
	run(t, wt.Dir, "git", "checkout", "--detach", "-q")
	run(t, wt.Dir, "git", "commit", "--allow-empty", "-q", "-m", "detached work")
	if err := Delete(ctx, project, wt); !errors.Is(err, ErrHasWork) {
		t.Fatalf("detached work must stay: %v", err)
	}
	if _, err := os.Stat(wt.Dir); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteStaleUpstream(t *testing.T) {
	ctx := context.Background()
	project := newRepo(t)
	run(t, project, "git", "branch", "stale")
	wt, err := Create(ctx, t.TempDir(), project, "upstream")
	if err != nil {
		t.Fatal(err)
	}
	run(t, wt.Dir, "git", "branch", "--set-upstream-to=stale")
	run(t, wt.Dir, "git", "commit", "--allow-empty", "-q", "-m", "merged work")
	run(t, project, "git", "merge", "--ff-only", wt.Branch)
	if err := Delete(ctx, project, wt); err != nil {
		t.Fatal(err)
	}
	if err := Delete(ctx, project, wt); err != nil {
		t.Fatalf("completed cleanup must be retryable: %v", err)
	}
}

func TestDeleteAfterDirectoryRemoval(t *testing.T) {
	ctx := context.Background()
	project := newRepo(t)
	wt, err := Create(ctx, t.TempDir(), project, "partial")
	if err != nil {
		t.Fatal(err)
	}
	run(t, project, "git", "worktree", "remove", wt.Dir)
	if err := Delete(ctx, project, wt); err != nil {
		t.Fatalf("retry branch cleanup: %v", err)
	}
	if _, err := RunGit(ctx, project, "show-ref", "--verify", "refs/heads/"+wt.Branch); err == nil {
		t.Fatal("owned branch remains")
	}
}

func TestDeletePreservesUncertainCheckout(t *testing.T) {
	for _, scenario := range []string{"switched branch", "unregistered directory", "missing registered directory", "wrong repository", "branch used elsewhere", "unmerged branch after removal"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			project := newRepo(t)
			wt, err := Create(ctx, t.TempDir(), project, "identity")
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "switched branch":
				run(t, wt.Dir, "git", "checkout", "-q", "-b", "other")
			case "unregistered directory":
				wt.Dir = newRepo(t)
			case "missing registered directory":
				if err := os.RemoveAll(wt.Dir); err != nil {
					t.Fatal(err)
				}
			case "wrong repository":
				project = newRepo(t)
			case "branch used elsewhere":
				run(t, project, "git", "worktree", "remove", wt.Dir)
				run(t, project, "git", "worktree", "add", filepath.Join(t.TempDir(), "other"), wt.Branch)
			case "unmerged branch after removal":
				run(t, wt.Dir, "git", "commit", "--allow-empty", "-q", "-m", "unmerged")
				run(t, project, "git", "worktree", "remove", wt.Dir)
			}
			if err := Delete(ctx, project, wt); err == nil {
				t.Fatal("unsafe cleanup accepted")
			}
		})
	}
}

func TestDeleteMergedDetachedCheckout(t *testing.T) {
	ctx := context.Background()
	project := newRepo(t)
	wt, err := Create(ctx, t.TempDir(), project, "merged-detached")
	if err != nil {
		t.Fatal(err)
	}
	run(t, wt.Dir, "git", "checkout", "--detach", "-q")
	run(t, wt.Dir, "git", "commit", "--allow-empty", "-q", "-m", "detached work")
	head, err := RunGit(ctx, wt.Dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	run(t, project, "git", "merge", "--ff-only", head)
	if err := Delete(ctx, project, wt); err != nil {
		t.Fatal(err)
	}
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
