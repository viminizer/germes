package job

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/viminizer/germes/internal/text"
)

// Workflow is a fixed order of steps a job follows each round.
type Workflow struct {
	Name  string `yaml:"name"`
	Steps []Step `yaml:"steps"`
	// StallAfter is how often a step may start again with nothing changed
	// before the agent must change its approach or name a blocker; nil = 2,
	// 0 = never.
	StallAfter *int `yaml:"stall_after"`
}

// Step is one step: an agent prompt, or a command (a shell command or a
// built-in: merge, cleanup, notify "<text>").
type Step struct {
	ID         string `yaml:"id"`
	Agent      string `yaml:"agent"`
	Run        string `yaml:"run"`
	Next       Next   `yaml:"next"`
	OnFail     string `yaml:"on_fail"`     // run steps: the step after a failed command; "" = the round fails
	MaxRetries *int   `yaml:"max_retries"` // times the step may run again in a round; nil = 3
}

// Next is where a step goes: one step id, or (agent steps) a step id per
// reply token, like {ISSUE: fix, NONE: done}.
type Next struct {
	To      string
	ByToken map[string]string
}

// UnmarshalYAML reads a step id or a token map.
func (n *Next) UnmarshalYAML(v *yaml.Node) error {
	if v.Kind == yaml.ScalarNode {
		return v.Decode(&n.To)
	}
	return v.Decode(&n.ByToken)
}

// stepDone ends the job: nothing is left to do.
const stepDone = "done"

// Built-in run steps.
const (
	builtinMerge   = "merge"
	builtinCleanup = "cleanup"
	builtinNotify  = "notify"
)

var tokenRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// LoadWorkflow reads the workflow called name from the first folder that has
// it (<name>.yaml or .yml), and checks it.
func LoadWorkflow(dirs []string, name string) (Workflow, error) {
	for _, dir := range dirs {
		for _, ext := range []string{".yaml", ".yml"} {
			b, err := os.ReadFile(filepath.Join(dir, name+ext))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return Workflow{}, err
			}
			return parseWorkflow(b)
		}
	}
	return Workflow{}, fmt.Errorf("no workflow %q in %s", name, strings.Join(dirs, ", "))
}

// ListWorkflows lists the workflow names in dirs; the first folder wins.
func ListWorkflows(dirs []string) []string {
	var names []string
	for _, dir := range dirs {
		for _, pattern := range []string{"*.yaml", "*.yml"} {
			files, _ := filepath.Glob(filepath.Join(dir, pattern))
			for _, f := range files {
				name := strings.TrimSuffix(filepath.Base(f), filepath.Ext(f))
				if !slices.Contains(names, name) {
					names = append(names, name)
				}
			}
		}
	}
	sort.Strings(names)
	return names
}

// parseWorkflow reads and checks a workflow file.
func parseWorkflow(b []byte) (Workflow, error) {
	var w Workflow
	if err := yaml.Unmarshal(b, &w); err != nil {
		return w, fmt.Errorf("bad YAML: %w", err)
	}
	return w, w.check()
}

// check finds every mistake at once: unknown or missing step ids, a step
// with no kind or two, bad tokens.
func (w Workflow) check() error {
	var errs []string
	add := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }
	if len(w.Steps) == 0 {
		add("no steps")
	}
	ids := map[string]bool{stepDone: true}
	for i, s := range w.Steps {
		switch {
		case s.ID == "":
			add("step %d has no id", i+1)
		case s.ID == stepDone:
			add(`step id "done" is reserved: it ends the job`)
		case ids[s.ID]:
			add("step id %q is used twice", s.ID)
		}
		ids[s.ID] = true
	}
	known := func(id, where string) {
		if !ids[id] {
			add("%s: unknown step %q", where, id)
		}
	}
	for _, s := range w.Steps {
		where := "step " + s.ID
		switch {
		case (s.Agent == "") == (s.Run == ""):
			add("%s: needs exactly one of agent or run", where)
		case s.Next.To == "" && len(s.Next.ByToken) == 0:
			add("%s: no next", where)
		case s.Run != "" && len(s.Next.ByToken) > 0:
			add("%s: a run step goes on by exit code: next is one step id, with on_fail for failures", where)
		case s.Agent != "" && s.OnFail != "":
			add("%s: on_fail is only for run steps", where)
		}
		if s.Next.To != "" {
			known(s.Next.To, where+" next")
		}
		for tok, to := range s.Next.ByToken {
			if !tokenRE.MatchString(tok) {
				add("%s: token %q must be upper case, like ISSUE", where, tok)
			}
			known(to, where+" next "+tok)
		}
		if s.OnFail != "" {
			known(s.OnFail, where+" on_fail")
		}
		if s.MaxRetries != nil && *s.MaxRetries < 0 {
			add("%s: max_retries must not be negative", where)
		}
		if strings.TrimSpace(s.Run) == builtinNotify {
			add(`%s: notify needs a text, like notify "tests pass"`, where)
		}
	}
	if w.StallAfter != nil && *w.StallAfter < 0 {
		add("stall_after must not be negative")
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// stallAfter is how often a step may repeat with nothing changed.
func (w Workflow) stallAfter() int {
	if w.StallAfter == nil {
		return 2
	}
	return *w.StallAfter
}

// step returns the step with id.
func (w Workflow) step(id string) (Step, bool) {
	for _, s := range w.Steps {
		if s.ID == id {
			return s, true
		}
	}
	return Step{}, false
}

// tries is how often the step may run in one round.
func (s Step) tries() int {
	if s.MaxRetries == nil {
		return 1 + 3
	}
	return 1 + *s.MaxRetries
}

// tokens lists the reply tokens an agent step accepts.
func (s Step) tokens() []string {
	var toks []string
	for tok := range s.Next.ByToken {
		toks = append(toks, tok)
	}
	sort.Strings(toks)
	return toks
}

var resultRE = regexp.MustCompile(`^RESULT:\s*([A-Z][A-Z0-9_]*)\s*(.*)$`)

// parseResult reads the reply's last `RESULT: TOKEN [value]` line.
func parseResult(reply string) (token, value string, ok bool) {
	lines := strings.Split(strings.TrimSpace(reply), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.Trim(strings.TrimSpace(lines[i]), "*`_ ")
		if m := resultRE.FindStringSubmatch(line); m != nil {
			return m[1], strings.TrimSpace(m[2]), true
		}
	}
	return "", "", false
}

var valueRE = regexp.MustCompile(`\{([A-Z][A-Z0-9_]*)\}`)

// fill puts the values into text: {ISSUE} becomes the ISSUE value.
func fill(text string, values map[string]string) string {
	return valueRE.ReplaceAllStringFunc(text, func(m string) string {
		if v, ok := values[m[1:len(m)-1]]; ok {
			return v
		}
		return m
	})
}

// fillCommand puts the values into a shell command as variables set above
// it: {ISSUE} becomes a reference to GERMES_VALUE_ISSUE, quoted for where it
// stands (bare, in "..." or in '...'). The shell never reads a value as
// code, so a reply cannot run a command wherever its placeholder is (#24).
// ponytail: the quote tracking knows quotes and backslashes, not heredocs; a
// bare placeholder in one prints with its quotes, but still runs nothing.
func fillCommand(cmd string, values map[string]string) string {
	var head, body strings.Builder
	set := map[string]bool{}
	last := 0
	for _, m := range valueRE.FindAllStringSubmatchIndex(cmd, -1) {
		body.WriteString(cmd[last:m[0]])
		last = m[1]
		name := cmd[m[2]:m[3]]
		v, ok := values[name]
		if !ok {
			body.WriteString(cmd[m[0]:m[1]])
			continue
		}
		if !set[name] {
			set[name] = true
			fmt.Fprintf(&head, "GERMES_VALUE_%s=%s\n", name, text.QuoteShell(v))
		}
		ref := "${GERMES_VALUE_" + name + "}"
		switch quoteAfter(cmd[:m[0]]) {
		case 0:
			ref = `"` + ref + `"`
		case '\'':
			ref = `'"` + ref + `"'`
		}
		body.WriteString(ref)
	}
	body.WriteString(cmd[last:])
	return head.String() + body.String()
}

// quoteAfter returns the quote (', " or 0 for none) the shell is inside
// after reading the command s.
func quoteAfter(s string) byte {
	quote := byte(0)
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			}
		case c == '\\':
			i++ // an escaped character is not a quote
		case quote == 0 && c == '#' && (i == 0 || strings.ContainsRune(" \t\n;", rune(s[i-1]))):
			// A comment, where "don't" opens no quote.
			if n := strings.IndexByte(s[i:], '\n'); n >= 0 {
				i += n
			} else {
				i = len(s)
			}
		case c == quote:
			quote = 0
		case quote == 0 && (c == '\'' || c == '"'):
			quote = c
		}
	}
	return quote
}
