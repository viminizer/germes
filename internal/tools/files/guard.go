package files

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// readLog remembers when each session last saw each file (read or wrote it).
type readLog struct {
	mu sync.Mutex
	at map[string]time.Time // session + path -> the file's mod time then
}

func (l *readLog) mark(sessionID, path string) {
	fi, err := os.Stat(path)
	if err != nil {
		return
	}
	l.mu.Lock()
	l.at[sessionID+"\x00"+path] = fi.ModTime()
	l.mu.Unlock()
}

// isCurrent reports whether writing path loses nothing the session has not
// seen: the file is new, or unchanged since the session saw it.
func (l *readLog) isCurrent(sessionID, path string) bool {
	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	if err != nil || fi.IsDir() {
		return true // the write itself reports it
	}
	l.mu.Lock()
	at, ok := l.at[sessionID+"\x00"+path]
	l.mu.Unlock()
	return ok && at.Equal(fi.ModTime())
}

// lintTimeout bounds a syntax check after an edit.
const lintTimeout = 10 * time.Second

// lint checks a changed file's syntax, when a checker for its language is
// installed. It returns a note for the tool result, or "" when it is fine.
func lint(path string) string {
	var cmd []string
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		cmd = []string{"gofmt", "-e", "-l", path}
	case ".py":
		cmd = []string{"python3", "-m", "py_compile", path}
	case ".js", ".mjs", ".cjs":
		cmd = []string{"node", "--check", path}
	case ".json":
		b, err := os.ReadFile(path)
		if err == nil && !json.Valid(b) {
			return "\n\nsyntax check: not valid JSON"
		}
		return ""
	default:
		return ""
	}
	if _, err := exec.LookPath(cmd[0]); err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), lintTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, cmd[0], cmd[1:]...).CombinedOutput()
	if err == nil {
		return ""
	}
	return "\n\nsyntax check failed (" + cmd[0] + "):\n" + strings.TrimSpace(string(out))
}
