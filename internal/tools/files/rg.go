package files

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// maxColumns cuts long lines (minified files) in Grep results.
const maxColumns = 300

// rgPath is ripgrep's path, or "" when it is not installed. With it,
// searches respect .gitignore and use every core; without it they walk the
// folder in Go (issue #15: download it when missing).
func rgPath() string {
	p, _ := exec.LookPath("rg")
	return p
}

// rgRun runs ripgrep in root (a folder, or a file) and calls line for each
// line it prints. It returns what ripgrep printed on stderr when it failed
// without output.
func rgRun(ctx context.Context, rg, root string, args []string, line func(string)) error {
	dir, target := root, []string{}
	if fi, err := os.Stat(root); err == nil && !fi.IsDir() {
		dir, target = filepath.Dir(root), []string{filepath.Base(root)}
	}
	args = append([]string{"--hidden", "--color", "never"}, args...)
	for _, d := range skipDirs {
		args = append(args, "--glob", "!"+d)
	}
	cmd := exec.CommandContext(ctx, rg, append(append(args, "--"), target...)...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	printed := false
	for sc.Scan() {
		printed = true
		line(sc.Text())
	}
	err = cmd.Wait()
	var exit *exec.ExitError
	switch {
	case err == nil, errors.As(err, &exit) && (exit.ExitCode() == 1 || printed):
		return nil // 1 = no match; 2 with output = some files were unreadable
	case ctx.Err() != nil:
		return ctx.Err()
	}
	return fmt.Errorf("rg: %s", strings.TrimSpace(stderr.String()))
}

// rgGrep searches with ripgrep. It returns the first max matches as
// "file:line: text" and how many there are in all.
func rgGrep(ctx context.Context, rg, root string, a grepArgs, max int) ([]string, int, error) {
	// --null ends the path with a NUL: a path may hold a colon.
	args := []string{"--line-number", "--no-heading", "--with-filename", "--null",
		"--max-columns", fmt.Sprint(maxColumns), "--max-columns-preview", "--regexp", a.Pattern}
	if a.Glob != "" {
		args = append(args, "--glob", a.Glob)
	}
	var out []string
	total := 0
	err := rgRun(ctx, rg, root, args, func(l string) {
		total++
		if len(out) < max {
			if path, rest, ok := strings.Cut(l, "\x00"); ok {
				if n, text, ok := strings.Cut(rest, ":"); ok {
					l = fmt.Sprintf("%s:%s: %s", path, n, strings.TrimSpace(text))
				}
			}
			out = append(out, l)
		}
	})
	return out, total, err
}

// rgFiles lists the files under root that git does not ignore, sorted.
func rgFiles(ctx context.Context, rg, root string) ([]string, error) {
	var out []string
	err := rgRun(ctx, rg, root, []string{"--files", "--sort", "path"}, func(l string) { out = append(out, l) })
	return out, err
}
