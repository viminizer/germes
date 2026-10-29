// Package prompt builds the system prompt for a session. Layers go from
// stable to changing, so the provider's prompt cache keeps working.
package prompt

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

//go:embed soul.md
var defaultSoul string

//go:embed guidance.md
var guidance string

// maxProjectFile is how much of AGENTS.md / CLAUDE.md goes into the prompt.
const maxProjectFile = 20_000

// Role is who the session is for.
type Role string

// Roles.
const (
	RoleProject  Role = "project"
	RoleOverseer Role = "overseer"
)

// Prompt is the built prompt.
type Prompt struct {
	System string // the full prompt, for engine/loop
	Extra  string // only the Germes parts, for claude --append-system-prompt
}

// Builder builds prompts.
type Builder struct {
	home string // ~/.germes: SOUL.md may live here
}

// NewBuilder creates a builder.
func NewBuilder(home string) *Builder { return &Builder{home: home} }

// Create builds the prompt for a session working in dir.
func (b *Builder) Create(ctx context.Context, dir string, role Role) (Prompt, error) {
	soul, err := readOptional(filepath.Join(b.home, "SOUL.md"))
	if err != nil {
		return Prompt{}, err
	}
	if soul == "" {
		soul = defaultSoul
	}
	project, claudeMD, err := projectContext(dir)
	if err != nil {
		return Prompt{}, err
	}

	// 1. identity, 2. tool guidance (stable)
	parts := []string{strings.TrimSpace(soul), strings.TrimSpace(guidance)}
	var extra []string
	// 3. project context
	if project != "" {
		parts = append(parts, project)
		if !claudeMD { // Claude Code reads CLAUDE.md itself
			extra = append(extra, project)
		}
	}
	// 4. memory and skills come in milestone 4. 5. changing facts last.
	facts := fmt.Sprintf("# Environment\n\nToday: %s\nWorking folder: %s", time.Now().Format("2006-01-02"), dir)
	parts = append(parts, facts)
	return Prompt{System: strings.Join(parts, "\n\n"), Extra: strings.Join(extra, "\n\n")}, nil
}

// projectContext reads AGENTS.md, or CLAUDE.md when there is no AGENTS.md
// (only one type, like Hermes). It reports whether the file was CLAUDE.md.
func projectContext(dir string) (string, bool, error) {
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		text, err := readOptional(filepath.Join(dir, name))
		if err != nil {
			return "", false, err
		}
		if text == "" {
			continue
		}
		if r := []rune(text); len(r) > maxProjectFile {
			text = string(r[:maxProjectFile]) + "\n\n[... cut ...]"
		}
		return fmt.Sprintf("# Project instructions (%s)\n\n%s", name, strings.TrimSpace(text)), name == "CLAUDE.md", nil
	}
	return "", false, nil
}

func readOptional(path string) (string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return string(b), nil
}
