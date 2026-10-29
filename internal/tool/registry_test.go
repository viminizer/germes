package tool

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	echo := func(ctx context.Context, c Call) (string, error) { return "ran", nil }
	r, err := NewRegistry(
		[]Tool{{Name: "Bash", MatchArg: "command", Run: echo}, {Name: "Read", MatchArg: "file_path", Run: echo},
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
	}
	for _, a := range allowed {
		if res := run(r, a.tool, a.args); res.IsError {
			t.Errorf("%s %s was blocked: %+v", a.tool, a.args, res)
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
