package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// exitedPID returns the process id of a process that already exited.
func exitedPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

func TestOnlyAGoneProcessProvesAnOldDaemonExited(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		t.Helper()
		file := filepath.Join(dir, name)
		if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return file
	}
	socket := filepath.Join(dir, "x.sock")
	for _, c := range []struct {
		name, content string
		exited        bool
	}{
		{"process gone", fmt.Sprintf("%s\n%d\n", socket, exitedPID(t)), true},
		{"process running", fmt.Sprintf("%s\n%d\n", socket, os.Getpid()), false},
		{"older version, no process id", socket + "\n", false},
		{"unreadable process id", socket + "\nabc\n", false},
		{"empty", "", false},
	} {
		if got := isExited(write(c.name, c.content)); got != c.exited {
			t.Errorf("%s: isExited = %v, want %v", c.name, got, c.exited)
		}
	}
	if !isExited(filepath.Join(dir, "no such file")) {
		t.Error("a removed registry file does not count as exited")
	}

	// removeIfExited keeps the file of a running daemon, and of one it
	// cannot read; it removes a gone one's, and an older version's (as before).
	for name, kept := range map[string]bool{"process gone": false, "process running": true, "unreadable process id": true, "older version, no process id": false} {
		removeIfExited(filepath.Join(dir, name))
		if _, err := os.Stat(filepath.Join(dir, name)); (err == nil) != kept {
			t.Errorf("%s: kept = %v, want %v", name, err == nil, kept)
		}
	}
}

func TestAllExitedReadsTheRegistry(t *testing.T) {
	dir := t.TempDir()
	o := NewOldDaemons(dir) // not loaded: its list is empty
	if !o.allExited() {
		t.Fatal("an empty registry has a running daemon")
	}
	os.WriteFile(filepath.Join(dir, "gone"), fmt.Appendf(nil, "s\n%d\n", exitedPID(t)), 0o600)
	if !o.allExited() {
		t.Fatal("a gone daemon counts as running")
	}
	os.WriteFile(filepath.Join(dir, "running"), fmt.Appendf(nil, "s\n%d\n", os.Getpid()), 0o600)
	if o.allExited() {
		t.Fatal("a running daemon in the registry, not in the empty list, was missed")
	}
}
