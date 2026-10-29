// Package archtest checks the module map (R2): each module imports only the
// Germes modules it is allowed to. See docs/PLAN.md "Module map".
package archtest

import (
	"os/exec"
	"strings"
	"testing"
)

const mod = "github.com/viminizer/germes/"

// allowed lists, for each package, the Germes packages it may import.
// A package missing here fails the test: add it with the module map.
var allowed = map[string][]string{
	"cmd/germes":             {"*"},
	"internal/api":           {},
	"internal/config":        {},
	"internal/engine":        {},
	"internal/engine/claude": {"internal/engine"},
	"internal/store":         {"internal/api", "internal/engine"},
	"internal/session":       {"internal/api", "internal/engine"},
	"internal/daemon":        {"internal/api", "internal/session", "internal/job", "internal/router"},
	"internal/tui":           {"internal/api"},
	"internal/cli":           {"internal/api"},
	"internal/archtest":      {},
	"internal/env":           {},
	"internal/router":        {"internal/api", "internal/engine", "internal/config"},
	"internal/tool":          {},
	"internal/llm":           {},
	"internal/llm/codex":     {"internal/llm"},
	"internal/prompt":        {"internal/memory", "internal/skill"},
	"internal/engine/loop":   {"internal/engine", "internal/llm", "internal/tool", "internal/prompt", "internal/api"},
	"internal/tools/files":   {"internal/tool", "internal/env"},
	"internal/tools/shell":   {"internal/tool", "internal/env"},
}

func TestImports(t *testing.T) {
	out, err := exec.Command("go", "list", "-f", `{{.ImportPath}} {{join .Imports " "}}`, mod+"...").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		pkg := strings.TrimPrefix(fields[0], mod) // test files may import anything
		rules, ok := allowed[pkg]
		if !ok {
			t.Errorf("%s is not in the module map; add it to allowed and docs/PLAN.md", pkg)
			continue
		}
		for _, imp := range fields[1:] {
			if !strings.HasPrefix(imp, mod) {
				continue
			}
			dep := strings.TrimPrefix(imp, mod)
			if !permits(rules, dep) {
				t.Errorf("%s must not import %s (R2)", pkg, dep)
			}
		}
	}
}

func permits(rules []string, dep string) bool {
	for _, r := range rules {
		if r == "*" || r == dep {
			return true
		}
	}
	return false
}
