package engine

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
	// Two sessions' reviews at once: s2's would span s1's saves, so
	// without running one at a time s2 would take credit for them.
	r.Run("s1", func(context.Context) {
		time.Sleep(30 * time.Millisecond)
		write("memories/MEMORY.md", "old note, and a new one")
		write("memories/USER.md", "likes short answers")
		write("skills/fix-issues/SKILL.md", "a new skill")
		os.RemoveAll(filepath.Join(home, "skills", "old"))
	})
	r.Run("s2", func(context.Context) { time.Sleep(100 * time.Millisecond) }) // nothing saved
	r.Wait()
	want := "s1: saved by the review: memory (about you), memory (agent notes), skill fix-issues, skill old"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("notices %q\nwant [%q]", got, want)
	}
}

func TestASessionHasAtMostOneReviewWaiting(t *testing.T) {
	r := NewReviews(context.Background(), "", nil)
	release, started := make(chan struct{}), make(chan struct{})
	r.Run("busy", func(context.Context) { close(started); <-release })
	<-started // another session's review runs: the next ones wait
	var runs atomic.Int32
	for range 3 {
		r.Run("s1", func(context.Context) { runs.Add(1) })
	}
	close(release)
	r.Wait()
	if n := runs.Load(); n != 1 {
		t.Fatalf("s1 reviews run: %d, want 1", n)
	}
	// Nothing waits any more: the session can queue a review again.
	r.Run("s1", func(context.Context) { runs.Add(1) })
	r.Wait()
	if n := runs.Load(); n != 2 {
		t.Fatalf("s1 reviews run: %d, want 2", n)
	}
}
