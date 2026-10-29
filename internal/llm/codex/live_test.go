package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/viminizer/germes/internal/llm"
)

// TestLive calls the real Codex backend with the Codex CLI login. It runs
// only with GERMES_LIVE=1 and uses a temporary Germes folder.
func TestLive(t *testing.T) {
	if os.Getenv("GERMES_LIVE") == "" {
		t.Skip("set GERMES_LIVE=1 to call the real Codex API")
	}
	home, _ := os.UserHomeDir()
	c := New(Options{Version: "test", GermesDir: t.TempDir(), CodexDir: filepath.Join(home, ".codex"), Effort: "low"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	tools := []llm.ToolSpec{{Name: "add", Description: "Add two numbers.", Schema: json.RawMessage(
		`{"type":"object","properties":{"a":{"type":"number"},"b":{"type":"number"}},"required":["a","b"]}`)}}
	msgs := []llm.Message{{Role: llm.RoleUser, Text: "Use the add tool to add 2 and 3, then reply with only the result."}}
	var call *llm.ToolCall
	for _, ch := range collect(t, c, ctx, msgs, tools) {
		switch ch.Kind {
		case llm.KindToolCall:
			call = ch.ToolCall
			msgs = append(msgs, llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{*ch.ToolCall}})
		case llm.KindRaw:
			msgs = append(msgs, llm.Message{Role: llm.RoleRaw, Raw: ch.Raw})
		}
	}
	if call == nil || call.Name != "add" {
		t.Fatalf("no add tool call")
	}
	msgs = append(msgs, llm.Message{Role: llm.RoleTool, ToolCallID: call.ID, Text: "5"})
	var text string
	for _, ch := range collect(t, c, ctx, msgs, tools) {
		if ch.Kind == llm.KindText {
			text += ch.Text
		}
		if ch.Kind == llm.KindUsage {
			t.Logf("usage %s: %.0f%% (resets %s)", ch.Usage.Window, ch.Usage.UsedPercent, ch.Usage.ResetAt.Format(time.RFC3339))
		}
	}
	t.Logf("model %s answered %q", c.opts.Model, text)
	if text == "" {
		t.Fatal("no text answer")
	}
}

func collect(t *testing.T, c *Client, ctx context.Context, msgs []llm.Message, tools []llm.ToolSpec) []llm.Chunk {
	t.Helper()
	ch, err := c.Send(ctx, llm.Request{System: "You are a test.", Messages: msgs, Tools: tools, CacheKey: "germes-live-test"})
	if err != nil {
		t.Fatal(err)
	}
	var out []llm.Chunk
	for chunk := range ch {
		if chunk.Kind == llm.KindError || chunk.Kind == llm.KindLimitHit {
			t.Fatalf("%s: %v", chunk.Kind, chunk.Err)
		}
		out = append(out, chunk)
	}
	return out
}
