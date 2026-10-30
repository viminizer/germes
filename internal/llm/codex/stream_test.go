package codex

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/viminizer/germes/internal/llm"
)

// end streams one event and returns the chunk the stream ended with.
func end(t *testing.T, event string) llm.Chunk {
	t.Helper()
	e, _ := (&Client{}).stream(context.Background(), strings.NewReader("data: "+event+"\n\n"), make(chan llm.Chunk, 1))
	if e == nil {
		t.Fatalf("%s: the stream completed", event)
	}
	return e.chunk
}

func TestStreamErrorEventSaysWhatFailed(t *testing.T) {
	for event, want := range map[string]string{
		`{"type":"error","code":"server_error","message":"try again"}`:                     "codex: server_error: try again",
		`{"type":"error","error":{"code":"context_length_exceeded","message":"too long"}}`: "codex: context_length_exceeded: too long",
		`{"type":"error","error":"backend down"}`:                                          "codex: backend down", // a text, not an object
		`{"type":"error"}`: "codex: the stream sent an error with no message",
		`{"type":"response.failed","response":{"error":{"message":"overloaded"}}}`: "codex: overloaded",
	} {
		if ch := end(t, event); ch.Kind != llm.KindError || ch.Err.Error() != want {
			t.Errorf("%s: got %v %q, want %q", event, ch.Kind, ch.Err, want)
		}
	}
}

func TestStreamErrorEventCanBeALimit(t *testing.T) {
	for _, event := range []string{
		`{"type":"error","error":{"type":"usage_limit_reached","message":"limit reached","resets_at":1900000000}}`,
		`{"type":"response.failed","response":{"error":{"code":"usage_limit_reached","message":"limit reached","resets_at":1900000000}}}`,
	} {
		if ch := end(t, event); ch.Kind != llm.KindLimitHit || ch.ResetAt.Unix() != 1900000000 {
			t.Errorf("%s: got %v, reset %v; want a limit until the reset", event, ch.Kind, ch.ResetAt)
		}
	}
}

const (
	serverError = `data: {"type":"error","code":"server_error","message":"try again"}` + "\n\n"
	answer      = `data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n" + `data: {"type":"response.completed"}` + "\n\n"
)

func body(s string) io.ReadCloser { return io.NopCloser(strings.NewReader(s)) }

// runStreams runs the first stream and serves the rest to each resend.
// It returns whether the response completed, what went out, and how many
// times the request was sent again.
func runStreams(first string, rest ...string) (bool, []llm.Chunk, int) {
	resends := 0
	resend := func() (io.ReadCloser, error) {
		s := rest[min(resends, len(rest)-1)]
		resends++
		return body(s), nil
	}
	out := make(chan llm.Chunk, 16)
	ok := (&Client{}).run(context.Background(), body(first), resend, out)
	close(out)
	var chunks []llm.Chunk
	for ch := range out {
		chunks = append(chunks, ch)
	}
	return ok, chunks, resends
}

func TestTemporaryStreamErrorIsSentAgain(t *testing.T) {
	ok, chunks, resends := runStreams(serverError, answer)
	if !ok || resends != 1 || len(chunks) != 1 || chunks[0].Text != "hi" {
		t.Fatalf("completed %v, resends %d, chunks %+v; want the answer after one resend", ok, resends, chunks)
	}
}

func TestCutStreamIsSentAgain(t *testing.T) {
	ok, _, resends := runStreams(`data: {"type":"response.created"}`+"\n\n", answer) // ends without completing
	if !ok || resends != 1 {
		t.Fatalf("completed %v, resends %d; want the answer after one resend", ok, resends)
	}
}

func TestStreamIsNotSentAgain(t *testing.T) {
	for name, first := range map[string]string{
		"after output":    `data: {"type":"response.output_text.delta","delta":"half"}` + "\n\n" + serverError, // it would repeat
		"a lasting error": `data: {"type":"error","code":"context_length_exceeded","message":"too long"}` + "\n\n",
	} {
		ok, chunks, resends := runStreams(first, answer)
		if ok || resends != 0 || chunks[len(chunks)-1].Kind != llm.KindError {
			t.Errorf("%s: completed %v, resends %d, chunks %+v; want the error, not a resend", name, ok, resends, chunks)
		}
	}
}

func TestStreamGivesUpAfterTheRetries(t *testing.T) {
	ok, chunks, resends := runStreams(serverError, serverError)
	if ok || resends != streamRetries || len(chunks) != 1 || !strings.Contains(chunks[0].Err.Error(), "server_error") {
		t.Fatalf("completed %v, resends %d, chunks %+v; want the error after %d resends", ok, resends, chunks, streamRetries)
	}
}
