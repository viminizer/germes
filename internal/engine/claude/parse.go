package claude

import (
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/viminizer/germes/internal/engine"
)

// mcpServer is the name of Germes' MCP server in Claude. Its tools are
// named mcp__germes__<tool>; without the prefix they read the same as in
// the Germes loop.
const (
	mcpServer = "germes"
	mcpPrefix = "mcp__" + mcpServer + "__"
)

// parser turns claude's stream-json lines into engine events.
type parser struct {
	sessionID string
	denied    []string // paths of tool calls Claude was not allowed to make
}

type line struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	SessionID string          `json:"session_id"`
	Event     *streamEvent    `json:"event"`
	Message   *message        `json:"message"`
	RateLimit *rateLimitInfo  `json:"rate_limit_info"`
	IsError   bool            `json:"is_error"`
	Result    json.RawMessage `json:"result"`
	Denials   []struct {
		Input map[string]any `json:"tool_input"`
	} `json:"permission_denials"`
}

type streamEvent struct {
	Type  string `json:"type"`
	Delta struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"delta"`
}

type message struct {
	Content []block `json:"content"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

type rateLimitInfo struct {
	Status         string `json:"status"`
	ResetsAt       int64  `json:"resetsAt"`
	UnifiedWindows map[string]struct {
		Utilization float64 `json:"utilization"`
		ResetsAt    int64   `json:"resetsAt"`
	} `json:"unifiedWindows"`
}

// parse returns the events in one line. Unknown lines give no events.
func (p *parser) parse(raw []byte) []engine.Event {
	var l line
	if err := json.Unmarshal(raw, &l); err != nil {
		slog.Warn("skip unreadable claude line", "err", err)
		return nil
	}
	if l.SessionID != "" {
		p.sessionID = l.SessionID
	}
	switch l.Type {
	case "stream_event":
		if l.Event != nil && l.Event.Type == "content_block_delta" && l.Event.Delta.Type == "text_delta" {
			return []engine.Event{{Kind: engine.KindTextDelta, Text: l.Event.Delta.Text}}
		}
	case "assistant":
		return p.assistant(l.Message)
	case "user":
		return p.toolResults(l.Message)
	case "rate_limit_event":
		return p.rateLimit(l.RateLimit)
	case "result":
		if l.IsError {
			return []engine.Event{{Kind: engine.KindError, Err: errors.New(textOf(l.Result))}}
		}
		for _, d := range l.Denials {
			for _, k := range []string{"file_path", "path", "notebook_path"} {
				if path, ok := d.Input[k].(string); ok && path != "" {
					p.denied = append(p.denied, path)
				}
			}
		}
		return []engine.Event{{Kind: engine.KindDone, ResumeID: p.sessionID, Engine: Name}}
	}
	return nil
}

func (p *parser) assistant(m *message) []engine.Event {
	if m == nil {
		return nil
	}
	var evs []engine.Event
	for _, b := range m.Content {
		switch b.Type {
		case "text":
			evs = append(evs, engine.Event{Kind: engine.KindText, Text: b.Text})
		case "tool_use":
			evs = append(evs, engine.Event{Kind: engine.KindToolCall, Tool: &engine.ToolCall{ID: b.ID, Name: strings.TrimPrefix(b.Name, mcpPrefix), Args: b.Input}})
		}
	}
	return evs
}

func (p *parser) toolResults(m *message) []engine.Event {
	if m == nil {
		return nil
	}
	var evs []engine.Event
	for _, b := range m.Content {
		if b.Type == "tool_result" {
			evs = append(evs, engine.Event{Kind: engine.KindToolResult, Tool: &engine.ToolCall{ID: b.ToolUseID, Output: textOf(b.Content), IsError: b.IsError}})
		}
	}
	return evs
}

func (p *parser) rateLimit(r *rateLimitInfo) []engine.Event {
	if r == nil {
		return nil
	}
	var evs []engine.Event
	for window, w := range r.UnifiedWindows {
		evs = append(evs, engine.Event{Kind: engine.KindUsage, Usage: &engine.Usage{
			Engine: Name, Window: window, UsedPercent: w.Utilization * 100, ResetAt: time.Unix(w.ResetsAt, 0),
		}})
	}
	if r.Status == "rejected" {
		evs = append(evs, engine.Event{Kind: engine.KindLimitHit, ResetAt: time.Unix(r.ResetsAt, 0)})
	}
	return evs
}

// textOf reads content that is either a JSON string or a list of text blocks.
func textOf(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []block
	if json.Unmarshal(raw, &blocks) == nil {
		var b strings.Builder
		for _, bl := range blocks {
			b.WriteString(bl.Text)
		}
		return b.String()
	}
	return string(raw)
}
