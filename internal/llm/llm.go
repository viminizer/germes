// Package llm talks to model APIs. Germes keeps its own message format;
// each implementation (llm/codex, later llm/openai) converts it.
package llm

import (
	"context"
	"encoding/json"
	"time"
)

// Client sends one request and streams the answer.
type Client interface {
	Send(ctx context.Context, r Request) (<-chan Chunk, error)
}

// Request is one model call.
type Request struct {
	Model    string
	System   string
	Messages []Message
	Tools    []ToolSpec
	CacheKey string // keeps the provider's prompt cache per session
}

// Role of a message.
type Role string

// Roles.
const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool" // a tool result
	RoleRaw       Role = "raw"  // a provider item passed back as is (reasoning)
)

// Message is one entry of the conversation.
type Message struct {
	Role       Role
	Text       string
	ToolCalls  []ToolCall      // assistant
	ToolCallID string          // tool
	Raw        json.RawMessage // raw
}

// ToolCall is the model asking to run a tool.
type ToolCall struct {
	ID   string
	Name string
	Args json.RawMessage
}

// ToolSpec describes a tool to the model.
type ToolSpec struct {
	Name        string
	Description string
	Schema      json.RawMessage
}

// Kind says what a Chunk is.
type Kind string

// Chunk kinds.
const (
	KindTextDelta Kind = "text_delta"
	KindText      Kind = "text" // one finished text block
	KindToolCall  Kind = "tool_call"
	KindRaw       Kind = "raw" // an item to send back next time (Raw)
	KindUsage     Kind = "usage"
	KindLimitHit  Kind = "limit_hit"
	KindDone      Kind = "done"
	KindError     Kind = "error"
)

// Chunk is one piece of a streamed answer. The channel ends after Done,
// LimitHit or Error.
type Chunk struct {
	Kind     Kind
	Text     string
	ToolCall *ToolCall
	Raw      json.RawMessage
	Usage    *Usage
	ResetAt  time.Time // limit_hit; zero when unknown
	Err      error
}

// Usage is how much of a usage window is used.
type Usage struct {
	Window      string // "five_hour", "seven_day"
	UsedPercent float64
	ResetAt     time.Time
}
