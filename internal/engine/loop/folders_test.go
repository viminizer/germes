package loop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/viminizer/germes/internal/llm"
)

func TestFolderRulesArriveOnce(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "web", "api"), 0o700)
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("root rules"), 0o600)
	os.WriteFile(filepath.Join(dir, "web", "AGENTS.md"), []byte("web rules"), 0o600)
	os.WriteFile(filepath.Join(dir, "web", "api", "CLAUDE.md"), []byte("api rules"), 0o600)
	l := &Loop{seenFolders: map[string]map[string]bool{}}
	call := func(args map[string]string) string {
		b, _ := json.Marshal(args)
		return l.folderRules("s", dir, llm.ToolCall{Name: "Read", Args: b})
	}
	got := call(map[string]string{"file_path": "web/api/handler.go"})
	web, api := strings.Index(got, "web rules"), strings.Index(got, "api rules")
	if web < 0 || api < web || strings.Contains(got, "root rules") {
		t.Fatalf("rules:\n%s", got)
	}
	if again := call(map[string]string{"path": "web"}); again != "" {
		t.Fatalf("given twice:\n%s", again)
	}
	if outside := call(map[string]string{"file_path": "/etc/hosts"}); outside != "" {
		t.Fatalf("outside the folder:\n%s", outside)
	}
}
