// Package api is the contract between the daemon and every client.
// It imports nothing from Germes.
package api

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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

// OverseerID is the overseer's session: one session, in the home folder,
// that controls Germes (other sessions, jobs) for the user.
const OverseerID = "overseer"

// Session is one conversation. The daemon owns it.
type Session struct {
	ID        string            `json:"id"`
	Title     string            `json:"title"`
	Project   string            `json:"project"`          // git root, or the folder itself
	Dir       string            `json:"dir"`              // where the agent works
	Branch    string            `json:"branch,omitempty"` // its own worktree's branch; "" = works in Project
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
// never saved. A job's Notice is live only too.
const (
	KindUserMessage    EventKind = "user_message"
	KindTextDelta      EventKind = "text_delta"
	KindQueue          EventKind = "queue"
	KindNotice         EventKind = "notice" // Text: a message for the user (a job's are live only; "plan saved" is saved)
	KindText           EventKind = "text"
	KindToolCall       EventKind = "tool_call"
	KindToolResult     EventKind = "tool_result"
	KindTurnDone       EventKind = "turn_done"
	KindEngineSwitched EventKind = "engine_switched"
	KindCompacted      EventKind = "compacted"  // Text: summary of everything before KeepFrom
	KindAttachment     EventKind = "attachment" // Text: the expanded @ references of the user message before it
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
	Queue     []string  `json:"queue,omitempty"`     // queue: the waiting messages
	Engine    string    `json:"engine,omitempty"`    // engine_switched, turn_done: the engine running the session
	KeepFrom  int64     `json:"keep_from,omitempty"` // compacted: events from this seq on are kept
}

// ToolCall is set on tool_call and tool_result events.
type ToolCall struct {
	ID      string          `json:"id"`
	Name    string          `json:"name,omitempty"`
	Args    json.RawMessage `json:"args,omitempty"`
	Output  string          `json:"output,omitempty"`
	IsError bool            `json:"is_error,omitempty"`
}

// ToolSendFile is the tool that sends a file to the user. Its result's
// Output is the file's full path; Telegram uploads it from there.
const ToolSendFile = "send_file"

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

// EventFilter selects events of one session with Seq > AfterSeq, oldest
// first. With Query set it is a full-text search instead (all sessions when
// SessionID is empty), best matches first.
type EventFilter struct {
	SessionID string
	AfterSeq  int64
	Limit     int
	Query     string
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
	Worktree  bool   `json:"worktree,omitempty"`    // a new session gets its own git worktree; false = works in Dir
	SameDirAs string `json:"same_dir_as,omitempty"` // new session works where this session works (a job's rounds share a worktree)
	Plan      bool   `json:"plan,omitempty"`        // a plan turn: change nothing; the reply is saved as a plan file
}

// MessageResult says which session got the message.
type MessageResult struct {
	SessionID string `json:"session_id"`
	Warning   string `json:"warning,omitempty"` // e.g. no worktree could be made
}

// StopRequest stops a session's current step. The queue is dropped, unless
// SendQueue is set: then the queued messages run right away (Esc Esc).
type StopRequest struct {
	ID        string `json:"id"`
	SendQueue bool   `json:"send_queue,omitempty"`
}

// SessionUpdate changes a session. Empty fields are left as they are.
type SessionUpdate struct {
	ID    string `json:"id"`
	Title string `json:"title,omitempty"`
	// LeaveWorktree moves the session to the project folder and removes its
	// worktree. Refused while the worktree has unmerged work.
	LeaveWorktree bool `json:"leave_worktree,omitempty"`
	// EnterWorktree moves the session into its own new git worktree. Refused
	// outside a git repo or when it has one.
	EnterWorktree bool `json:"enter_worktree,omitempty"`
}

// JobStatus is where a job is in its life.
type JobStatus string

// Job statuses.
const (
	JobRunning JobStatus = "running"
	JobStopped JobStatus = "stopped"
	JobDone    JobStatus = "done"   // the agent replied [DONE], or MaxRuns rounds ran
	JobFailed  JobStatus = "failed" // rounds kept failing, or the job cannot run
)

// Job runs a prompt again and again, each round in a new session, all rounds
// in one folder (their own worktree unless NoWorktree).
type Job struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Dir        string            `json:"dir"`
	Prompt     string            `json:"prompt"`
	NoWorktree bool              `json:"no_worktree,omitempty"`
	Repeat     bool              `json:"repeat,omitempty"` // runs until stopped: [DONE] or workflow done only means "nothing to do now"
	Idle       int               `json:"idle,omitempty"`   // repeat: rounds in a row with nothing to do; the wait grows with it
	Every      time.Duration     `json:"every"`            // time between round starts; 0 = next round right away
	MaxRuns    int               `json:"max_runs"`         // 0 = no limit
	Runs       int               `json:"runs"`
	Failures   int               `json:"failures"` // failed rounds in a row
	NextRunAt  time.Time         `json:"next_run_at"`
	Status     JobStatus         `json:"status"`
	SessionID  string            `json:"session_id,omitempty"` // the latest round's session
	Error      string            `json:"error,omitempty"`      // why the last round failed
	Workflow   string            `json:"workflow,omitempty"`   // the workflow each round follows; "" = one prompt per round
	Step       string            `json:"step,omitempty"`       // workflow: the step running now ("" = between rounds)
	Values     map[string]string `json:"values,omitempty"`     // workflow: values from replies, like ISSUE
	Visits     map[string]int    `json:"visits,omitempty"`     // workflow: runs of each step this round
	CreatedAt  time.Time         `json:"created_at"`
	UpdatedAt  time.Time         `json:"updated_at"`
}

// JobRequest creates a job. No Name: the job's id. With a Workflow the
// prompt is optional: it is the {GOAL} value.
type JobRequest struct {
	Name       string        `json:"name,omitempty"`
	Workflow   string        `json:"workflow,omitempty"`
	Dir        string        `json:"dir"`
	Prompt     string        `json:"prompt"`
	NoWorktree bool          `json:"no_worktree,omitempty"`
	Repeat     bool          `json:"repeat,omitempty"`
	Every      time.Duration `json:"every,omitempty"`
	MaxRuns    int           `json:"max_runs,omitempty"`
}

// JobFilter selects jobs. Empty fields match everything.
type JobFilter struct {
	Status JobStatus
	Limit  int
	Cursor string
}

// ToolRequest runs one tool, as an agent would, in the session's folder (or
// Dir). The block list applies.
type ToolRequest struct {
	SessionID string          `json:"session_id,omitempty"`
	Dir       string          `json:"dir,omitempty"` // "" = the session's folder
	Name      string          `json:"name"`
	Args      json.RawMessage `json:"args"`
}

// Checkpoint is a snapshot of a session's folder, taken before a turn.
type Checkpoint struct {
	ID    string    `json:"id"`
	Label string    `json:"label"`
	Time  time.Time `json:"time"`
}

// ToolInfo describes a tool an engine may call.
type ToolInfo struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"` // JSON Schema of the arguments
}

// ToolResult is a tool's output.
type ToolResult struct {
	Output  string `json:"output"`
	IsError bool   `json:"is_error,omitempty"`
}

// Daemon describes the running daemon.
type Daemon struct {
	Version   string    `json:"version"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
	Busy      []string  `json:"busy,omitempty"` // sessions with a turn running or messages waiting here
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

// HasCode reports whether err is an *Error with one of codes.
func HasCode(err error, codes ...Code) bool {
	var e *Error
	return errors.As(err, &e) && slices.Contains(codes, e.Code)
}

// IsNotFound reports whether err says the thing does not exist.
func IsNotFound(err error) bool { return HasCode(err, CodeNotFound) }

// NewID returns a new short random id, for sessions and jobs.
func NewID() string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 6)
	rand.Read(b)
	for i := range b {
		b[i] = letters[int(b[i])%len(letters)]
	}
	return string(b)
}

// RefKinds are the @ references a message may use (expanded by the daemon),
// with a short help for suggestions.
var RefKinds = []struct{ Token, Help string }{
	{"@file:", "a file's text; add :10-20 for lines"},
	{"@folder:", "a folder's files"},
	{"@diff", "uncommitted changes"},
	{"@staged", "staged changes"},
	{"@git:", `git output, e.g. @git:"log -5"`},
	{"@url:", "a web page's text"},
}

// Insights is how Germes was used since a time, from the event log.
type Insights struct {
	Since    time.Time  `json:"since"`
	Sessions int        `json:"sessions"` // sessions with a turn since then
	Turns    []Count    `json:"turns"`    // finished turns per engine, most first
	Errors   int        `json:"errors"`
	Tools    []Count    `json:"tools"`  // calls per tool, most first
	Skills   []SkillUse `json:"skills"` // skills loaded, most first
}

// Count is how often something happened.
type Count struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// SkillUse is how often a skill was loaded, and when last.
type SkillUse struct {
	Name     string    `json:"name"`
	Count    int       `json:"count"`
	LastUsed time.Time `json:"last_used"`
}
