package codex

import (
	"context"
	"strings"
	"testing"

	"github.com/viminizer/germes/internal/llm"
)

func TestStreamErrorEventSaysWhatFailed(t *testing.T) {
	for event, want := range map[string]string{
		`{"type":"error","code":"server_error","message":"try again"}`:                     "codex: server_error: try again",
		`{"type":"error","error":{"code":"context_length_exceeded","message":"too long"}}`: "codex: context_length_exceeded: too long",
		`{"type":"error"}`: "codex: the stream sent an error with no message",
		`{"type":"response.failed","response":{"error":{"message":"overloaded"}}}`: "codex: overloaded",
	} {
		out := make(chan llm.Chunk, 1)
		(&Client{}).stream(context.Background(), strings.NewReader("data: "+event+"\n\n"), out)
		if ch := <-out; ch.Kind != llm.KindError || ch.Err.Error() != want {
			t.Errorf("%s: got %v %q, want %q", event, ch.Kind, ch.Err, want)
		}
	}
}

func TestStreamErrorEventCanBeALimit(t *testing.T) {
	for _, event := range []string{
		`{"type":"error","error":{"type":"usage_limit_reached","message":"limit reached","resets_at":1900000000}}`,
		`{"type":"response.failed","response":{"error":{"code":"usage_limit_reached","message":"limit reached","resets_at":1900000000}}}`,
	} {
		out := make(chan llm.Chunk, 1)
		(&Client{}).stream(context.Background(), strings.NewReader("data: "+event+"\n\n"), out)
		if ch := <-out; ch.Kind != llm.KindLimitHit || ch.ResetAt.Unix() != 1900000000 {
			t.Errorf("%s: got %v, reset %v; want a limit until the reset", event, ch.Kind, ch.ResetAt)
		}
	}
}
