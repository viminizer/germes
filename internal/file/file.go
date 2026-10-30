// Package file saves files safely and deletes old ones.
package file

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Save writes data to a temp file next to path, then renames it over path,
// so a crash never leaves a half-written file.
func Save(path string, data []byte, perm fs.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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
