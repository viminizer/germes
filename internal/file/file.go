// Package file saves files safely, deletes old ones, and tells whether a
// path is inside a folder.
package file

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Save replaces path with a complete file with the requested permissions.
// Concurrent saves publish whole files; callers must serialize read-modify-write
// operations themselves. Rename does not guarantee durability after power loss.
func Save(path string, data []byte, perm fs.FileMode) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), ".germes-save-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, os.Remove(f.Name()))
		}
	}()
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(perm)
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// DeleteOld deletes the files in dir last changed before before, and returns
// how many it deleted. Folders inside dir are left alone; a missing dir has
// nothing to delete.
func DeleteOld(dir string, before time.Time) (int, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || info.IsDir() || !info.ModTime().Before(before) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// IsInside reports whether path is dir or under it. Both must be absolute
// or both relative to the same folder; symlinks are not followed.
func IsInside(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
