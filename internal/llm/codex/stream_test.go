package codex

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

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
	if !ok || resends != 1 || len(chunks) != 2 || chunks[1].Text != "hi" {
		t.Fatalf("completed %v, resends %d, chunks %+v; want a status, then the answer after one resend", ok, resends, chunks)
	}
	// The user sees why it waits, and for how long.
	if n := chunks[0]; n.Kind != llm.KindStatus || n.Text != "codex: server_error: try again. Trying again in 0s (retry 1 of 2)." {
		t.Errorf("notice %+v", n)
	}
}

func TestStatusReadsWell(t *testing.T) {
	// The real message ends with a period; the status adds no second one.
	first := `data: {"type":"error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}` + "\n\n"
	_, chunks, _ := runStreams(first, answer)
	want := "codex: server_is_overloaded: Our servers are currently overloaded. Please try again later. Trying again in 0s (retry 1 of 2)."
	if chunks[0].Text != want {
		t.Fatalf("status %q\nwant   %q", chunks[0].Text, want)
	}
}

func TestCutStreamIsSentAgain(t *testing.T) {
	ok, _, resends := runStreams(`data: {"type":"response.created"}`+"\n\n", answer) // ends without completing
	if !ok || resends != 1 {
		t.Fatalf("completed %v, resends %d; want the answer after one resend", ok, resends)
	}
}

func TestStreamIsNotSentAgain(t *testing.T) {
	for name, tc := range map[string]struct {
		first  string
		outage bool // the router moves the turn on; else the turn fails
	}{
		"after output":    {`data: {"type":"response.output_text.delta","delta":"half"}` + "\n\n" + serverError, true}, // a resend would repeat it
		"a lasting error": {`data: {"type":"error","code":"context_length_exceeded","message":"too long"}` + "\n\n", false},
	} {
		ok, chunks, resends := runStreams(tc.first, answer)
		last := chunks[len(chunks)-1]
		if ok || resends != 0 || (last.Kind == llm.KindLimitHit && last.Outage) != tc.outage || (!tc.outage && last.Kind != llm.KindError) {
			t.Errorf("%s: completed %v, resends %d, chunks %+v; want no resend, outage %v", name, ok, resends, chunks, tc.outage)
		}
	}
}

func TestStreamGivesUpAfterTheRetries(t *testing.T) {
	ok, chunks, resends := runStreams(serverError, serverError)
	last := chunks[len(chunks)-1]
	// Out of tries: an outage, so the router can move the turn on (issue #40).
	if ok || resends != streamRetries || len(chunks) != streamRetries+1 || last.Kind != llm.KindLimitHit || !last.Outage || !strings.Contains(last.Err.Error(), "server_error") {
		t.Fatalf("completed %v, resends %d, chunks %+v; want an outage after %d resends", ok, resends, chunks, streamRetries)
	}
}

func TestOverloadedStreamIsSentAgain(t *testing.T) {
	for _, first := range []string{
		// What Codex sent on 2026-10-30 (session ahx0mu).
		`data: {"type":"error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}` + "\n\n",
		`data: {"type":"error","error":{"code":"rate_limit_exceeded","message":"slow down"}}` + "\n\n",
	} {
		if ok, _, resends := runStreams(first, answer); !ok || resends != 1 {
			t.Errorf("%s: completed %v, resends %d; want the answer after one resend", first, ok, resends)
		}
	}
}

func TestRetryAfterIsRead(t *testing.T) {
	for event, want := range map[string]time.Duration{
		`{"type":"error","code":"server_is_overloaded","retry_after":6}`:                              6 * time.Second,
		`{"type":"error","error":{"code":"server_is_overloaded","retry_after":"1.5"}}`:                1500 * time.Millisecond, // as a text
		`{"type":"response.failed","response":{"error":{"code":"server_error","retry_after":2}}}`:     2 * time.Second,
		`{"type":"error","code":"server_is_overloaded","retry_after":9999}`:                           maxRetryAfter,
		`{"type":"error","code":"server_is_overloaded","message":"Please try again later."}`:          0,
		`{"type":"error","code":"rate_limit_exceeded","message":"Slow down. Retry after 6 seconds."}`: 6 * time.Second, // in the message
		`{"type":"error","error":{"code":"server_error","message":"retry in 2.5s"}}`:                  2500 * time.Millisecond,
		`{"type":"error","code":"server_error","message":"retry in 9s","retry_after":3}`:              3 * time.Second, // the field wins
	} {
		e, _ := (&Client{}).stream(context.Background(), strings.NewReader("data: "+event+"\n\n"), make(chan llm.Chunk, 1))
		if e.wait != want || !e.retry {
			t.Errorf("%s: wait %v, retry %v; want %v and a retry", event, e.wait, e.retry, want)
		}
	}
}

// A request that kept failing for a temporary reason is an outage; a usage
// limit stays a limit; anything else (a bad request, the login) an error.
func TestFailedRequestKinds(t *testing.T) {
	for name, tc := range map[string]struct {
		err    error
		kind   llm.Kind
		outage bool
	}{
		"server error": {&outageError{errors.New("codex server error 503")}, llm.KindLimitHit, true},
		"usage limit":  {&limitError{msg: "limit reached"}, llm.KindLimitHit, false},
		"bad request":  {errors.New("codex 400 Bad Request: bad"), llm.KindError, false},
	} {
		if ch := errorChunk(tc.err); ch.Kind != tc.kind || ch.Outage != tc.outage {
			t.Errorf("%s: got %s outage %v", name, ch.Kind, ch.Outage)
		}
	}
}

func TestCompletedResponseSaysHowFullTheContextIs(t *testing.T) {
	_, chunks, _ := runStreams(`data: {"type":"response.completed","response":{"model":"gpt-5.5","usage":{"input_tokens":57257,"output_tokens":9}}}` + "\n\n")
	if len(chunks) != 1 || chunks[0].Kind != llm.KindContext || *chunks[0].Context != (llm.Context{Model: "gpt-5.5", Tokens: 57257}) {
		t.Fatalf("chunks %+v", chunks)
	}
	// No usage in the response: nothing to report.
	if _, chunks, _ := runStreams(answer); len(chunks) != 1 || chunks[0].Kind != llm.KindTextDelta {
		t.Fatalf("without usage: %+v", chunks)
	}
}
