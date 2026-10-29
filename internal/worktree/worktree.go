// Package worktree gives each session its own git worktree, so sessions on
// one project never clash. It never deletes work: a worktree with changes or
// unmerged commits is kept.
package worktree

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const gitTimeout = 30 * time.Second

// Worktree is one session's checkout.
type Worktree struct {
	Dir    string // where the session works
	Branch string // germes/<session id>
}

// ErrHasWork means removing the worktree would lose changes or commits.
var ErrHasWork = errors.New("the worktree has work that is not merged")

// IsRepo reports whether dir is the top of a git repository.
func IsRepo(ctx context.Context, dir string) bool {
	top, err := git(ctx, dir, "rev-parse", "--show-toplevel")
	return err == nil && filepath.Clean(top) == filepath.Clean(evalLinks(dir))
}

// Create makes a worktree for a session under base, on a new branch from
// the project's current HEAD.
func Create(ctx context.Context, base, project, sessionID string) (Worktree, error) {
	sum := sha256.Sum256([]byte(project))
	dir := filepath.Join(base, filepath.Base(project)+"-"+hex.EncodeToString(sum[:])[:8], sessionID)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return Worktree{}, err
	}
	branch := "germes/" + sessionID
	if _, err := git(ctx, project, "worktree", "add", "-b", branch, dir, "HEAD"); err != nil {
		return Worktree{}, fmt.Errorf("create worktree: %w", err)
	}
	return Worktree{Dir: dir, Branch: branch}, nil
}

// HasWork reports whether the worktree has uncommitted changes, or commits
// that the project's current branch does not have.
func HasWork(ctx context.Context, project string, wt Worktree) (bool, error) {
	status, err := git(ctx, wt.Dir, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	if status != "" {
		return true, nil
	}
	ahead, err := git(ctx, project, "rev-list", "--count", "HEAD.."+wt.Branch)
	if err != nil {
		return false, err
	}
	n, err := strconv.Atoi(ahead)
	if err != nil {
		return false, fmt.Errorf("read commit count %q: %w", ahead, err)
	}
	return n > 0, nil
}

// Delete removes the worktree and its branch. It refuses with ErrHasWork
// when that would lose anything.
func Delete(ctx context.Context, project string, wt Worktree) error {
	has, err := HasWork(ctx, project, wt)
	if err != nil {
		return err
	}
	if has {
		return ErrHasWork
	}
	if _, err := git(ctx, project, "worktree", "remove", wt.Dir); err != nil {
		return fmt.Errorf("remove worktree: %w", err)
	}
	// -d (not -D): git itself refuses to drop a branch that is not merged.
	if _, err := git(ctx, project, "branch", "-d", wt.Branch); err != nil {
		return fmt.Errorf("delete branch %s: %w", wt.Branch, err)
	}
	return nil
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return "", fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), text, err)
	}
	return text, nil
}

// evalLinks resolves symlinks (macOS /tmp is /private/tmp) so paths compare.
func evalLinks(dir string) string {
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		return real
	}
	return dir
}
