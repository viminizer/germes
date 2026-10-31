// Package codex talks to the ChatGPT Codex backend (the Responses API) with
// the Codex CLI login. Wire details follow Hermes (hermes/agent/transports/codex.py).
package codex

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/viminizer/germes/internal/stream"
	"github.com/viminizer/germes/internal/text"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/viminizer/germes/internal/llm"
)

const baseURL = "https://chatgpt.com/backend-api"

// Options configure the client.
type Options struct {
	Model     string // "" = the Codex CLI's model from ~/.codex/config.toml
	Effort    string // reasoning effort, "" = medium
	Version   string // Germes version, for the User-Agent
	GermesDir string // ~/.germes: Germes' own copy of the login lives here
	CodexDir  string // ~/.codex
}

// Client is the Codex client.
type Client struct {
	opts      Options
	auth      *auth
	http      *http.Client
	retryWait time.Duration // before sending a failed stream again; times the attempt

	base string // the Codex backend; tests point it elsewhere

	winMu   sync.Mutex    // the context window read from the model list
	win     int           // last good window; 0 = none yet
	winErr  error         // why the last read failed; nil = it worked
	winAt   time.Time     // when the last read ended
	winDone chan struct{} // closed when the running read ends; nil = none runs
}

var _ llm.Client = (*Client)(nil)

// New creates the client.
func New(opts Options) *Client {
	if opts.Model == "" {
		opts.Model = GetDefaultModel(opts.CodexDir)
	}
	if opts.Effort == "" {
		opts.Effort = "medium"
	}
	hc := &http.Client{Timeout: 0} // streams can be long; ctx ends them
	return &Client{opts: opts, http: hc, retryWait: 2 * time.Second, base: baseURL, auth: &auth{
		germesFile: filepath.Join(opts.GermesDir, "auth", "codex.json"),
		codexFile:  filepath.Join(opts.CodexDir, "auth.json"),
		http:       &http.Client{Timeout: 30 * time.Second},
	}}
}

// GetDefaultModel reads `model = "..."` from the Codex CLI config.
func GetDefaultModel(codexDir string) string {
	b, err := os.ReadFile(filepath.Join(codexDir, "config.toml"))
	if err == nil {
		if m := regexp.MustCompile(`(?m)^model\s*=\s*"([^"]+)"`).FindSubmatch(b); m != nil {
			return string(m[1])
		}
	}
	return "gpt-5.5"
}

// IsAvailable reports whether there is a Codex login to use.
func IsAvailable(germesDir, codexDir string) bool {
	_, err1 := os.Stat(filepath.Join(germesDir, "auth", "codex.json"))
	_, err2 := os.Stat(filepath.Join(codexDir, "auth.json"))
	return err1 == nil || err2 == nil
}

// Send posts one request and streams the answer.
func (c *Client) Send(ctx context.Context, r llm.Request) (<-chan llm.Chunk, error) {
	body, err := json.Marshal(c.body(r))
	if err != nil {
		return nil, err
	}
	resp, tok, err := c.post(ctx, body, r.CacheKey)
	out := make(chan llm.Chunk, 64)
	if err != nil {
		var limit *limitError
		var outage *outageError
		if errors.As(err, &limit) || errors.As(err, &outage) {
			out <- errorChunk(err)
			close(out)
			return out, nil
		}
		return nil, err
	}
	go func() {
		defer close(out)
		resend := func() (io.ReadCloser, error) {
			var err error
			resp, tok, err = c.post(ctx, body, r.CacheKey)
			if err != nil {
				return nil, err
			}
			return resp.Body, nil
		}
		if !c.run(ctx, resp.Body, resend, out) {
			return
		}
		for _, u := range c.usage(ctx, tok) {
			stream.Send(ctx, out, llm.Chunk{Kind: llm.KindUsage, Usage: &u})
		}
		stream.Send(ctx, out, llm.Chunk{Kind: llm.KindDone})
	}()
	return out, nil
}

// streamRetries is how many times a stream that failed with a temporary
// error is sent again.
// ponytail: 2 quick retries (2 s, 4 s); Hermes then waits up to 5 more
// times (15-60 s, turn_recovery_autorecover.py). Add that if outages last.
const streamRetries = 2

// run streams body to out. A stream that fails with a temporary error
// (server error, overload, a cut connection) before any output went out is
// sent again with resend: nothing was shown or saved yet, so nothing
// repeats. Out of tries, or after output (a resend would repeat it), the
// provider is down for now: an outage, which the router moves to another
// engine or waits out (issue #40). It reports whether the response completed.
func (c *Client) run(ctx context.Context, body io.ReadCloser, resend func() (io.ReadCloser, error), out chan<- llm.Chunk) bool {
	for attempt := 1; ; attempt++ {
		end, sent := c.stream(ctx, body, out)
		body.Close()
		if end == nil {
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		if !end.retry {
			stream.Send(ctx, out, end.chunk)
			return false
		}
		if sent || attempt > streamRetries {
			stream.Send(ctx, out, outageChunk(end.chunk.Err, end.wait))
			return false
		}
		slog.Warn("codex stream failed; sending the request again", "attempt", attempt, "err", end.chunk.Err)
		// The backend's own retry_after wins when it asks for longer.
		wait := max(time.Duration(attempt)*c.retryWait, end.wait)
		// A wait can be long (up to maxRetryAfter): say so, as Hermes shows a
		// countdown (hermes/agent/turn_recovery_autorecover.py).
		stream.Send(ctx, out, llm.Chunk{Kind: llm.KindStatus,
			Text: fmt.Sprintf("%s. Trying again in %s (retry %d of %d).", strings.TrimRight(end.chunk.Err.Error(), ". "), wait.Round(time.Second), attempt, streamRetries)})
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return false
		}
		var err error
		if body, err = resend(); err != nil {
			stream.Send(ctx, out, errorChunk(err))
			return false
		}
	}
}

// errorChunk is the chunk for a failed request: a limit hit, an outage or
// an error.
func errorChunk(err error) llm.Chunk {
	var limit *limitError
	if errors.As(err, &limit) {
		return llm.Chunk{Kind: llm.KindLimitHit, ResetAt: limit.resetAt, Err: err}
	}
	var outage *outageError
	if errors.As(err, &outage) {
		return outageChunk(err, 0)
	}
	return llm.Chunk{Kind: llm.KindError, Err: err}
}

// outageChunk says the provider is down for now. wait is the backend's
// retry_after, 0 when it gave none; the router picks the cooldown.
func outageChunk(err error, wait time.Duration) llm.Chunk {
	ch := llm.Chunk{Kind: llm.KindLimitHit, Outage: true, Err: err}
	if wait > 0 {
		ch.ResetAt = time.Now().Add(wait)
	}
	return ch
}

// outageError is a request that kept failing for a temporary reason (a
// network error, a 5xx, a short rate limit): the provider is down for now.
type outageError struct{ err error }

func (e *outageError) Error() string { return e.err.Error() }
func (e *outageError) Unwrap() error { return e.err }

// limitError is a usage limit (HTTP 429 usage_limit_reached).
type limitError struct {
	resetAt time.Time
	msg     string
}

func (e *limitError) Error() string { return "codex usage limit: " + e.msg }

// post sends the request. It refreshes the login once on 401 and tries
// network errors, 5xx and short rate limits 3 times in all; still failing,
// they are an *outageError.
func (c *Client) post(ctx context.Context, body []byte, cacheKey string) (*http.Response, tokens, error) {
	force := false
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ { // 3 tries in all, as issue #40 asks
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(1<<(attempt-1)) * time.Second):
			case <-ctx.Done():
				return nil, tokens{}, ctx.Err()
			}
		}
		tok, err := c.auth.get(ctx, force)
		if err != nil {
			return nil, tokens{}, err
		}
		req, err := http.NewRequestWithContext(ctx, "POST", c.base+"/codex/responses", bytes.NewReader(body))
		if err != nil {
			return nil, tokens{}, err
		}
		for k, v := range headers(tok) {
			req.Header.Set(k, v)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("User-Agent", "germes/"+c.opts.Version)
		req.Header.Set("originator", "germes")
		if cacheKey != "" {
			req.Header.Set("session_id", cacheKey)
			req.Header.Set("x-client-request-id", cacheKey)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, tokens{}, ctx.Err()
			}
			lastErr = &outageError{fmt.Errorf("codex request: %w", err)}
			continue
		}
		if resp.StatusCode == http.StatusOK {
			return resp, tok, nil
		}
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusUnauthorized && !force:
			force = true
			lastErr = errors.New("codex rejected the login")
		case resp.StatusCode == http.StatusTooManyRequests:
			if limit := parseLimit(msg, resp.Header); limit != nil {
				return nil, tokens{}, limit
			}
			lastErr = &outageError{fmt.Errorf("codex rate limit: %s", short(msg))}
		case resp.StatusCode >= 500:
			lastErr = &outageError{fmt.Errorf("codex server error %s: %s", resp.Status, short(msg))}
		default:
			return nil, tokens{}, fmt.Errorf("codex %s: %s", resp.Status, short(msg))
		}
	}
	return nil, tokens{}, lastErr
}

// parseLimit returns a limitError when a 429 body, or a stream error event,
// says the usage limit is hit.
func parseLimit(body []byte, h http.Header) *limitError {
	var b struct {
		Error struct {
			Type, Code, Message string
			ResetsAt            int64 `json:"resets_at"`
			ResetAt             int64 `json:"reset_at"`
			ResetsInSeconds     int64 `json:"resets_in_seconds"`
		} `json:"error"`
	}
	json.Unmarshal(body, &b)
	e := b.Error
	if e.Type != "usage_limit_reached" && e.Code != "usage_limit_reached" && !strings.Contains(string(body), "usage_limit_reached") {
		return nil
	}
	var reset time.Time
	switch {
	case e.ResetsAt > 0:
		reset = time.Unix(e.ResetsAt, 0)
	case e.ResetAt > 0:
		reset = time.Unix(e.ResetAt, 0)
	case e.ResetsInSeconds > 0:
		reset = time.Now().Add(time.Duration(e.ResetsInSeconds) * time.Second)
	}
	return &limitError{resetAt: reset, msg: e.Message}
}

func short(b []byte) string { return text.Shorten(strings.TrimSpace(string(b)), 300) }

// body is the Responses API request.
func (c *Client) body(r llm.Request) map[string]any {
	model := r.Model
	if model == "" {
		model = c.opts.Model
	}
	b := map[string]any{
		"model": model, "instructions": r.System, "input": items(r.Messages), "store": false, "stream": true,
		"reasoning": map[string]any{"effort": c.opts.Effort, "summary": "auto"},
		"include":   []string{"reasoning.encrypted_content"},
	}
	if r.CacheKey != "" {
		b["prompt_cache_key"] = shortID("pck_", r.CacheKey)
	}
	if len(r.Tools) > 0 {
		var tools []map[string]any
		for _, t := range r.Tools {
			if t.Builtin == "web_search" {
				// Codex searches the web itself, like Hermes on Codex: a source
				// for each fact, and no fetched page in the context.
				tools = append(tools, map[string]any{"type": "web_search"})
				continue
			}
			tools = append(tools, map[string]any{"type": "function", "name": t.Name, "description": t.Description, "strict": false, "parameters": t.Schema})
		}
		b["tools"], b["tool_choice"], b["parallel_tool_calls"] = tools, "auto", true
	}
	return b
}

// items converts Germes messages to Responses API input items.
func items(msgs []llm.Message) []any {
	var out []any
	for _, m := range msgs {
		switch m.Role {
		case llm.RoleUser:
			out = append(out, map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": m.Text}}})
		case llm.RoleAssistant:
			if m.Text != "" {
				msg := map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": m.Text}}}
				if m.Phase != "" {
					msg["phase"] = string(m.Phase) // the model tells progress from final answers by it
				}
				out = append(out, msg)
			}
			for _, tc := range m.ToolCalls {
				args := string(tc.Args)
				if args == "" {
					args = "{}"
				}
				out = append(out, map[string]any{"type": "function_call", "call_id": shortID("call_", tc.ID), "name": tc.Name, "arguments": args})
			}
		case llm.RoleTool:
			out = append(out, map[string]any{"type": "function_call_output", "call_id": shortID("call_", m.ToolCallID), "output": m.Text})
		case llm.RoleRaw:
			out = append(out, m.Raw)
		}
	}
	return out
}

// shortID keeps ids within the 64-character limit.
func shortID(prefix, id string) string {
	if len(id) <= 64 {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return prefix + hex.EncodeToString(sum[:])[:32]
}

type event struct {
	Type     string          `json:"type"`
	Delta    string          `json:"delta"`
	Item     json.RawMessage `json:"item"`
	Response json.RawMessage `json:"response"` // response.failed: its "error" is read like a 429 body
	Code     string          `json:"code"`
	Message  string          `json:"message"`
	Error    json.RawMessage `json:"error"` // some backends nest the error here, as an object or a text
}

// nestedError reads a nested "error": an object with a code and message, or
// just a text. Anything else gives two empty strings.
func nestedError(raw json.RawMessage) (code, message string) {
	var e struct{ Code, Message string }
	if json.Unmarshal(raw, &e) == nil {
		return e.Code, e.Message
	}
	json.Unmarshal(raw, &message)
	return "", message
}

// transientCodes are error codes worth sending the request again for: the
// backend is expected back soon. Hermes retries the same kinds
// (hermes/agent/error_classifier.py: overloaded, server_error, timeout).
// server_is_overloaded is what Codex sent on 2026-10-30 (issue #32).
var transientCodes = map[string]bool{"server_error": true, "server_is_overloaded": true, "overloaded": true,
	"service_unavailable": true, "timeout": true, "rate_limit_exceeded": true}

// maxRetryAfter caps a backend's retry_after, as Hermes does (120 s,
// hermes/agent/turn_recovery_autorecover.py).
const maxRetryAfter = 120 * time.Second

// contextOf reads how full the context was from a finished response: its
// model and input tokens. nil when the response does not say.
func contextOf(raw json.RawMessage) *llm.Context {
	var r struct {
		Model string `json:"model"`
		Usage struct {
			InputTokens int `json:"input_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(raw, &r) != nil || r.Usage.InputTokens <= 0 {
		return nil
	}
	return &llm.Context{Model: r.Model, Tokens: r.Usage.InputTokens}
}

// streamEnd is how a stream ended without completing.
type streamEnd struct {
	chunk llm.Chunk     // the error or limit hit to send
	retry bool          // a temporary error: the request may be sent again
	wait  time.Duration // the backend's retry_after, 0 when it gave none
}

// retryAfterText finds a wait in a message, like Hermes's
// _RETRY_AFTER_SECONDS_RE (hermes/agent/retry_utils.py): "retry after 6
// seconds", "retry in 2.5s".
var retryAfterText = regexp.MustCompile(`(?i)retry\s+(?:after\s+|in\s+)?(\d+(?:\.\d+)?)\s*(?:sec|secs|seconds|s\b)`)

// retryAfter reads how long an error event asks to wait: retry_after
// (seconds, a number or a text) at the top level, under "error" or under
// "response"."error", else a wait in its message (Hermes reads the same
// places: hermes/agent/retry_utils.py). 0 when it asks for none.
func retryAfter(data, message string) time.Duration {
	type field struct {
		RetryAfter json.RawMessage `json:"retry_after"`
	}
	var ev struct {
		field
		Error    json.RawMessage `json:"error"`
		Response struct {
			Error json.RawMessage `json:"error"`
		} `json:"response"`
	}
	json.Unmarshal([]byte(data), &ev)
	var nested, failed field
	json.Unmarshal(ev.Error, &nested)
	json.Unmarshal(ev.Response.Error, &failed)
	var inText json.RawMessage
	if m := retryAfterText.FindStringSubmatch(message); m != nil {
		inText = json.RawMessage(m[1])
	}
	for _, raw := range []json.RawMessage{ev.RetryAfter, nested.RetryAfter, failed.RetryAfter, inText} {
		var v any
		json.Unmarshal(raw, &v)
		secs, _ := v.(float64)
		if str, ok := v.(string); ok {
			secs, _ = strconv.ParseFloat(strings.TrimSpace(str), 64)
		}
		if secs > 0 {
			return min(time.Duration(secs*float64(time.Second)), maxRetryAfter)
		}
	}
	return 0
}

// errorText describes an error from its code and message, never as "".
func errorText(code, message string) string {
	switch {
	case code != "" && message != "":
		return code + ": " + message
	case code+message != "":
		return code + message
	}
	return "the stream sent an error with no message"
}

type item struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Phase     string `json:"phase"`
	Content   []struct {
		Type, Text string
	} `json:"content"`
}

// stream reads the SSE body. It returns nil when the response completed,
// else how it ended, and whether any chunk went out.
func (c *Client) stream(ctx context.Context, body io.Reader, out chan<- llm.Chunk) (end *streamEnd, sent bool) {
	fail := func(code, message, data string) *streamEnd {
		return &streamEnd{chunk: llm.Chunk{Kind: llm.KindError, Err: errors.New("codex: " + errorText(code, message))},
			retry: transientCodes[code], wait: retryAfter(data, message)}
	}
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64*1024), 32*1024*1024)
	var reasoning strings.Builder // summary text streamed for the current reasoning item, like Hermes reads it
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "" || data == "[DONE]" {
			continue
		}
		var ev event
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			slog.Warn("skip unreadable Codex event", "err", err)
			continue
		}
		switch ev.Type {
		case "response.output_text.delta":
			sent = stream.Send(ctx, out, llm.Chunk{Kind: llm.KindTextDelta, Text: ev.Delta}) || sent
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			reasoning.WriteString(ev.Delta)
		case "response.reasoning_summary_part.done":
			reasoning.WriteString("\n\n") // between parts of one summary
		case "response.output_item.done":
			if s := reasoningSummary(ev.Item, reasoning.String()); s != "" {
				// Shown and saved: a stream cut after it is not sent again, or it would show twice.
				sent = stream.Send(ctx, out, llm.Chunk{Kind: llm.KindReasoning, Text: s}) || sent
			}
			reasoning.Reset()
			if ch, ok := itemChunk(ev.Item); ok {
				sent = stream.Send(ctx, out, ch) || sent
			}
		case "response.completed", "response.incomplete":
			if c := contextOf(ev.Response); c != nil {
				stream.Send(ctx, out, llm.Chunk{Kind: llm.KindContext, Context: c})
			}
			return nil, sent
		case "response.failed":
			if limit := parseLimit(ev.Response, nil); limit != nil {
				return &streamEnd{chunk: llm.Chunk{Kind: llm.KindLimitHit, ResetAt: limit.resetAt, Err: limit}}, sent
			}
			var failed struct {
				Error json.RawMessage `json:"error"`
			}
			json.Unmarshal(ev.Response, &failed)
			code, message := nestedError(failed.Error)
			// The parsed fields first: the raw event can be long (the whole
			// response) and is cut.
			slog.Warn("codex response failed", "code", code, "message", message, "event", text.Shorten(data, 500))
			if code+message == "" {
				message = "the response failed"
			}
			return fail(code, message, data), sent
		case "error":
			// A usage limit can come this way too: the session then waits
			// for the reset instead of failing.
			if limit := parseLimit([]byte(data), nil); limit != nil {
				return &streamEnd{chunk: llm.Chunk{Kind: llm.KindLimitHit, ResetAt: limit.resetAt, Err: limit}}, sent
			}
			// The fields are at the top level, or nested under "error" (Hermes
			// reads both too: hermes/agent/codex_runtime.py _raise_stream_error).
			code, message := nestedError(ev.Error)
			code, message = cmp.Or(ev.Code, code), cmp.Or(ev.Message, message)
			slog.Warn("codex error event", "code", code, "message", message, "event", text.Shorten(data, 500))
			return fail(code, message, data), sent
		}
	}
	if ctx.Err() != nil {
		return &streamEnd{}, sent
	}
	err := sc.Err()
	if err == nil {
		err = errors.New("stream ended early")
	}
	// A cut connection is temporary, like a timeout.
	return &streamEnd{chunk: llm.Chunk{Kind: llm.KindError, Err: fmt.Errorf("codex stream: %w", err)}, retry: true}, sent
}

// reasoningSummary is the summary text of a reasoning item ("" for other
// items): Codex sends it because the request asks for summary "auto". The
// streamed text counts; the item's own summary is the fallback.
func reasoningSummary(raw json.RawMessage, streamed string) string {
	var it struct {
		Type    string `json:"type"`
		Summary []struct {
			Text string `json:"text"`
		} `json:"summary"`
	}
	if json.Unmarshal(raw, &it) != nil || it.Type != "reasoning" {
		return ""
	}
	if s := strings.TrimSpace(streamed); s != "" {
		return s
	}
	var parts []string
	for _, s := range it.Summary {
		if t := strings.TrimSpace(s.Text); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n\n")
}

// itemChunk turns a finished output item into a chunk.
func itemChunk(raw json.RawMessage) (llm.Chunk, bool) {
	var it item
	if json.Unmarshal(raw, &it) != nil {
		return llm.Chunk{}, false
	}
	switch it.Type {
	case "message":
		var b strings.Builder
		for _, part := range it.Content {
			if part.Type == "output_text" {
				b.WriteString(part.Text)
			}
		}
		return llm.Chunk{Kind: llm.KindText, Text: b.String(), Phase: llm.Phase(it.Phase)}, b.Len() > 0
	case "function_call":
		return llm.Chunk{Kind: llm.KindToolCall, ToolCall: &llm.ToolCall{ID: it.CallID, Name: it.Name, Args: json.RawMessage(it.Arguments)}}, true
	case "web_search_call":
		// Codex ran the search; replayed like a reasoning item, so the next
		// request of this turn still has it (without the id: store=false).
		var m map[string]any
		json.Unmarshal(raw, &m)
		delete(m, "id")
		b, _ := json.Marshal(m)
		return llm.Chunk{Kind: llm.KindRaw, Raw: b}, true
	case "reasoning":
		// Replayed in the next request of this turn. With store=false the id
		// must go (lookups 404), and temporary items are skipped.
		if strings.HasPrefix(it.ID, "rs_tmp_") {
			return llm.Chunk{}, false
		}
		var m map[string]any
		json.Unmarshal(raw, &m)
		delete(m, "id")
		delete(m, "status")
		b, _ := json.Marshal(m)
		return llm.Chunk{Kind: llm.KindRaw, Raw: b}, true
	}
	return llm.Chunk{}, false
}

// usage reads the usage windows. Failures are only logged: usage is a hint.
func (c *Client) usage(ctx context.Context, tok tokens) []llm.Usage {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := c.get(ctx, tok, "/wham/usage")
	if err != nil {
		slog.Warn("Codex usage", "err", err)
		return nil
	}
	defer resp.Body.Close()
	type window struct {
		UsedPercent        float64 `json:"used_percent"`
		ResetAt            int64   `json:"reset_at"`
		LimitWindowSeconds int64   `json:"limit_window_seconds"`
	}
	var b struct {
		RateLimit struct {
			Primary   *window `json:"primary_window"`
			Secondary *window `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&b) != nil {
		slog.Warn("Codex usage: bad reply", "status", resp.Status)
		return nil
	}
	var out []llm.Usage
	for _, w := range []*window{b.RateLimit.Primary, b.RateLimit.Secondary} {
		if w == nil {
			continue
		}
		// Name windows by length, not position (Hermes does the same).
		name := fmt.Sprintf("%ds", w.LimitWindowSeconds)
		switch w.LimitWindowSeconds {
		case 18000:
			name = "five_hour"
		case 604800:
			name = "seven_day"
		}
		out = append(out, llm.Usage{Window: name, UsedPercent: w.UsedPercent, ResetAt: time.Unix(w.ResetAt, 0)})
	}
	return out
}
