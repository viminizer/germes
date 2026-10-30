// Package engine defines what runs one turn of a session: the Engine
// interface and the events it streams. Implementations live in subpackages.
package engine

import (
	"context"
	"encoding/json"
	"fmt"
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
	Project    string // the project folder (main checkout)
	Branch     string // the worktree branch; "" = works in Project
	Prompt     string
	ResumeIDs  map[string]string // engine name -> its own session id
	LastEngine string
	Extra      string // added to the system prompt (handoff, memory, skills)
	Overseer   bool   // the overseer's turn: its role and its tools
	Plan       bool   // a plan turn: change nothing, reply with a plan
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
	KindSwitched   Kind = "switched"  // the router moved the turn to another engine (Text, Engine)
	KindCompacted  Kind = "compacted" // older history was summarized (Text, KeepFrom)
	KindError      Kind = "error"
	KindStatus     Kind = "status" // a status line for the user, not saved (Text)
)

// Event is one thing that happened during a turn. Only the fields of its
// Kind are set.
type Event struct {
	Kind     Kind
	Text     string
	Tool     *ToolCall
	Usage    *Usage
	ResetAt  time.Time // limit_hit
	Outage   bool      // limit_hit: the provider is down for now, not a usage limit; Err says why
	ResumeID string    // done
	Engine   string    // done, switched: the engine that ran (or now runs) the turn
	KeepFrom int64     // compacted: events from this seq on are kept as they are
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

// UnavailableError means every engine is limited or reserved. The session
// pauses and tries again at ResetAt.
type UnavailableError struct {
	ResetAt time.Time
}

func (e *UnavailableError) Error() string {
	return fmt.Sprintf("every engine is limited or unavailable; trying again at %s", e.ResetAt.Format("Jan 2 15:04"))
}
