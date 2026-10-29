// Package api is the contract between the daemon and every client.
// It imports nothing from Germes.
package api

import (
	"encoding/json"
	"fmt"
	"time"
)

// Status is what a session is doing right now.
type Status string

// Session statuses.
const (
	StatusIdle    Status = "idle"
	StatusWorking Status = "working"
	StatusPaused  Status = "paused"
)

// Session is one conversation. The daemon owns it.
type Session struct {
	ID        string            `json:"id"`
	Title     string            `json:"title"`
	Project   string            `json:"project"` // git root, or the folder itself
	Dir       string            `json:"dir"`     // where the agent works
	Status    Status            `json:"status"`
	Engine    string            `json:"engine"`               // engine that ran the last turn
	ResumeIDs map[string]string `json:"resume_ids,omitempty"` // engine name -> its own session id
	Queued    []string          `json:"queued,omitempty"`     // messages waiting for the next turn (not saved)
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

// EventKind says what happened in a session.
type EventKind string

// Event kinds. TextDelta and Queue are live only: they have Seq 0 and are
// never saved.
const (
	KindUserMessage    EventKind = "user_message"
	KindTextDelta      EventKind = "text_delta"
	KindQueue          EventKind = "queue"
	KindText           EventKind = "text"
	KindToolCall       EventKind = "tool_call"
	KindToolResult     EventKind = "tool_result"
	KindTurnDone       EventKind = "turn_done"
	KindEngineSwitched EventKind = "engine_switched"
	KindLimitHit       EventKind = "limit_hit"
	KindError          EventKind = "error"
)

// Event is one entry in a session's log.
type Event struct {
	SessionID string    `json:"session_id"`
	Seq       int64     `json:"seq"`
	Time      time.Time `json:"time"`
	Kind      EventKind `json:"kind"`
	Text      string    `json:"text,omitempty"`
	Tool      *ToolCall `json:"tool,omitempty"`
	Queue     []string  `json:"queue,omitempty"` // queue: the waiting messages
}

// ToolCall is set on tool_call and tool_result events.
type ToolCall struct {
	ID      string          `json:"id"`
	Name    string          `json:"name,omitempty"`
	Args    json.RawMessage `json:"args,omitempty"`
	Output  string          `json:"output,omitempty"`
	IsError bool            `json:"is_error,omitempty"`
}

// Page is one page of a list. Next is "" at the end.
type Page[T any] struct {
	Items []T    `json:"items"`
	Next  string `json:"next,omitempty"`
}

// SessionFilter selects sessions. Empty fields match everything.
type SessionFilter struct {
	Project string
	Status  Status
	Limit   int
	Cursor  string
}

// EventFilter selects events of one session with Seq > AfterSeq, oldest first.
type EventFilter struct {
	SessionID string
	AfterSeq  int64
	Limit     int
}

// WatchFilter selects live events. AfterSeq replays stored events first.
type WatchFilter struct {
	SessionID string
	AfterSeq  int64
}

// MessageRequest sends text to a session. No SessionID: a new session is
// created in Dir.
type MessageRequest struct {
	SessionID string `json:"session_id,omitempty"`
	Dir       string `json:"dir,omitempty"`
	Text      string `json:"text"`
}

// MessageResult says which session got the message.
type MessageResult struct {
	SessionID string `json:"session_id"`
}

// StopRequest stops a session's current step. The queue is dropped, unless
// SendQueue is set: then the queued messages run right away (Esc Esc).
type StopRequest struct {
	ID        string `json:"id"`
	SendQueue bool   `json:"send_queue,omitempty"`
}

// SessionUpdate changes a session. Only Title for now.
type SessionUpdate struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// Daemon describes the running daemon.
type Daemon struct {
	Version   string    `json:"version"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
}

// Code is the kind of an Error.
type Code string

// Error codes.
const (
	CodeNotFound        Code = "not_found"
	CodeInvalid         Code = "invalid"
	CodeUnauthorized    Code = "unauthorized"
	CodeVersionMismatch Code = "version_mismatch"
	CodeUnavailable     Code = "unavailable"
	CodeInternal        Code = "internal"
)

// Error is the error every Service method returns.
type Error struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

// NewError creates an Error.
func NewError(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}
