// Package checkpoint saves a folder's files before each turn and puts them
// back on request (rollback). Snapshots go into a hidden git repo per folder
// under the store, apart from the project's own git: its history, branches
// and index are never touched. The folder's .gitignore applies.
package checkpoint

import (
	"context"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/viminizer/germes/internal/worktree"
)

// Checkpoint is one snapshot of a folder.
type Checkpoint struct {
	ID    string // the snapshot's commit
	Label string
	Time  time.Time
}

// Never in a snapshot: Germes' own files (plans), which a rollback must not
// delete.
const exclude = ".germes/\n"

// Create snapshots dir's files with a label and returns the snapshot's id.
// An unchanged folder gives a snapshot too, so every turn has one.
func Create(ctx context.Context, store, dir, label string) (string, error) {
	gitDir, err := open(ctx, store, dir)
	if err != nil {
		return "", err
	}
	if _, err := git(ctx, gitDir, dir, "add", "-A"); err != nil {
		return "", err
	}
	if _, err := git(ctx, gitDir, dir, "commit", "-q", "--allow-empty", "--no-verify", "-m", label); err != nil {
		return "", err
	}
	return git(ctx, gitDir, dir, "rev-parse", "HEAD")
}

// List lists dir's snapshots, newest first, at most n.
func List(ctx context.Context, store, dir string, n int) ([]Checkpoint, error) {
	gitDir := repoPath(store, dir)
	if _, err := os.Stat(gitDir); err != nil {
		return nil, nil // no snapshot yet
	}
	out, err := git(ctx, gitDir, dir, "log", fmt.Sprintf("-%d", n), "--format=%H%x00%ct%x00%s")
	if err != nil || out == "" {
		return nil, err
	}
	var list []Checkpoint
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "\x00", 3)
		if len(parts) != 3 {
			continue
		}
		var secs int64
		fmt.Sscan(parts[1], &secs)
		list = append(list, Checkpoint{ID: parts[0], Label: parts[2], Time: time.Unix(secs, 0)})
	}
	return list, nil
}

// Restore puts dir's files back as they were in snapshot id: changed files
// get their old content, files added since are removed. The current state
// is saved first, so a rollback can be undone.
func Restore(ctx context.Context, store, dir, id string) error {
	if _, err := Create(ctx, store, dir, "before rollback to "+short(id)); err != nil {
		return err
	}
	gitDir := repoPath(store, dir)
	if _, err := git(ctx, gitDir, dir, "read-tree", "-u", "--reset", id); err != nil {
		return err
	}
	_, err := git(ctx, gitDir, dir, "commit", "-q", "--allow-empty", "--no-verify", "-m", "rollback to "+short(id))
	return err
}

// open returns dir's snapshot repo, making it on first use.
func open(ctx context.Context, store, dir string) (string, error) {
	gitDir := repoPath(store, dir)
	if _, err := os.Stat(gitDir); err == nil {
		return gitDir, nil
	}
	if err := os.MkdirAll(filepath.Dir(gitDir), 0o700); err != nil {
		return "", err
	}
	if _, err := worktree.RunGit(ctx, dir, "init", "-q", "--bare", gitDir); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(gitDir, "info", "exclude"), []byte(exclude), 0o600); err != nil {
		return "", err
	}
	// A record of which folder this is, for whoever looks in the store.
	return gitDir, os.WriteFile(filepath.Join(gitDir, "folder"), []byte(dir+"\n"), 0o600)
}

func repoPath(store, dir string) string {
	return filepath.Join(store, fmt.Sprintf("%s-%08x", filepath.Base(dir), crc32.ChecksumIEEE([]byte(dir))))
}

// git runs git on dir's files with the snapshot repo, ignoring the user's
// hooks, signing and identity settings.
func git(ctx context.Context, gitDir, dir string, args ...string) (string, error) {
	base := []string{"--git-dir", gitDir, "--work-tree", dir,
		"-c", "user.name=germes", "-c", "user.email=germes@localhost", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}
	return worktree.RunGit(ctx, dir, append(base, args...)...)
}

func short(id string) string { return id[:min(len(id), 8)] }
