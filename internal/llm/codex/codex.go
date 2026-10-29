// Package codex talks to the ChatGPT Codex backend (the Responses API) with
// the Codex CLI login. Wire details follow Hermes (hermes/agent/transports/codex.py).
package codex

import (
	"bufio"
	"bytes"
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
	"strings"
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
	opts Options
	auth *auth
	http *http.Client
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
	return &Client{opts: opts, http: hc, auth: &auth{
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
		if errors.As(err, &limit) {
			out <- llm.Chunk{Kind: llm.KindLimitHit, ResetAt: limit.resetAt, Err: err}
			close(out)
			return out, nil
		}
		return nil, err
	}
	go func() {
		defer close(out)
		defer resp.Body.Close()
		if !c.stream(ctx, resp.Body, out) {
			return
		}
		for _, u := range c.usage(ctx, tok) {
			stream.Send(ctx, out, llm.Chunk{Kind: llm.KindUsage, Usage: &u})
		}
		stream.Send(ctx, out, llm.Chunk{Kind: llm.KindDone})
	}()
	return out, nil
}

// limitError is a usage limit (HTTP 429 usage_limit_reached).
type limitError struct {
	resetAt time.Time
	msg     string
}

func (e *limitError) Error() string { return "codex usage limit: " + e.msg }

// post sends the request. It refreshes the login once on 401 and retries
// network errors, 5xx and short rate limits up to 3 times.
func (c *Client) post(ctx context.Context, body []byte, cacheKey string) (*http.Response, tokens, error) {
	force := false
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
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
		req, err := http.NewRequestWithContext(ctx, "POST", baseURL+"/codex/responses", bytes.NewReader(body))
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
			lastErr = fmt.Errorf("codex request: %w", err)
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
			lastErr = fmt.Errorf("codex rate limit: %s", short(msg))
		case resp.StatusCode >= 500:
			lastErr = fmt.Errorf("codex server error %s: %s", resp.Status, short(msg))
		default:
			return nil, tokens{}, fmt.Errorf("codex %s: %s", resp.Status, short(msg))
		}
	}
	return nil, tokens{}, lastErr
}

// parseLimit returns a limitError when a 429 body says the usage limit is hit.
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
				out = append(out, map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": m.Text}}})
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
	Response struct {
		Error *struct{ Code, Message string } `json:"error"`
	} `json:"response"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type item struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Content   []struct {
		Type, Text string
	} `json:"content"`
}

// stream reads the SSE body. It reports whether the response completed.
func (c *Client) stream(ctx context.Context, body io.Reader, out chan<- llm.Chunk) bool {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64*1024), 32*1024*1024)
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
			stream.Send(ctx, out, llm.Chunk{Kind: llm.KindTextDelta, Text: ev.Delta})
		case "response.output_item.done":
			if ch, ok := itemChunk(ev.Item); ok {
				stream.Send(ctx, out, ch)
			}
		case "response.completed", "response.incomplete":
			return true
		case "response.failed":
			msg := "the response failed"
			if e := ev.Response.Error; e != nil {
				msg = e.Code + ": " + e.Message
				if e.Code == "usage_limit_reached" {
					stream.Send(ctx, out, llm.Chunk{Kind: llm.KindLimitHit, Err: errors.New(msg)})
					return false
				}
			}
			stream.Send(ctx, out, llm.Chunk{Kind: llm.KindError, Err: errors.New("codex: " + msg)})
			return false
		case "error":
			stream.Send(ctx, out, llm.Chunk{Kind: llm.KindError, Err: fmt.Errorf("codex: %s %s", ev.Code, ev.Message)})
			return false
		}
	}
	if ctx.Err() != nil {
		return false
	}
	err := sc.Err()
	if err == nil {
		err = errors.New("stream ended early")
	}
	stream.Send(ctx, out, llm.Chunk{Kind: llm.KindError, Err: fmt.Errorf("codex stream: %w", err)})
	return false
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
		return llm.Chunk{Kind: llm.KindText, Text: b.String()}, b.Len() > 0
	case "function_call":
		return llm.Chunk{Kind: llm.KindToolCall, ToolCall: &llm.ToolCall{ID: it.CallID, Name: it.Name, Args: json.RawMessage(it.Arguments)}}, true
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
	req, err := http.NewRequestWithContext(ctx, "GET", baseURL+"/wham/usage", nil)
	if err != nil {
		return nil
	}
	for k, v := range headers(tok) {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "germes/"+c.opts.Version)
	resp, err := c.http.Do(req)
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
