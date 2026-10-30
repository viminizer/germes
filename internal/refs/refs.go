// Package refs expands @ references in a message, like Hermes
// (hermes/agent/context_references.py): @file:path[:start-end],
// @folder:path, @diff, @staged, @git:"args", @url:address. Files and git go
// through the tool registry, so the block list applies to them too.
package refs

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/viminizer/germes/internal/text"
	"github.com/viminizer/germes/internal/tool"
)

const (
	maxPerRef = 50_000  // characters of one reference
	maxTotal  = 120_000 // characters of all references in one message
)

var refRe = regexp.MustCompile(`(?:^|\s)@(file|folder|git|url):("[^"]*"|\S+)|(?:^|\s)@(diff|staged)\b`)

// Ref is one reference found in a message.
type Ref struct {
	Kind  string // file, folder, diff, staged, git, url
	Value string // path, git args or address ("" for diff and staged)
}

// String is the reference as the user writes it.
func (r Ref) String() string {
	if r.Value == "" {
		return "@" + r.Kind
	}
	if strings.ContainsAny(r.Value, " \t") {
		return "@" + r.Kind + `:"` + r.Value + `"`
	}
	return "@" + r.Kind + ":" + r.Value
}

// List returns the references in msg, in order, without duplicates.
func List(msg string) []Ref {
	var out []Ref
	seen := map[Ref]bool{}
	for _, m := range refRe.FindAllStringSubmatch(msg, -1) {
		r := Ref{Kind: m[1], Value: strings.Trim(m[2], `"`)}
		if m[3] != "" {
			r = Ref{Kind: m[3]}
		}
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	return out
}

// Expander expands references with the tool registry.
type Expander struct {
	tools *tool.Registry
}

// New creates an expander.
func New(tools *tool.Registry) *Expander {
	return &Expander{tools: tools}
}

// Expand returns the text of every reference in msg, each under a
// "--- @kind:value ---" header, or "" when there are none. A reference that
// fails shows its error instead of its text.
func (e *Expander) Expand(ctx context.Context, msg, sessionID, dir string) string {
	var b strings.Builder
	for _, r := range List(msg) {
		body := e.expand(ctx, r, sessionID, dir)
		if left := maxTotal - b.Len(); left <= 0 {
			fmt.Fprintf(&b, "--- %s ---\n[left out: the references are too long]\n\n", r.String())
			continue
		} else if len(body) > left {
			body = text.Shorten(body, left)
		}
		fmt.Fprintf(&b, "--- %s ---\n%s\n\n", r.String(), strings.TrimRight(body, "\n"))
	}
	return strings.TrimSpace(b.String())
}

var lineRange = regexp.MustCompile(`^(.*):(\d+)(?:-(\d+))?$`)

func (e *Expander) expand(ctx context.Context, r Ref, sessionID, dir string) string {
	run := func(name string, args any) string {
		raw, _ := json.Marshal(args)
		res := e.tools.Run(ctx, name, tool.Call{SessionID: sessionID, Dir: dir, Args: raw})
		if res.IsError {
			return "[error: " + res.Output + "]"
		}
		return text.Shorten(res.Output, maxPerRef)
	}
	switch r.Kind {
	case "file":
		args := map[string]any{"file_path": r.Value}
		if m := lineRange.FindStringSubmatch(r.Value); m != nil {
			start, _ := strconv.Atoi(m[2])
			end := start
			if m[3] != "" {
				end, _ = strconv.Atoi(m[3])
			}
			args = map[string]any{"file_path": m[1], "offset": start, "limit": max(end-start+1, 1)}
		}
		return run("Read", args)
	case "folder":
		return run("Glob", map[string]any{"pattern": "**/*", "path": r.Value})
	case "diff":
		return run("Bash", map[string]any{"command": "git diff"})
	case "staged":
		return run("Bash", map[string]any{"command": "git diff --cached"})
	case "git":
		return run("Bash", map[string]any{"command": "git " + r.Value})
	case "url":
		url := r.Value
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			url = "https://" + url
		}
		return run("WebFetch", map[string]any{"url": url})
	}
	return "[error: unknown reference]"
}
