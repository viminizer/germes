package refs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/viminizer/germes/internal/tool"
	"github.com/viminizer/germes/internal/tools/files"
)

func TestListFindsReferences(t *testing.T) {
	got := List(`look at @file:main.go:10-12 and @diff, also @git:"log -3" and an email a@b.c and @file:main.go:10-12`)
	want := []string{"@file:main.go:10-12", "@diff", `@git:"log -3"`}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i, r := range got {
		if r.Token() != want[i] {
			t.Fatalf("ref %d = %s, want %s", i, r.Token(), want[i])
		}
	}
}

func TestExpandRespectsBlockList(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\ntwo\nthree\n"), 0o644)
	os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET=1"), 0o644)
	reg, err := tool.NewRegistry(files.New(), nil, []string{"Read(**/.env*)"})
	if err != nil {
		t.Fatal(err)
	}
	out := New(reg).Expand(context.Background(), "see @file:a.txt:2-3 and @file:.env", "s", dir)
	if !strings.Contains(out, "--- @file:a.txt:2-3 ---") || !strings.Contains(out, "two") || strings.Contains(out, "one") {
		t.Fatalf("line range not applied:\n%s", out)
	}
	if strings.Contains(out, "SECRET") || !strings.Contains(out, "blocked") {
		t.Fatalf(".env was not blocked:\n%s", out)
	}
}
