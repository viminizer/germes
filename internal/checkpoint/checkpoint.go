// Package checkpoint saves a folder's files before each turn and puts them
// back on request (rollback). Snapshots go into a hidden git repo per folder
// under the store, apart from the project's own git: its history, branches
// and index are never touched. The folder's .gitignore applies.
package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
	lock, err := lockRepo(ctx, repoPath(store, dir), true)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	return create(ctx, store, dir, label)
}

func create(ctx context.Context, store, dir, label string) (string, error) {
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
	lock, err := lockRepo(ctx, repoPath(store, dir), true)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
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
	lock, err := lockRepo(ctx, repoPath(store, dir), true)
	if err != nil {
		return err
	}
	defer lock.Close()
	if _, err := create(ctx, store, dir, "before rollback to "+short(id)); err != nil {
		return err
	}
	gitDir := repoPath(store, dir)
	if _, err := git(ctx, gitDir, dir, "read-tree", "-u", "--reset", id); err != nil {
		return err
	}
	_, err = git(ctx, gitDir, dir, "commit", "-q", "--allow-empty", "--no-verify", "-m", "rollback to "+short(id))
	return err
}

// Prune deletes the snapshot repos of folders that are gone, and of
// folders not snapshot since before. It returns how many it deleted.
func Prune(store string, before time.Time) (int, error) {
	entries, err := os.ReadDir(store)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	removed := 0
	var failures []error
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		gitDir := filepath.Join(store, e.Name())
		n, err := pruneRepo(gitDir, store, before)
		removed += n
		if err != nil {
			failures = append(failures, err)
		}
	}
	return removed, errors.Join(failures...)
}

func pruneRepo(gitDir, store string, before time.Time) (int, error) {
	lock, err := lockRepo(context.Background(), gitDir, false)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return 0, nil // active snapshot, reader or rollback; retry next cleanup
	}
	if err != nil {
		return 0, err
	}
	defer lock.Close()
	folder, err := os.ReadFile(filepath.Join(gitDir, "folder"))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil // not a snapshot repo: leave it
	}
	if err != nil {
		return 0, fmt.Errorf("read snapshot folder %s: %w", gitDir, err)
	}
	// open appends one newline; whitespace can be part of the path.
	dir := strings.TrimSuffix(string(folder), "\n")
	if !filepath.IsAbs(dir) || repoPath(store, dir) != gitDir {
		return 0, fmt.Errorf("invalid snapshot folder record in %s", gitDir)
	}
	_, err = os.Stat(dir)
	gone := errors.Is(err, os.ErrNotExist)
	if err != nil && !gone {
		return 0, fmt.Errorf("stat snapshot project %s: %w", dir, err)
	}
	if !gone {
		// Neither missing metadata nor an old index alone proves inactivity.
		used, err := os.Lstat(filepath.Join(gitDir, "index"))
		if err != nil {
			return 0, fmt.Errorf("stat snapshot index %s: %w", gitDir, err)
		}
		if !used.Mode().IsRegular() {
			return 0, fmt.Errorf("invalid snapshot index in %s", gitDir)
		}
		if !used.ModTime().Before(before) {
			return 0, nil
		}
		old, err := refsBefore(gitDir, before)
		if err != nil || !old {
			return 0, err
		}
	}
	if err := os.RemoveAll(gitDir); err != nil {
		return 0, fmt.Errorf("delete snapshot repo %s: %w", gitDir, err)
	}
	return 1, nil
}

// Git commits always update HEAD or its ref, even when add leaves the index
// unchanged. Inspect both loose and packed refs for existing shadow repos.
func refsBefore(gitDir string, before time.Time) (bool, error) {
	old := true
	for _, name := range []string{"HEAD", "refs", "packed-refs"} {
		err := filepath.WalkDir(filepath.Join(gitDir, name), func(path string, entry fs.DirEntry, err error) error {
			if name == "packed-refs" && errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if name != "refs" {
					return fmt.Errorf("invalid snapshot ref %s", path)
				}
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("invalid snapshot ref %s", path)
			}
			old = old && info.ModTime().Before(before)
			return nil
		})
		if err != nil {
			return false, fmt.Errorf("inspect snapshot activity %s: %w", gitDir, err)
		}
	}
	return old, nil
}

// The sidecar inode must outlive repo deletion: removing a lock file would
// allow different processes to hold locks on different inodes for one repo.
func lockRepo(ctx context.Context, gitDir string, wait bool) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(gitDir), 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(gitDir+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open snapshot lock: %w", err)
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, errors.Join(err, lock.Close())
		}
		err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return lock, nil
		}
		if !wait || !errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.Join(fmt.Errorf("lock snapshot repo %s: %w", gitDir, err), lock.Close())
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(ctx.Err(), lock.Close())
		case <-time.After(10 * time.Millisecond):
		}
	}
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
