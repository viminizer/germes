// Package files has the file tools: Read, Write, Edit, Glob, Grep, and
// send_file. Names follow Claude Code, so one block list works for both
// engines.
package files

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/viminizer/germes/internal/api"
	"github.com/viminizer/germes/internal/text"
	"github.com/viminizer/germes/internal/tool"
)

const (
	maxReadLines = 2000
	maxMatches   = 200
	maxSendBytes = 50 << 20 // the Bot API's upload limit
)

// New returns the file tools. They share what each session has read, so
// Write refuses to overwrite a file the session has not seen as it is now.
func New() []tool.Tool {
	seen := &readLog{at: map[string]time.Time{}}
	read := func(ctx context.Context, c tool.Call, a readArgs) (string, error) {
		out, err := read(ctx, c, a)
		if err == nil {
			seen.mark(c.SessionID, resolve(c.Dir, a.FilePath))
		}
		return out, err
	}
	write := func(ctx context.Context, c tool.Call, a writeArgs) (string, error) {
		p := resolve(c.Dir, a.FilePath)
		if !seen.isCurrent(c.SessionID, p) {
			return "", fmt.Errorf("%s exists and changed since you last read it (or you never did). Read it first, then write; for a small change use Edit", a.FilePath)
		}
		out, err := write(ctx, c, a)
		if err == nil {
			seen.mark(c.SessionID, p)
			out += lint(p)
		}
		return out, err
	}
	edit := func(ctx context.Context, c tool.Call, a editArgs) (string, error) {
		out, err := edit(ctx, c, a)
		if err == nil {
			p := resolve(c.Dir, a.FilePath)
			seen.mark(c.SessionID, p)
			out += lint(p)
		}
		return out, err
	}
	return []tool.Tool{
		{Name: "Read", Toolset: "files", MatchArg: "file_path", Run: tool.NewRun(read),
			Description: "Read a text file. Returns numbered lines. Use offset and limit for big files.",
			Schema:      schema(`"file_path":{"type":"string","description":"path, absolute or relative to the project"},"offset":{"type":"integer","description":"first line, 1-based"},"limit":{"type":"integer"}`, "file_path")},
		{Name: "Write", Toolset: "files", MatchArg: "file_path", Run: tool.NewRun(write),
			Description: "Create or overwrite a file with the given content. Creates missing folders.",
			Schema:      schema(`"file_path":{"type":"string"},"content":{"type":"string"}`, "file_path", "content")},
		{Name: "Edit", Toolset: "files", MatchArg: "file_path", Run: tool.NewRun(edit),
			Description: "Replace old_string with new_string in a file. old_string must match exactly and be unique, unless replace_all is true. Read the file first.",
			Schema:      schema(`"file_path":{"type":"string"},"old_string":{"type":"string"},"new_string":{"type":"string"},"replace_all":{"type":"boolean"}`, "file_path", "old_string", "new_string")},
		{Name: "Glob", Toolset: "files", MatchArg: "pattern", Run: tool.NewRun(glob),
			Description: "Find files by name pattern, like **/*.go. Returns paths relative to the search folder.",
			Schema:      schema(`"pattern":{"type":"string"},"path":{"type":"string","description":"folder to search, default the project"}`, "pattern")},
		{Name: "Grep", Toolset: "files", MatchArg: "pattern", Run: tool.NewRun(grep),
			Description: "Search file contents with a regular expression. Returns file:line: text.",
			Schema:      schema(`"pattern":{"type":"string"},"path":{"type":"string"},"glob":{"type":"string","description":"only files matching this, like *.go"}`, "pattern")},
		{Name: api.ToolSendFile, Toolset: "files", MatchArg: "file_path", BlockedAs: "Read", Run: tool.NewRun(sendFile),
			Description: "Send a file to the user. It reaches them when they talk from Telegram; in the terminal they see only the path. Up to 50 MB.",
			Schema:      schema(`"file_path":{"type":"string","description":"path, absolute or relative to the project"}`, "file_path")},
	}
}

func schema(props string, required ...string) json.RawMessage {
	req, _ := json.Marshal(required)
	return json.RawMessage(`{"type":"object","properties":{` + props + `},"required":` + string(req) + `,"additionalProperties":false}`)
}

// resolve makes a path absolute against the project folder.
func resolve(dir, p string) string {
	if p == "" {
		return dir
	}
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[2:])
		}
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(dir, p)
}

type readArgs struct {
	FilePath      string `json:"file_path"`
	Offset, Limit int
}

type sendArgs struct {
	FilePath string `json:"file_path"`
}

// sendFile checks the file can be sent and returns its full path. It does
// not send it: Telegram watches for this tool's result and uploads the file.
func sendFile(ctx context.Context, c tool.Call, a sendArgs) (string, error) {
	p := resolve(c.Dir, a.FilePath)
	fi, err := os.Stat(p)
	switch {
	case err != nil:
		return "", err
	case fi.IsDir():
		return "", fmt.Errorf("%s is a folder; send a file", a.FilePath)
	case fi.Size() > maxSendBytes:
		return "", fmt.Errorf("%s is %d MB; Telegram takes up to 50 MB", a.FilePath, fi.Size()>>20)
	}
	return p, nil
}

func read(ctx context.Context, c tool.Call, a readArgs) (string, error) {
	f, err := os.Open(resolve(c.Dir, a.FilePath))
	if err != nil {
		return "", err
	}
	defer f.Close()
	if a.Offset < 1 {
		a.Offset = 1
	}
	if a.Limit <= 0 || a.Limit > maxReadLines {
		a.Limit = maxReadLines
	}
	var b strings.Builder
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	n, shown := 0, 0
	for sc.Scan() {
		n++
		if n < a.Offset {
			continue
		}
		if shown == a.Limit {
			fmt.Fprintf(&b, "[file continues; read more with offset %d]\n", n)
			break
		}
		fmt.Fprintf(&b, "%6d\t%s\n", n, sc.Text())
		shown++
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	if n == 0 {
		return "(empty file)", nil
	}
	return b.String(), nil
}

type writeArgs struct {
	FilePath string `json:"file_path"`
	Content  string `json:"content"`
}

func write(ctx context.Context, c tool.Call, a writeArgs) (string, error) {
	p := resolve(c.Dir, a.FilePath)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(p, []byte(a.Content), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %s (%d lines)", a.FilePath, strings.Count(a.Content, "\n")+1), nil
}

type editArgs struct {
	FilePath   string `json:"file_path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

func edit(ctx context.Context, c tool.Call, a editArgs) (string, error) {
	p := resolve(c.Dir, a.FilePath)
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	edited, count, err := text.Replace(string(b), a.OldString, a.NewString, a.ReplaceAll)
	if err != nil {
		return "", fmt.Errorf("old_string: %w", err)
	}
	info, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(p, []byte(edited), info.Mode().Perm()); err != nil {
		return "", err
	}
	return fmt.Sprintf("edited %s (%d replacement(s))", a.FilePath, count), nil
}

// skipDir is folders searches never enter.
func skipDir(name string) bool {
	return name == ".git" || name == "node_modules" || name == ".claude"
}

type globArgs struct{ Pattern, Path string }

func glob(ctx context.Context, c tool.Call, a globArgs) (string, error) {
	root := resolve(c.Dir, a.Path)
	re, err := globRegexp(a.Pattern)
	if err != nil {
		return "", err
	}
	var out []string
	err = walk(ctx, root, func(path, rel string) bool {
		if re.MatchString(rel) {
			out = append(out, rel)
		}
		return len(out) < maxMatches
	})
	if err != nil {
		return "", err
	}
	if len(out) == 0 {
		return "no files match", nil
	}
	return strings.Join(out, "\n"), nil
}

type grepArgs struct{ Pattern, Path, Glob string }

func grep(ctx context.Context, c tool.Call, a grepArgs) (string, error) {
	re, err := regexp.Compile(a.Pattern)
	if err != nil {
		return "", fmt.Errorf("bad pattern: %w", err)
	}
	var only *regexp.Regexp
	if a.Glob != "" {
		if only, err = globRegexp("**/" + a.Glob); err != nil {
			return "", err
		}
	}
	root := resolve(c.Dir, a.Path)
	var out []string
	// ponytail: Go walk and regexp, no ripgrep; fine for project-sized trees.
	err = walk(ctx, root, func(path, rel string) bool {
		if only != nil && !only.MatchString(rel) {
			return true
		}
		f, err := os.Open(path)
		if err != nil {
			return true
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for n := 1; sc.Scan(); n++ {
			line := sc.Text()
			if strings.IndexByte(line, 0) >= 0 {
				return true // binary file
			}
			if re.MatchString(line) {
				out = append(out, fmt.Sprintf("%s:%d: %s", rel, n, strings.TrimSpace(line)))
				if len(out) >= maxMatches {
					return false
				}
			}
		}
		return true
	})
	if err != nil {
		return "", err
	}
	if len(out) == 0 {
		return "no matches", nil
	}
	return strings.Join(out, "\n"), nil
}

// walk calls fn for every file under root (with its path relative to root)
// until fn returns false or ctx ends.
func walk(ctx context.Context, root string, fn func(path, rel string) bool) error {
	stop := errors.New("stop")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entry: skip it
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if path != root && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if !fn(path, rel) {
			return stop
		}
		return nil
	})
	if errors.Is(err, stop) {
		return nil
	}
	return err
}

// globRegexp turns a glob into a regexp: ** matches any folders, * and ?
// stay inside one folder.
func globRegexp(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch ch := pattern[i]; {
		case strings.HasPrefix(pattern[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 2
		case strings.HasPrefix(pattern[i:], "**"):
			b.WriteString(".*")
			i++
		case ch == '*':
			b.WriteString("[^/]*")
		case ch == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(ch)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}
