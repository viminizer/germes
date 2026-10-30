package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	echo := func(ctx context.Context, c Call) (string, error) { return "ran", nil }
	r, err := NewRegistry(
		[]Tool{{Name: "Bash", MatchArg: "command", Run: echo}, {Name: "Read", MatchArg: "file_path", Run: echo},
			{Name: "send_file", MatchArg: "file_path", BlockedAs: "Read", Run: echo},
			{Name: "Boom", Run: func(ctx context.Context, c Call) (string, error) { panic("boom") }}},
		map[string]Toolset{"core": {Tools: []string{"Bash", "Read"}}, "all": {Tools: []string{"Boom"}, Includes: []string{"core"}}},
		[]string{"Bash(sudo *)", "Bash(rm -rf /*)", "Read(.env)", "Read(**/.env*)"}, "",
	)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func run(r *Registry, name, args string) Result {
	return r.Run(context.Background(), name, Call{Dir: "/proj", Args: json.RawMessage(args)})
}

func TestBlockList(t *testing.T) {
	r := newTestRegistry(t)
	blocked := []struct{ tool, args string }{
		{"Bash", `{"command":"sudo rm x"}`},
		{"Bash", `{"command":"cd /tmp && sudo ls"}`},
		{"Bash", `{"command":"echo hi | sudo tee f"}`},
		{"Bash", `{"command":"rm -rf /usr"}`},
		{"Read", `{"file_path":".env"}`},
		{"Read", `{"file_path":"/proj/.env"}`},
		{"Read", `{"file_path":"config/.env.local"}`},
		{"Read", `{"file_path":"/elsewhere/app/.env.prod"}`},
		{"send_file", `{"file_path":"config/.env.local"}`}, // what cannot be read cannot be sent
	}
	for _, b := range blocked {
		if res := run(r, b.tool, b.args); !res.IsError || !strings.Contains(res.Output, "blocked") {
			t.Errorf("%s %s was not blocked: %+v", b.tool, b.args, res)
		}
	}
	allowed := []struct{ tool, args string }{
		{"Bash", `{"command":"echo sudo"}`},
		{"Bash", `{"command":"rm -rf ./build"}`},
		{"Read", `{"file_path":"main.go"}`},
		{"Read", `{"file_path":"docs/env.md"}`},
		{"send_file", `{"file_path":"main.go"}`},
	}
	for _, a := range allowed {
		if res := run(r, a.tool, a.args); res.IsError {
			t.Errorf("%s %s was blocked: %+v", a.tool, a.args, res)
		}
	}
}

func TestBlockListResolvedFilePaths(t *testing.T) {
	t.Setenv("HOME", "/home/test-agent")
	for _, tc := range []struct {
		name, pattern, path string
		blocked             bool
	}{
		{"relative", "/proj/private.txt", "private.txt", true},
		{"dot segments", "/proj/private.txt", "sub/../private.txt", true},
		{"absolute", "/elsewhere/private.txt", "/elsewhere/./private.txt", true},
		{"relative pattern", "config/private.txt", "/proj/config/private.txt", true},
		{"home", "/home/test-agent/private.txt", "~/private.txt", true},
		{"raw home pattern", "~/private.txt", "~/private.txt", true},
		{"basename", "private.txt", "sub/private.txt", true},
		{"basename trailing dot", "private.txt", "sub/private.txt/.", true},
		{"basename parent segments", "private.txt", "sub/private.txt/../private.txt/.", true},
		{"basename absolute trailing dot", "private.txt", "/elsewhere/private.txt/.", true},
		{"basename home trailing dot", "private.txt", "~/sub/private.txt/.", true},
		{"basename different file", "private.txt", "sub/public.txt/.", false},
		{"different file", "/proj/private.txt", "public.txt", false},
		{"different directory", "/proj/private.txt", "/elsewhere/private.txt", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, name := range []string{"Read", "Write", "Edit"} {
				t.Run(name, func(t *testing.T) {
					called := false
					r, err := NewRegistry([]Tool{{Name: name, MatchArg: "file_path", Run: func(context.Context, Call) (string, error) {
						called = true
						return "ran", nil
					}}}, nil, []string{name + "(" + tc.pattern + ")"}, "")
					if err != nil {
						t.Fatal(err)
					}
					args, err := json.Marshal(map[string]string{"file_path": tc.path})
					if err != nil {
						t.Fatal(err)
					}
					res := r.Run(context.Background(), name, Call{Dir: "/proj", Args: args})
					if res.IsError != tc.blocked || called == tc.blocked {
						t.Fatalf("blocked=%v called=%v result=%+v", tc.blocked, called, res)
					}
				})
			}
		})
	}
}

func TestBlockListWildcardMatchesNewlines(t *testing.T) {
	for _, tc := range []struct {
		name, arg, pattern, value string
		blocked                   bool
	}{
		{"Read", "file_path", "private*", "private\nnotes.txt", true},
		{"Read", "file_path", "**/private*", "sub/private\nnotes.txt", true},
		{"Bash", "command", "restricted *", "restricted first\nsecond", true},
		{"Read", "file_path", "private*", "public\nnotes.txt", false},
		{"Bash", "command", "restricted *", "echo first\nsecond", false},
	} {
		t.Run(tc.name+"/"+tc.value, func(t *testing.T) {
			called := false
			r, err := NewRegistry([]Tool{{Name: tc.name, MatchArg: tc.arg, Run: func(context.Context, Call) (string, error) {
				called = true
				return "ran", nil
			}}}, nil, []string{tc.name + "(" + tc.pattern + ")"}, "")
			if err != nil {
				t.Fatal(err)
			}
			args, err := json.Marshal(map[string]string{tc.arg: tc.value})
			if err != nil {
				t.Fatal(err)
			}
			res := r.Run(context.Background(), tc.name, Call{Dir: "/proj", Args: args})
			if res.IsError != tc.blocked || called == tc.blocked {
				t.Fatalf("blocked=%v called=%v result=%+v", tc.blocked, called, res)
			}
		})
	}
}

func TestBlockListProtectedArgumentKeys(t *testing.T) {
	for _, toolName := range []string{"Read", "Write", "Edit", "Bash"} {
		arg, denied, allowed := "file_path", "private.txt", "public.txt"
		if toolName == "Bash" {
			arg, denied, allowed = "command", "restricted action", "echo hello"
		}
		for _, tc := range []struct {
			name, args string
			blocked    bool
		}{
			{"malformed", `{"broken":`, true},
			{"nonobject", `[]`, true},
			{"wrong type", fmt.Sprintf(`{"%s":42}`, arg), true},
			{"canonical denied", fmt.Sprintf(`{"%s":%q}`, arg, denied), true},
			{"uppercase", fmt.Sprintf(`{"%s":%q}`, strings.ToUpper(arg), denied), true},
			{"mixed case", fmt.Sprintf(`{"%s":%q}`, strings.ToUpper(arg[:1])+arg[1:], denied), true},
			{"conflicting alias last", fmt.Sprintf(`{"%s":%q,"%s":%q}`, arg, allowed, strings.ToUpper(arg), denied), true},
			{"conflicting canonical last", fmt.Sprintf(`{"%s":%q,"%s":%q}`, strings.ToUpper(arg), denied, arg, allowed), true},
			{"duplicate null", fmt.Sprintf(`{"%s":%q,"%s":null}`, arg, denied, arg), true},
			{"duplicate string", fmt.Sprintf(`{"%s":%q,"%s":%q}`, arg, denied, arg, allowed), true},
			{"canonical allowed", fmt.Sprintf(`{"%s":%q,"other":{"command":"ignored"}}`, arg, allowed), false},
		} {
			t.Run(toolName+"/"+tc.name, func(t *testing.T) {
				called := false
				r, err := NewRegistry([]Tool{{Name: toolName, MatchArg: arg, Run: NewRun(func(_ context.Context, _ Call, a struct {
					FilePath string `json:"file_path"`
					Command  string `json:"command"`
				}) (string, error) {
					called = true
					return a.FilePath + a.Command, nil
				})}}, nil, []string{toolName + "(" + denied + ")"}, "")
				if err != nil {
					t.Fatal(err)
				}
				res := run(r, toolName, tc.args)
				if res.IsError != tc.blocked || called == tc.blocked {
					t.Fatalf("blocked=%v called=%v result=%+v", tc.blocked, called, res)
				}
			})
		}
	}
}

func TestRunErrors(t *testing.T) {
	r := newTestRegistry(t)
	if res := run(r, "Nope", `{}`); !res.IsError || !strings.Contains(res.Output, "Bash, Boom, Read") {
		t.Errorf("unknown tool: %+v", res)
	}
	if res := run(r, "Boom", `{}`); !res.IsError || !strings.Contains(res.Output, "boom") {
		t.Errorf("panic: %+v", res)
	}
	if got := r.List("all"); len(got) != 3 {
		t.Errorf("toolset includes: %d tools", len(got))
	}
	if long := cut(strings.Repeat("x", maxOutput+10)); len([]rune(long)) > maxOutput+100 {
		t.Errorf("output not cut: %d", len(long))
	}
}

func TestLongOutputIsSaved(t *testing.T) {
	long := strings.Repeat("x", maxOutput) + "THE END"
	r, err := NewRegistry([]Tool{{Name: "Big", Run: func(ctx context.Context, c Call) (string, error) { return long, nil }}},
		nil, nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	res := r.Run(context.Background(), "Big", Call{SessionID: "s1"})
	path := regexp.MustCompile(`in (\S+\.txt):`).FindStringSubmatch(res.Output)
	if path == nil || len([]rune(res.Output)) > maxOutput+500 || !strings.HasSuffix(res.Output, "THE END") {
		t.Fatalf("output %d characters, path %v", len(res.Output), path)
	}
	if b, err := os.ReadFile(path[1]); err != nil || string(b) != long {
		t.Fatalf("saved file: %v", err)
	}
}

func TestPruneOutputs(t *testing.T) {
	dir := t.TempDir()
	r, _ := NewRegistry(nil, nil, nil, dir)
	os.MkdirAll(filepath.Join(dir, "s1"), 0o700)
	os.MkdirAll(filepath.Join(dir, "s2"), 0o700)
	os.WriteFile(filepath.Join(dir, "s1", "old.txt"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(dir, "s2", "old.txt"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(dir, "s2", "new.txt"), []byte("x"), 0o600)
	long := time.Now().Add(-60 * 24 * time.Hour)
	os.Chtimes(filepath.Join(dir, "s1", "old.txt"), long, long)
	os.Chtimes(filepath.Join(dir, "s2", "old.txt"), long, long)
	n, err := r.PruneOutputs(time.Now().Add(-30 * 24 * time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "s1")); err == nil {
		t.Error("kept an empty session folder")
	}
	if _, err := os.Stat(filepath.Join(dir, "s2", "new.txt")); err != nil {
		t.Error("deleted a fresh output")
	}
}
