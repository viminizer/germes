package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/viminizer/germes/internal/api"
)

type checker struct {
	api.Service
	got    api.ToolRequest
	reason string
	err    error
}

func (c *checker) CheckTool(_ context.Context, r api.ToolRequest) (api.ToolVerdict, error) {
	c.got = r
	return api.ToolVerdict{Reason: c.reason}, c.err
}

func TestCheckHook(t *testing.T) {
	const bash = `{"session_id":"claude-1","cwd":"/proj","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"gh pr create"}}`
	for name, tc := range map[string]struct {
		input   string
		svc     *checker
		allowed bool
		says    string
	}{
		"allowed":       {bash, &checker{}, true, ""},
		"blocked":       {bash, &checker{reason: "blocked by a hook: `make check` must pass"}, false, "make check"},
		"daemon down":   {bash, &checker{err: errors.New("connection refused")}, false, "could not check"}, // fail closed
		"unreadable":    {`not json`, &checker{}, false, "could not read"},
		"a Germes tool": {`{"tool_name":"mcp__germes__session_search","tool_input":{}}`, &checker{reason: "never asked"}, true, ""},
	} {
		var errOut bytes.Buffer
		allowed := CheckHook(context.Background(), tc.svc, "s1", strings.NewReader(tc.input), &errOut)
		if allowed != tc.allowed || !strings.Contains(errOut.String(), tc.says) {
			t.Errorf("%s: allowed %v, said %q", name, allowed, errOut.String())
		}
	}
	// The daemon gets the Germes session, claude's folder and the call.
	c := &checker{}
	CheckHook(context.Background(), c, "s1", strings.NewReader(bash), &bytes.Buffer{})
	if c.got.SessionID != "s1" || c.got.Dir != "/proj" || c.got.Name != "Bash" || !strings.Contains(string(c.got.Args), "gh pr create") {
		t.Fatalf("asked %+v", c.got)
	}
}
