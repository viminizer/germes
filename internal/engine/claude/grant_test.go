package claude

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestParserCollectsRefusedPaths(t *testing.T) {
	var p parser
	evs := p.parse([]byte(`{"type":"result","subtype":"success","session_id":"s1","permission_denials":[` +
		`{"tool_name":"Read","tool_input":{"file_path":"/x/notes.txt"}},{"tool_name":"Grep","tool_input":{"pattern":"a","path":"/y"}}]}`))
	if len(evs) != 1 || evs[0].ResumeID != "s1" || !slices.Equal(p.denied, []string{"/x/notes.txt", "/y"}) {
		t.Fatalf("events %+v, denied %v", evs, p.denied)
	}
}

func TestGrantableFolders(t *testing.T) {
	home, work, other := t.TempDir(), t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(other, "sub"), 0o700)
	c := New(Options{GermesHome: home})
	got := c.grantable([]string{
		filepath.Join(other, "a.txt"), // a file: its folder
		filepath.Join(other, "sub"),   // a folder: itself
		filepath.Join(other, "b.txt"), // the same folder again
		filepath.Join(work, ".env"),   // inside the session's folder: refused by a rule, no grant
		filepath.Join(home, "x.yaml"), // Germes' home: already allowed
		"relative.txt",                // relative: inside the session's folder
	}, work, nil)
	if !slices.Equal(got, []string{other}) {
		t.Fatalf("got %v", got)
	}
	if again := c.grantable([]string{filepath.Join(other, "a.txt")}, work, got); len(again) != 0 {
		t.Fatalf("granted twice: %v", again)
	}
}
