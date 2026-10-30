// Package skill reads and writes skills: folders with a SKILL.md
// (agentskills.io format, like Hermes and Claude Code).
package skill

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/viminizer/germes/internal/file"
	"github.com/viminizer/germes/internal/text"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	maxDescription = 1024
	maxSkill       = 100_000 // characters of a SKILL.md (same as Hermes)
)

// ErrRename means a change would rename a skill.
var ErrRename = errors.New("a skill's name cannot change; save it under the new name instead")

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// Summary is what the prompt lists about a skill.
type Summary struct {
	Name        string
	Description string
	Dir         string // the skill's folder
}

// Skill is a full skill.
type Skill struct {
	Summary
	Body  string   // SKILL.md without the frontmatter
	Files []string // extra files in the folder, relative to Dir
}

// Store reads skills from several folders and writes to one.
type Store struct {
	writeDir string   // ~/.germes/skills: the only place Germes writes
	readDirs []string // e.g. ~/.claude/skills, read-only
}

// New creates the store. The same name in several folders: writeDir wins.
func New(writeDir string, readDirs ...string) *Store {
	return &Store{writeDir: writeDir, readDirs: readDirs}
}

// List returns every skill, sorted by name. Broken skills are skipped.
func (s *Store) List(ctx context.Context) ([]Summary, error) {
	seen := map[string]bool{}
	var out []Summary
	for _, dir := range append([]string{s.writeDir}, s.readDirs...) {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if !e.IsDir() || seen[e.Name()] {
				continue
			}
			sum, _, err := read(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			seen[e.Name()] = true
			out = append(out, sum)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Load reads one skill by name.
func (s *Store) Load(ctx context.Context, name string) (Skill, error) {
	for _, dir := range append([]string{s.writeDir}, s.readDirs...) {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(filepath.Join(path, "SKILL.md")); err != nil {
			continue
		}
		sum, body, err := read(path)
		if err != nil {
			return Skill{}, err
		}
		sk := Skill{Summary: sum, Body: body}
		err = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && d.Name() != "SKILL.md" {
				rel, err := filepath.Rel(path, p)
				if err != nil {
					return err
				}
				sk.Files = append(sk.Files, rel)
			}
			return nil
		})
		if err != nil {
			return Skill{}, fmt.Errorf("skill %s: list its files: %w", name, err)
		}
		return sk, nil
	}
	return Skill{}, fmt.Errorf("no skill named %q", name)
}

// Save creates or updates a skill in the write folder.
func (s *Store) Save(ctx context.Context, sk Skill) error {
	if !nameRe.MatchString(sk.Name) {
		return fmt.Errorf("skill name %q: use lowercase letters, digits and dashes", sk.Name)
	}
	desc := strings.TrimSpace(sk.Description)
	if desc == "" || len(desc) > maxDescription || strings.Contains(desc, "\n") {
		return errors.New("skill description: one line, 1 to 1024 characters, saying when to use it")
	}
	if strings.TrimSpace(sk.Body) == "" {
		return errors.New("skill body is empty")
	}
	front, err := yaml.Marshal(map[string]string{"name": sk.Name, "description": desc})
	if err != nil {
		return err
	}
	dir := filepath.Join(s.writeDir, sk.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	text := "---\n" + string(front) + "---\n\n" + strings.TrimSpace(sk.Body) + "\n"
	if n := len([]rune(text)); n > maxSkill {
		return fmt.Errorf("SKILL.md would be %d characters, the limit is %d: move details into extra files in the skill's folder", n, maxSkill)
	}
	return file.Save(filepath.Join(dir, "SKILL.md"), []byte(text), 0o644)
}

// Update replaces the one place old appears in a skill's SKILL.md with
// repl, and saves it with the same checks as Save.
func (s *Store) Update(ctx context.Context, name, old, repl string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("bad skill name %q", name)
	}
	dir := filepath.Join(s.writeDir, name)
	b, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return fmt.Errorf("no skill %q in %s (other folders are read-only)", name, s.writeDir)
	}
	patched, _, err := text.Replace(string(b), old, repl, false)
	if err != nil {
		return fmt.Errorf("skill %s: %w", name, err)
	}
	sum, body, err := parse([]byte(patched), dir)
	if err != nil {
		return err
	}
	if sum.Name != name {
		return ErrRename
	}
	return s.Save(ctx, Skill{Summary: sum, Body: body})
}

// Delete removes a skill from the write folder. Read-only skills stay.
func (s *Store) Delete(ctx context.Context, name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("bad skill name %q", name)
	}
	dir := filepath.Join(s.writeDir, name)
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("no skill %q in %s (other folders are read-only)", name, s.writeDir)
	}
	return os.RemoveAll(dir)
}

// read parses SKILL.md in dir.
func read(dir string) (Summary, string, error) {
	b, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return Summary{}, "", err
	}
	return parse(b, dir)
}

// parse reads a SKILL.md of the skill in dir: YAML frontmatter, then the body.
func parse(b []byte, dir string) (Summary, string, error) {
	rest, ok := bytes.CutPrefix(bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")), []byte("---\n"))
	if !ok {
		return Summary{}, "", errors.New("SKILL.md has no frontmatter")
	}
	front, body, ok := bytes.Cut(rest, []byte("\n---"))
	if !ok {
		return Summary{}, "", errors.New("SKILL.md frontmatter is not closed")
	}
	var meta struct{ Name, Description string }
	if err := yaml.Unmarshal(front, &meta); err != nil {
		return Summary{}, "", fmt.Errorf("SKILL.md frontmatter: %w", err)
	}
	if meta.Name == "" {
		meta.Name = filepath.Base(dir)
	}
	return Summary{Name: meta.Name, Description: strings.Join(strings.Fields(meta.Description), " "), Dir: dir},
		strings.TrimSpace(strings.TrimPrefix(string(body), "\n")), nil
}
