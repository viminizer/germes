package tool

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// rule is one block list entry, in Claude Code's syntax: Tool(pattern).
// `*` matches any characters; `**/x` matches x in any folder.
type rule struct {
	raw     string
	tool    string
	pattern string
	matches func(string) bool // the pattern, compiled once
}

var ruleRe = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)\((.*)\)$`)

func parseRule(s string) (rule, error) {
	s = strings.TrimSpace(s)
	if m := ruleRe.FindStringSubmatch(s); m != nil {
		return rule{raw: s, tool: m[1], pattern: m[2], matches: compilePattern(m[2])}, nil
	}
	if ruleRe.MatchString(s+"()") && !strings.ContainsAny(s, "()") {
		return rule{raw: s, tool: s}, nil // whole tool blocked
	}
	return rule{}, fmt.Errorf("bad block list entry %q (want Tool or Tool(pattern))", s)
}

// isBlocked checks the call's MatchArg against the rules for its tool.
func (r *Registry) isBlocked(t Tool, c Call) (string, bool) {
	return firstMatch(r.blocked, t, c)
}

// firstMatch returns the first of rules that matches the call, or false. A
// call whose argument cannot be read safely matches (fail closed).
func firstMatch(rules []rule, t Tool, c Call) (string, bool) {
	value := func() (string, error) { return protectedArgument(c.Args, t.MatchArg) }
	return matchRules(rules, []string{t.Name, t.BlockedAs}, t.MatchArg, value, c.Dir)
}

// isReadBlocked reports whether a Read rule blocks path.
func (r *Registry) isReadBlocked(path, dir string) bool {
	_, blocked := matchRules(r.blocked, []string{"Read"}, "file_path", func() (string, error) { return path, nil }, dir)
	return blocked
}

// matchRules returns the first of rules for tools that matches the value of
// arg (read only when a rule has a pattern), or false.
func matchRules(rules []rule, tools []string, arg string, value func() (string, error), dir string) (string, bool) {
	for _, rl := range rules {
		if rl.tool == "" || !slices.Contains(tools, rl.tool) {
			continue
		}
		if rl.pattern == "" {
			return rl.raw, true
		}
		v, err := value()
		if err != nil {
			return fmt.Sprintf("%s: %v", rl.raw, err), true
		}
		if v == "" {
			continue
		}
		written, target := candidates(arg, v, dir)
		if slices.ContainsFunc(written, rl.matches) {
			return rl.raw, true
		}
		if slices.ContainsFunc(target, rl.matches) {
			// Say so: by its name alone the block looks like a mistake.
			return fmt.Sprintf("%s, through a link to %s", rl.raw, target[0]), true
		}
	}
	return "", false
}

// protectedArgument refuses ambiguous keys rather than guessing how a handler
// decodes them. Struct decoding accepts case aliases and retains strings on null;
// map decoding loses that ordering, so it cannot safely check duplicate keys.
func protectedArgument(raw json.RawMessage, arg string) (string, error) {
	if !json.Valid(raw) {
		return "", fmt.Errorf("invalid argument JSON")
	}
	d := json.NewDecoder(strings.NewReader(string(raw)))
	first, err := d.Token()
	if err != nil || first != json.Delim('{') {
		return "", fmt.Errorf("arguments must be an object")
	}
	var value string
	seen := false
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return "", fmt.Errorf("read argument key: %w", err)
		}
		key, ok := token.(string)
		if !ok {
			return "", fmt.Errorf("argument key must be a string")
		}
		var field json.RawMessage
		if err := d.Decode(&field); err != nil {
			return "", fmt.Errorf("read argument value: %w", err)
		}
		if !strings.EqualFold(key, arg) {
			continue
		}
		if key != arg || seen {
			return "", fmt.Errorf("protected argument %q must occur once with canonical spelling", arg)
		}
		seen = true
		if err := json.Unmarshal(field, &value); err != nil {
			return "", fmt.Errorf("protected argument %q must be a string", arg)
		}
	}
	return value, nil
}

var (
	shellUnquote = strings.NewReplacer(`"`, "", "'", "", `\`, "")
	shellSplit   = regexp.MustCompile("[;|&\n()`{}<>]")
	shellWord    = regexp.MustCompile(`\S+`)
)

// candidates are the strings a pattern is checked against. A shell command,
// as written and without its quotes and escapes (so `"su"do` and
// `bash -c 'sudo y'` read as sudo, and a rule with quotes still matches), is
// split at every separator, line, subshell and substitution, and each part is
// checked from every word on, so `if sudo y` and `env sudo y` are caught.
// A path is checked as given, by its base name, and as a lexical absolute and
// project-relative path; a link also by its target, returned apart.
// ponytail: text splitting, not a shell parser. It errs toward blocking
// (`git commit -m "drop sudo use"` is blocked) and cannot see commands built
// at run time ($(echo sudo) y). Use a real parser if that matters.
func candidates(arg, value, dir string) (written, target []string) {
	if arg == "command" {
		var parts []string
		for _, v := range []string{value, shellUnquote.Replace(value)} {
			for _, p := range shellSplit.Split(v, -1) {
				for _, loc := range shellWord.FindAllStringIndex(p, -1) {
					parts = append(parts, strings.TrimSpace(p[loc[0]:]))
				}
			}
		}
		return parts, nil
	}
	if arg == "url" {
		// Claude Code's WebFetch(domain:example.com) works here too.
		if u, err := url.Parse(value); err == nil && u.Hostname() != "" {
			return []string{value, "domain:" + strings.ToLower(u.Hostname())}, nil
		}
		return []string{value}, nil
	}
	out := []string{value, filepath.Base(value)}
	// Only file_path has home expansion; search patterns are literals.
	if arg == "file_path" {
		value = ResolvePath(dir, value)
	} else if !filepath.IsAbs(value) {
		value = filepath.Join(dir, value)
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return out, nil
	}
	base, _ := filepath.Abs(dir)
	out = appendPath(out, abs, base)
	// A link is checked by what it points to as well, folders on the way
	// included (issue #23). A path that does not resolve does not exist:
	// nothing can be read through it.
	if real, err := filepath.EvalSymlinks(abs); err == nil && real != abs {
		if realBase, err := filepath.EvalSymlinks(base); err == nil {
			base = realBase
		}
		target = appendPath(nil, real, base)
	}
	return out, target
}

// appendPath adds an absolute path, its base name and its path relative to
// the project folder base.
func appendPath(out []string, abs, base string) []string {
	out = append(out, abs, filepath.Base(abs))
	if rel, err := filepath.Rel(base, abs); err == nil {
		out = append(out, rel)
	}
	return out
}

// compilePattern turns a rule's pattern into a matcher: * matches any
// characters (also /) and a leading **/ matches any folder. Compiled once
// per rule: Grep checks every file it searches.
func compilePattern(pattern string) func(string) bool {
	if rest, ok := strings.CutPrefix(pattern, "**/"); ok {
		inner, nested := compilePattern(rest), compilePattern("*/"+rest)
		return func(s string) bool { return inner(s) || inner(filepath.Base(s)) || nested(s) }
	}
	return regexp.MustCompile("(?s)^" + strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, ".*") + "$").MatchString
}

// HasRules says whether a block rule or a hook names the tool in dir: then
// every call of it must come through Check, so a provider's own version of
// the tool, which Germes never sees, must not stand in for it (R47).
func (r *Registry) HasRules(name, dir string) bool {
	names := []string{name, r.tools[name].BlockedAs} // the names its calls are checked under, like firstMatch
	named := func(tool string) bool { return tool != "" && slices.Contains(names, tool) }
	for _, b := range r.blocked {
		if named(b.tool) {
			return true
		}
	}
	hooks, err := r.hooksFor(dir)
	if err != nil {
		return true // fail closed
	}
	for _, h := range hooks {
		if named(h.rule.tool) {
			return true
		}
	}
	return false
}
