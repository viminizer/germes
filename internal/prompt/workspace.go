package prompt

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// ListChecks lists the commands that check the project in dir (build,
// tests, lint), found from its files. Empty: none found.
func ListChecks(dir string) []string {
	var checks []string
	has := func(name string) bool { _, err := os.Stat(filepath.Join(dir, name)); return err == nil }
	if targets := makeTargets(dir); len(targets) > 0 {
		for _, t := range []string{"check", "test", "lint", "build"} {
			if slices.Contains(targets, t) {
				checks = append(checks, "make "+t)
			}
		}
		if slices.Contains(checks, "make check") {
			return []string{"make check"} // the project's own all-in-one check
		}
	}
	if has("go.mod") {
		checks = append(checks, "go build ./... && go vet ./... && go test ./...")
	}
	if scripts := npmScripts(dir); len(scripts) > 0 {
		run := "npm run"
		switch {
		case has("pnpm-lock.yaml"):
			run = "pnpm"
		case has("yarn.lock"):
			run = "yarn"
		case has("bun.lockb"), has("bun.lock"):
			run = "bun run"
		}
		for _, s := range []string{"typecheck", "lint", "test", "build"} {
			if slices.Contains(scripts, s) {
				checks = append(checks, run+" "+s)
			}
		}
	}
	if has("Cargo.toml") {
		checks = append(checks, "cargo build && cargo test")
	}
	if has("pyproject.toml") || has("pytest.ini") || has("setup.py") {
		checks = append(checks, "pytest")
	}
	return checks
}

var makeTargetRE = regexp.MustCompile(`(?m)^([a-zA-Z][\w-]*):`)

func makeTargets(dir string) []string {
	b, err := os.ReadFile(filepath.Join(dir, "Makefile"))
	if err != nil {
		return nil
	}
	var targets []string
	for _, m := range makeTargetRE.FindAllStringSubmatch(string(b), -1) {
		targets = append(targets, m[1])
	}
	return targets
}

func npmScripts(dir string) []string {
	b, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return nil
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(b, &pkg) != nil {
		return nil
	}
	var names []string
	for n := range pkg.Scripts {
		names = append(names, n)
	}
	return names
}

// workspace is a snapshot of the folder for the prompt: its git state and
// the commands that check it. "" when the folder is not a project.
func workspace(dir string) string {
	checks := ListChecks(dir)
	git := gitState(dir)
	if len(checks) == 0 && git == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Workspace\n\nA snapshot from the start of the session; run `git status` before you rely on it.\n")
	if git != "" {
		b.WriteString(git + "\n")
	}
	if len(checks) > 0 {
		fmt.Fprintf(&b, "Checks (run them after you change files): `%s`\n", strings.Join(checks, "`, `"))
	}
	return strings.TrimSpace(b.String())
}

// gitState says the branch and how many files are changed, or "".
func gitState(dir string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	branch, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return ""
	}
	status, err := exec.CommandContext(ctx, "git", "-C", dir, "status", "--porcelain").Output()
	if err != nil {
		return ""
	}
	changed := 0
	if s := strings.TrimSpace(string(status)); s != "" {
		changed = strings.Count(s, "\n") + 1
	}
	return fmt.Sprintf("Git: branch %s, %d changed files.", strings.TrimSpace(string(branch)), changed)
}
