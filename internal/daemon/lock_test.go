package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGetLockHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.lock")
	if pid, err := GetLockHolder(path); pid != 0 || err != nil {
		t.Fatalf("no lock file: %d %v", pid, err)
	}
	unlock, err := lock(path)
	if err != nil {
		t.Fatal(err)
	}
	if pid, err := GetLockHolder(path); pid != os.Getpid() || err != nil {
		t.Fatalf("held: %d %v", pid, err)
	}
	unlock()
	if pid, err := GetLockHolder(path); pid != 0 || err != nil {
		t.Fatalf("released: %d %v", pid, err)
	}
}
