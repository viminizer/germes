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

// ErrNoCommits means the project has no commit to start a worktree from.
var ErrNoCommits = errors.New("the project has no commits yet")

// IsRepo reports whether dir is the top of a git repository.
func IsRepo(ctx context.Context, dir string) bool {
	top, err := RunGit(ctx, dir, "rev-parse", "--show-toplevel")
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
	if _, err := RunGit(ctx, project, "rev-parse", "--verify", "--quiet", "HEAD"); err != nil {
		return Worktree{}, ErrNoCommits
	}
	branch := "germes/" + sessionID
	if _, err := RunGit(ctx, project, "worktree", "add", "-b", branch, dir, "HEAD"); err != nil {
		return Worktree{}, fmt.Errorf("create worktree: %w", err)
	}
	return Worktree{Dir: dir, Branch: branch}, nil
}

// HasWork checks both the actual checkout HEAD and the saved owned branch.
func HasWork(ctx context.Context, project string, wt Worktree) (bool, error) {
	_, _, err := inspect(ctx, project, wt)
	if errors.Is(err, ErrHasWork) {
		return true, nil
	}
	return false, err
}

// inspect returns the verified branch tip and whether the checkout remains.
// A missing, unregistered checkout is a retry after worktree removal; an
// existing but unrecognized checkout is never treated as a completed removal.
func inspect(ctx context.Context, project string, wt Worktree) (string, bool, error) {
	ref := "refs/heads/" + wt.Branch
	if !strings.HasPrefix(wt.Branch, "germes/") {
		return "", false, errors.New("not a session branch")
	}
	if _, err := RunGit(ctx, project, "check-ref-format", ref); err != nil {
		return "", false, err
	}
	registered, err := registeredCheckout(ctx, project, wt)
	if err != nil {
		return "", false, err
	}
	_, statErr := os.Lstat(wt.Dir)
	present := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return "", false, statErr
	}
	if registered != present {
		return "", false, errors.New("worktree registration and directory disagree; restore or repair the checkout before cleanup")
	}
	var heads []string
	if present {
		if !registered || !IsRepo(ctx, wt.Dir) {
			return "", false, errors.New("not the registered worktree root")
		}
		common, err := RunGit(ctx, project, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err != nil {
			return "", false, err
		}
		other, err := RunGit(ctx, wt.Dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err != nil || evalLinks(common) != evalLinks(other) {
			return "", false, fmt.Errorf("cannot verify checkout repository: %w", errors.Join(err, errors.New("repository identity mismatch")))
		}
		status, err := RunGit(ctx, wt.Dir, "status", "--porcelain")
		if err != nil {
			return "", false, err
		}
		if status != "" {
			return "", false, ErrHasWork
		}
		head, err := RunGit(ctx, wt.Dir, "rev-parse", "--verify", "HEAD")
		if err != nil {
			return "", false, err
		}
		heads = append(heads, head)
	}
	refs, err := RunGit(ctx, project, "for-each-ref", "--format=%(refname) %(objectname)", ref)
	if err != nil {
		return "", false, err
	}
	tip := ""
	for _, line := range strings.Split(refs, "\n") {
		name, oid, _ := strings.Cut(line, " ")
		if name == ref {
			tip = oid
			heads = append(heads, tip)
		}
	}
	for _, head := range heads {
		ahead, err := RunGit(ctx, project, "rev-list", "--count", "HEAD.."+head)
		if err != nil {
			return "", false, err
		}
		n, err := strconv.Atoi(ahead)
		if err != nil {
			return "", false, fmt.Errorf("read commit count %q: %w", ahead, err)
		}
		if n > 0 {
			return "", false, ErrHasWork
		}
	}
	return tip, present, nil
}

func registeredCheckout(ctx context.Context, project string, wt Worktree) (bool, error) {
	out, err := RunGit(ctx, project, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return false, err
	}
	found, current := false, ""
	for _, field := range strings.Split(out, "\x00") {
		if path, ok := strings.CutPrefix(field, "worktree "); ok {
			current = filepath.Clean(evalLinks(path))
			if current == filepath.Clean(evalLinks(wt.Dir)) {
				found = true
				if current == filepath.Clean(evalLinks(project)) {
					return false, errors.New("cannot remove the project checkout")
				}
			}
		}
		if branch, ok := strings.CutPrefix(field, "branch "); ok {
			ours := branch == "refs/heads/"+wt.Branch
			target := current == filepath.Clean(evalLinks(wt.Dir))
			if (target && !ours) || (ours && !target) {
				return false, errors.New("session branch is checked out in a different checkout")
			}
		}
	}
	return found, nil
}

// Delete is retryable after directory or branch removal. Remove the owned
// branch's upstream so Git rechecks ancestry against project HEAD, not a stale
// tracking ref. Ordinary branch deletion also protects checked-out branches.
func Delete(ctx context.Context, project string, wt Worktree) error {
	tip, present, err := inspect(ctx, project, wt)
	if err != nil {
		return err
	}
	if present {
		if _, err := RunGit(ctx, project, "worktree", "remove", wt.Dir); err != nil {
			return fmt.Errorf("remove worktree: %w", err)
		}
	}
	if tip != "" {
		if _, err := registeredCheckout(ctx, project, wt); err != nil {
			return err
		}
		upstream, err := RunGit(ctx, project, "for-each-ref", "--count=1", "--format=%(upstream)", "refs/heads/"+wt.Branch)
		if err != nil {
			return err
		}
		if upstream != "" {
			if _, err := RunGit(ctx, project, "branch", "--unset-upstream", wt.Branch); err != nil {
				return fmt.Errorf("unset owned branch upstream (retry cleanup): %w", err)
			}
		}
		if _, err := RunGit(ctx, project, "branch", "-d", "--", wt.Branch); err != nil {
			return fmt.Errorf("delete branch %s (retry cleanup): %w", wt.Branch, err)
		}
	}
	return nil
}

// RunGit runs git in dir with a timeout and returns its trimmed output.
func RunGit(ctx context.Context, dir string, args ...string) (string, error) {
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
