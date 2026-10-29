// Package memory keeps two small files the agent writes: MEMORY.md (its own
// notes) and USER.md (about the user). Entries are separated by §.
package memory

import (
	"context"
	"errors"
	"fmt"
	"github.com/viminizer/germes/internal/file"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const separator = "\n§\n"

// Target is which file an entry belongs to.
type Target string

// Targets.
const (
	Agent Target = "agent" // MEMORY.md
	User  Target = "user"  // USER.md
)

// limits keep the files small on purpose (same as Hermes).
var limits = map[Target]int{Agent: 2200, User: 1375}

var files = map[Target]string{Agent: "MEMORY.md", User: "USER.md"}

// Snapshot is both files, read once at session start.
type Snapshot struct {
	Agent string
	User  string
}

// Store reads and writes the memory files.
type Store struct {
	dir string
	mu  sync.Mutex
}

// New creates the store for dir (~/.germes/memories).
func New(dir string) *Store { return &Store{dir: dir} }

// Load reads both files.
func (s *Store) Load(ctx context.Context) (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	agent, err := s.read(Agent)
	if err != nil {
		return Snapshot{}, err
	}
	user, err := s.read(User)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Agent: strings.Join(agent, separator), User: strings.Join(user, separator)}, nil
}

// CreateEntry adds an entry.
func (s *Store) CreateEntry(ctx context.Context, t Target, text string) error {
	return s.change(t, func(entries []string) ([]string, error) {
		text = strings.TrimSpace(text)
		if text == "" {
			return nil, errors.New("empty entry")
		}
		for _, e := range entries {
			if e == text {
				return nil, errors.New("this entry already exists")
			}
		}
		return append(entries, text), nil
	})
}

// UpdateEntry replaces the entry containing old with text.
func (s *Store) UpdateEntry(ctx context.Context, t Target, old, text string) error {
	return s.change(t, func(entries []string) ([]string, error) {
		i, err := find(entries, old)
		if err != nil {
			return nil, err
		}
		entries[i] = strings.TrimSpace(text)
		return entries, nil
	})
}

// DeleteEntry removes the entry containing old.
func (s *Store) DeleteEntry(ctx context.Context, t Target, old string) error {
	return s.change(t, func(entries []string) ([]string, error) {
		i, err := find(entries, old)
		if err != nil {
			return nil, err
		}
		return append(entries[:i], entries[i+1:]...), nil
	})
}

// find returns the one entry containing part.
func find(entries []string, part string) (int, error) {
	part = strings.TrimSpace(part)
	found := -1
	for i, e := range entries {
		if part != "" && strings.Contains(e, part) {
			if found >= 0 {
				return 0, fmt.Errorf("%q matches more than one entry; quote more of it", part)
			}
			found = i
		}
	}
	if found < 0 {
		return 0, fmt.Errorf("no entry contains %q", part)
	}
	return found, nil
}

func (s *Store) change(t Target, fn func([]string) ([]string, error)) error {
	name, ok := files[t]
	if !ok {
		return fmt.Errorf("unknown memory target %q (use agent or user)", t)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.read(t)
	if err != nil {
		return err
	}
	entries, err = fn(entries)
	if err != nil {
		return err
	}
	text := strings.Join(entries, separator)
	if n := len([]rune(text)); n > limits[t] {
		return fmt.Errorf("%s would be %d characters, the limit is %d: merge or delete entries first", name, n, limits[t])
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	return file.Save(filepath.Join(s.dir, name), []byte(text+"\n"), 0o600)
}

func (s *Store) read(t Target) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(s.dir, files[t]))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var entries []string
	for _, e := range strings.Split(string(b), separator) {
		if e = strings.TrimSpace(e); e != "" {
			entries = append(entries, e)
		}
	}
	return entries, nil
}
