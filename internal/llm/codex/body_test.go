package codex

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/viminizer/germes/internal/llm"
)

// A tool Codex may replace with its own search goes out as the built-in
// web_search; other tools stay functions.
func TestWebSearchGoesOutAsCodexsOwnSearch(t *testing.T) {
	c := &Client{opts: Options{Model: "m"}}
	b, _ := json.Marshal(c.body(llm.Request{Tools: []llm.ToolSpec{
		{Name: "WebSearch", Builtin: "web_search"}, {Name: "WebFetch"}}}))
	if s := string(b); !strings.Contains(s, `{"type":"web_search"}`) || strings.Contains(s, `"name":"WebSearch"`) || !strings.Contains(s, `"name":"WebFetch"`) {
		t.Fatalf("tools: %s", s)
	}
}
