package loop

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/viminizer/germes/internal/llm"
	"github.com/viminizer/germes/internal/prompt"
	"github.com/viminizer/germes/internal/tool"
)

const (
	maxSameFailure = 2 // an identical call that failed this often is not run again this turn
	maxSameRead    = 3 // an identical read run this often gets a note
)

// checkRE finds commands that check a project: builds, tests, linters.
var checkRE = regexp.MustCompile(`\b(make|go (build|vet|test)|npm|pnpm|yarn|bun|cargo|pytest|tsc|eslint|ruff|mypy|vitest|jest|gofmt)\b`)

// guard watches one turn's tool calls. It keeps an identical failing call
// from looping, and when the model wants to stop with changed files and no
// passing check since, it sends the model back once to run the checks.
type guard struct {
	checks []string       // the project's check commands
	fails  map[string]int // call -> failures this turn
	reads  map[string]int // read-only call -> runs this turn
	edited bool           // files changed since the last passing check
	nudged bool
}

func newGuard(dir string) *guard {
	return &guard{checks: prompt.ListChecks(dir), fails: map[string]int{}, reads: map[string]int{}}
}

// refuse returns why the call must not run again, or "".
func (g *guard) refuse(c llm.ToolCall) string {
	if g.fails[signature(c)] < maxSameFailure {
		return ""
	}
	return "[Germes] Not run: this exact call failed twice already this turn. Do not retry it unchanged. " +
		"Run one diagnostic (read the file again, `pwd && ls`, look at the error), then try a different approach, " +
		"or tell the user what blocks you."
}

// note records a result and returns a note to add to it ("" = none).
func (g *guard) note(c llm.ToolCall, res tool.Result) string {
	sig := signature(c)
	if res.IsError {
		g.fails[sig]++
		if g.fails[sig] == maxSameFailure {
			return "\n\n[Germes] This exact call failed twice. Find out why before you try again, and change the approach."
		}
		return ""
	}
	switch c.Name {
	case "Write", "Edit":
		g.edited = true
	case "Bash":
		if isCheck(c.Args) {
			g.edited = false
		}
	case "Read", "Glob", "Grep":
		g.reads[sig]++
		if g.reads[sig] == maxSameRead {
			return "\n\n[Germes] You made this exact call 3 times this turn. Use what you have, or change the query."
		}
	}
	return ""
}

// finishNudge sends the model back to check its changes, once a turn, when
// the project has checks. "" = the model may finish.
func (g *guard) finishNudge() string {
	if !g.edited || g.nudged || len(g.checks) == 0 {
		return ""
	}
	g.nudged = true
	return "[Germes] You changed files but have not run a check since. Run `" + strings.Join(g.checks, "`, `") +
		"` now, read any failure, fix it, and then say what passed."
}

// isCheck reports whether a Bash call ran a check whose exit code counts: a
// check piped into another command hides its failure.
func isCheck(args json.RawMessage) bool {
	var a struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(args, &a) != nil {
		return false
	}
	piped := strings.Contains(strings.ReplaceAll(a.Command, "||", ""), "|") && !strings.Contains(a.Command, "pipefail")
	return checkRE.MatchString(a.Command) && !piped
}

func signature(c llm.ToolCall) string { return c.Name + "\x00" + string(c.Args) }
