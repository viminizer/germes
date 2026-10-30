package loop

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/viminizer/germes/internal/file"
	"github.com/viminizer/germes/internal/llm"
	"github.com/viminizer/germes/internal/prompt"
)

// folderRules returns the instructions (AGENTS.md or CLAUDE.md) of the
// subfolders a tool call first reaches in this session, outermost first, to
// add to the tool's result: rules for an area arrive when the agent works
// there. The session folder's own file is in the prompt already.
func (l *Loop) folderRules(sessionID, dir string, c llm.ToolCall) string {
	var a struct {
		FilePath string `json:"file_path"`
		Path     string `json:"path"`
	}
	if json.Unmarshal(c.Args, &a) != nil {
		return ""
	}
	p := a.FilePath
	if p == "" {
		p = a.Path
	}
	if p == "" {
		return ""
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
		p = filepath.Dir(p)
	}
	if !file.IsInside(p, dir) || filepath.Clean(p) == filepath.Clean(dir) {
		return "" // the session folder itself, or outside it
	}
	var folders []string // from the touched folder up to just below dir
	for d := p; d != dir && d != filepath.Dir(d); d = filepath.Dir(d) {
		folders = append(folders, d)
	}
	slices.Reverse(folders)
	var out []string
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seenFolders[sessionID] == nil {
		l.seenFolders[sessionID] = map[string]bool{}
	}
	for _, f := range folders {
		if l.seenFolders[sessionID][f] {
			continue
		}
		l.seenFolders[sessionID][f] = true
		name, text, err := prompt.LoadRules(f)
		if err != nil || name == "" {
			continue
		}
		r, _ := filepath.Rel(dir, f)
		out = append(out, fmt.Sprintf("[Germes] Instructions for %s/ (%s), follow them for work there:\n%s", r, name, text))
	}
	if len(out) == 0 {
		return ""
	}
	return "\n\n" + strings.Join(out, "\n\n")
}
