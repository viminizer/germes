package file_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/viminizer/germes/internal/file"
)

func TestSaveIgnoresExistingTemporaryPaths(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(fmt.Sprint(symlink), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "state")
			victim := filepath.Join(dir, "other")
			if err := os.WriteFile(victim, []byte("untouched"), 0o600); err != nil {
				t.Fatal(err)
			}
			if symlink {
				if err := os.Symlink(victim, path+".tmp"); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(path+".tmp", []byte("stale"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := file.Save(path, []byte("new"), 0o600); err != nil {
				t.Fatal(err)
			}
			info, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
				t.Fatalf("published mode %v", info.Mode())
			}
			got, err := os.ReadFile(victim)
			if err != nil || string(got) != "untouched" {
				t.Fatalf("other file: %q, %v", got, err)
			}
			got, err = os.ReadFile(path + ".tmp")
			want := "stale"
			if symlink {
				want = "untouched"
			}
			if err != nil || string(got) != want {
				t.Fatalf("pre-existing temporary path: %q, %v", got, err)
			}
		})
	}
}

func TestSaveConcurrentReplacements(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	const writers = 16
	payloads := make([][]byte, writers)
	for i := range payloads {
		payloads[i] = bytes.Repeat([]byte{byte('a' + i)}, 128*1024+i)
	}
	if err := file.Save(path, payloads[0], 0o600); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, payload := range payloads {
		wg.Go(func() {
			<-start
			for range 8 {
				if err := file.Save(path, payload, 0o600); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	close(start)
	for range 128 {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		whole := false
		for _, payload := range payloads {
			if bytes.Equal(got, payload) {
				whole = true
				break
			}
		}
		if !whole {
			t.Errorf("published incomplete or mixed file of %d bytes", len(got))
			break
		}
	}
	wg.Wait()
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatalf("remaining entries: %v, %v", entries, err)
	}
}

func TestSaveFailedReplacementCleansTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(path, "old")
	if err := os.WriteFile(child, []byte("preserved"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := file.Save(path, []byte("new"), 0o600); err == nil {
		t.Fatal("replacement of nonempty directory succeeded")
	}
	got, err := os.ReadFile(child)
	if err != nil || string(got) != "preserved" {
		t.Fatalf("old destination: %q, %v", got, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("remaining entries: %v, %v", entries, err)
	}
}

func TestSaveAppliesRequestedMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "skill")
	if err := file.Save(path, []byte("public"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v", info.Mode())
	}
	if err := file.Save(path, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode())
	}
}
