package tool

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// rule is one block list entry, in Claude Code's syntax: Tool(pattern).
// `*` matches any characters; `**/x` matches x in any folder.
type rule struct {
	raw     string
	tool    string
	pattern string
}

var ruleRe = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)\((.*)\)$`)

func parseRule(s string) (rule, error) {
	s = strings.TrimSpace(s)
	if m := ruleRe.FindStringSubmatch(s); m != nil {
		return rule{raw: s, tool: m[1], pattern: m[2]}, nil
	}
	if ruleRe.MatchString(s+"()") && !strings.ContainsAny(s, "()") {
		return rule{raw: s, tool: s}, nil // whole tool blocked
	}
	return rule{}, fmt.Errorf("bad block list entry %q (want Tool or Tool(pattern))", s)
}

// isBlocked checks the call's MatchArg against the rules for its tool.
func (r *Registry) isBlocked(t Tool, c Call) (string, bool) {
	for _, rl := range r.blocked {
		if rl.tool != t.Name {
			continue
		}
		if rl.pattern == "" {
			return rl.raw, true
		}
		var args map[string]any
		json.Unmarshal(c.Args, &args)
		value, _ := args[t.MatchArg].(string)
		if value == "" {
			continue
		}
		for _, candidate := range candidates(t.MatchArg, value, c.Dir) {
			if match(rl.pattern, candidate) {
				return rl.raw, true
			}
		}
	}
	return "", false
}

// candidates are the strings a pattern is checked against. A shell command is
// split at &&, ||, ; and | so `cd x && sudo y` is caught. A path is checked
// as given, by its base name, and as a lexical absolute and project-relative path.
// ponytail: plain splitting, not a shell parser; quoted separators and
// subshells like $(sudo x) slip through. Upgrade to a real parser if needed.
func candidates(arg, value, dir string) []string {
	if arg == "command" {
		var parts []string
		for _, p := range regexp.MustCompile(`&&|\|\||;|\|`).Split(value, -1) {
			if p = strings.TrimSpace(p); p != "" {
				parts = append(parts, p)
			}
		}
		return parts
	}
	out := []string{value, filepath.Base(value)}
	// Mirror tools/files.resolve here: tool is a leaf and cannot import its
	// callers. Only file_path has home expansion; search patterns are literals.
	if arg == "file_path" && strings.HasPrefix(value, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			value = filepath.Join(home, value[2:])
		}
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(dir, value)
	}
	if abs, err := filepath.Abs(value); err == nil {
		out = append(out, abs)
		if base, err := filepath.Abs(dir); err == nil {
			if rel, err := filepath.Rel(base, abs); err == nil {
				out = append(out, rel)
			}
		}
	}
	return out
}

// match reports whether s matches pattern, where * matches any characters
// (also /) and a leading **/ matches any folder.
func match(pattern, s string) bool {
	if rest, ok := strings.CutPrefix(pattern, "**/"); ok {
		return match(rest, s) || match(rest, filepath.Base(s)) || match("*/"+rest, s)
	}
	re := "(?s)^" + strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, ".*") + "$"
	ok, _ := regexp.MatchString(re, s)
	return ok
}
