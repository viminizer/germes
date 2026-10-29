package prompt

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestListChecks(t *testing.T) {
	write := func(dir, name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	withMake := t.TempDir()
	write(withMake, "Makefile", "build:\n\tgo build\ncheck: build\n\tgo test\n")
	write(withMake, "go.mod", "module x\n")
	if got := ListChecks(withMake); !slices.Equal(got, []string{"make check"}) {
		t.Errorf("make check project: %v", got)
	}
	node := t.TempDir()
	write(node, "package.json", `{"scripts":{"test":"vitest","lint":"eslint .","dev":"vite"}}`)
	write(node, "pnpm-lock.yaml", "")
	if got := ListChecks(node); !slices.Equal(got, []string{"pnpm lint", "pnpm test"}) {
		t.Errorf("pnpm project: %v", got)
	}
	if got := ListChecks(t.TempDir()); len(got) != 0 {
		t.Errorf("empty folder: %v", got)
	}
}
