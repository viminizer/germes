package engine

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// A review that saves memory or a skill tells the session what it saved, in
// one line; one that saves nothing says nothing (issue #18).
func TestReviewSaysWhatItSaved(t *testing.T) {
	home := t.TempDir()
	write := func(rel, text string) {
		path := filepath.Join(home, rel)
		os.MkdirAll(filepath.Dir(path), 0o700)
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("memories/MEMORY.md", "old note")
	write("skills/old/SKILL.md", "old skill")
	var mu sync.Mutex
	var got []string
	r := NewReviews(context.Background(), home, func(id, text string) {
		mu.Lock()
		got = append(got, id+": "+text)
		mu.Unlock()
	})
	r.Run("s1", func(context.Context) {
		write("memories/MEMORY.md", "old note, and a new one")
		write("memories/USER.md", "likes short answers")
		write("skills/fix-issues/SKILL.md", "a new skill")
		os.RemoveAll(filepath.Join(home, "skills", "old"))
	})
	r.Run("s2", func(context.Context) {}) // nothing saved
	r.Wait()
	want := "s1: saved by the review: memory (about you), memory (agent notes), skill fix-issues, skill old"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("notices %q\nwant [%q]", got, want)
	}
}
