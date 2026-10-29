// Package file saves files safely.
package file

import (
	"io/fs"
	"os"
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
