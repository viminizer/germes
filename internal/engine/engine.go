// Package engine defines what runs one turn of a session: the Engine
// interface and the events it streams. Implementations live in subpackages.
package engine

import (
	"context"
	"encoding/json"
	"time"
)

// Engine runs one turn and streams what happens. The channel closes after a
// Done or Error event, or when ctx is cancelled.
type Engine interface {
	Name() string
	Run(ctx context.Context, t Turn) (<-chan Event, error)
}

// Turn is one user message to run.
type Turn struct {
	SessionID  string
	Dir        string
	Prompt     string
	ResumeIDs  map[string]string // engine name -> its own session id
	LastEngine string
	Extra      string // added to the system prompt (handoff, memory, skills)
}

// Kind says what an Event is.
type Kind string

// Event kinds.
const (
	KindTextDelta  Kind = "text_delta" // a piece of text while it streams
	KindText       Kind = "text"       // a finished text block
	KindToolCall   Kind = "tool_call"
	KindToolResult Kind = "tool_result"
	KindUsage      Kind = "usage"
	KindLimitHit   Kind = "limit_hit"
	KindDone       Kind = "done"
	KindError      Kind = "error"
)

// Event is one thing that happened during a turn. Only the fields of its
// Kind are set.
type Event struct {
	Kind     Kind
	Text     string
	Tool     *ToolCall
	Usage    *Usage
	ResetAt  time.Time // limit_hit
	ResumeID string    // done
	Engine   string    // done: the engine that ran the turn
	Err      error     // error
}

// ToolCall is a tool call or its result.
type ToolCall struct {
	ID      string
	Name    string
	Args    json.RawMessage
	Output  string
	IsError bool
}

// Usage is how much of a usage window is used.
type Usage struct {
	Engine      string
	Window      string // "five_hour", "seven_day"
	UsedPercent float64
	ResetAt     time.Time
}
