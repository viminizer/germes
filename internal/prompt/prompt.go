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

	"github.com/viminizer/germes/internal/memory"
	"github.com/viminizer/germes/internal/skill"
)

//go:embed soul.md
var defaultSoul string

//go:embed guidance.md
var guidance string

//go:embed germes.md
var germesInfo string

const (
	maxProjectFile = 20_000 // characters of AGENTS.md / CLAUDE.md in the prompt
	maxSkillDesc   = 200    // characters of each skill description in the index
)

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

// MemoryStore reads the memory files.
type MemoryStore interface {
	Load(ctx context.Context) (memory.Snapshot, error)
}

// SkillStore lists the skills.
type SkillStore interface {
	List(ctx context.Context) ([]skill.Summary, error)
}

// Builder builds prompts.
type Builder struct {
	home   string // ~/.germes: SOUL.md may live here
	memory MemoryStore
	skills SkillStore
}

// NewBuilder creates a builder.
func NewBuilder(home string, memory MemoryStore, skills SkillStore) *Builder {
	return &Builder{home: home, memory: memory, skills: skills}
}

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
	skills, err := b.skillIndex(ctx)
	if err != nil {
		return Prompt{}, err
	}
	mem, err := b.memoryBlock(ctx)
	if err != nil {
		return Prompt{}, err
	}
	germes := strings.ReplaceAll(strings.TrimSpace(germesInfo), "{{HOME}}", b.home)

	// Stable first: identity, how to work, Germes itself. Then the project,
	// then what changes more often: skills, memory, today.
	system := []string{strings.TrimSpace(soul), strings.TrimSpace(guidance), germes}
	extra := []string{germes} // Claude has its own identity and guidance
	if project != "" {
		system = append(system, project)
		if !claudeMD { // Claude Code reads CLAUDE.md itself
			extra = append(extra, project)
		}
	}
	for _, part := range []string{skills, mem} {
		if part != "" {
			system = append(system, part)
			extra = append(extra, part)
		}
	}
	system = append(system, fmt.Sprintf("# Environment\n\nToday: %s\nWorking folder: %s", time.Now().Format("2006-01-02"), dir))
	return Prompt{System: strings.Join(system, "\n\n"), Extra: strings.Join(extra, "\n\n")}, nil
}

func (b *Builder) skillIndex(ctx context.Context) (string, error) {
	list, err := b.skills.List(ctx)
	if err != nil || len(list) == 0 {
		return "", err
	}
	var s strings.Builder
	s.WriteString("# Skills\n\nIf a skill below matches the task, you MUST load it before you start (skill_view, or read its SKILL.md). " +
		"After a hard task, save what you learned as a skill (skill_save, or write a SKILL.md into the Germes skills folder).\n")
	for _, sk := range list {
		desc := sk.Description
		if r := []rune(desc); len(r) > maxSkillDesc {
			desc = string(r[:maxSkillDesc]) + "…"
		}
		fmt.Fprintf(&s, "\n- %s: %s (%s)", sk.Name, desc, sk.Dir)
	}
	return s.String(), nil
}

func (b *Builder) memoryBlock(ctx context.Context) (string, error) {
	m, err := b.memory.Load(ctx)
	if err != nil || (m.Agent == "" && m.User == "") {
		return "", err
	}
	var s strings.Builder
	s.WriteString("# Memory (saved in earlier sessions; keep it up to date with the memory tool)\n")
	if m.Agent != "" {
		fmt.Fprintf(&s, "\n## Your notes (MEMORY.md)\n\n%s\n", m.Agent)
	}
	if m.User != "" {
		fmt.Fprintf(&s, "\n## About the user (USER.md)\n\n%s\n", m.User)
	}
	return strings.TrimSpace(s.String()), nil
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
