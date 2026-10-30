package api

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Service is everything a client can ask Germes to do. The daemon implements
// it; Client calls it over the socket.
type Service interface {
	GetDaemon(ctx context.Context) (Daemon, error)
	StopDaemon(ctx context.Context) error

	ListSessions(ctx context.Context, f SessionFilter) (Page[Session], error)
	GetSession(ctx context.Context, id string) (Session, error)
	UpdateSession(ctx context.Context, u SessionUpdate) error
	DeleteSession(ctx context.Context, id string) error
	SendMessage(ctx context.Context, r MessageRequest) (MessageResult, error)
	StopSession(ctx context.Context, r StopRequest) error

	ListEvents(ctx context.Context, f EventFilter) (Page[Event], error)
	Watch(ctx context.Context, f WatchFilter) (<-chan Event, error)
	ListCheckpoints(ctx context.Context, sessionID string) ([]Checkpoint, error)
	RestoreCheckpoint(ctx context.Context, sessionID, id string) error

	CreateJob(ctx context.Context, r JobRequest) (Job, error)
	ListJobs(ctx context.Context, f JobFilter) (Page[Job], error)
	GetJob(ctx context.Context, idOrName string) (Job, error)
	StartJob(ctx context.Context, id string) error
	StopJob(ctx context.Context, id string) error
	DeleteJob(ctx context.Context, id string) error

	GetInsights(ctx context.Context, since time.Time) (Insights, error)
	ListTools(ctx context.Context, sessionID string) ([]ToolInfo, error)
	ListSkills(ctx context.Context) ([]Skill, error)
	RunTool(ctx context.Context, r ToolRequest) (ToolResult, error)
	CheckTool(ctx context.Context, r ToolRequest) (ToolVerdict, error)
}

// EventLister lists stored events (the Service, or the store).
type EventLister interface {
	ListEvents(ctx context.Context, f EventFilter) (Page[Event], error)
}

// ListAllEvents reads every stored event of f.SessionID after f.AfterSeq,
// page by page.
func ListAllEvents(ctx context.Context, l EventLister, f EventFilter) ([]Event, error) {
	var events []Event
	after := f.AfterSeq
	for {
		page, err := l.ListEvents(ctx, EventFilter{SessionID: f.SessionID, AfterSeq: after, Limit: 1000})
		if err != nil {
			return nil, err
		}
		events = append(events, page.Items...)
		if page.Next == "" || len(page.Items) == 0 {
			return events, nil
		}
		after = page.Items[len(page.Items)-1].Seq
	}
}

// RunRollback is the /rollback command of the TUI and Telegram: no number
// lists the session's snapshots, a number puts the files back as they were
// in that one. It returns the text to show.
func RunRollback(ctx context.Context, svc Service, sessionID, arg string) (string, error) {
	list, err := svc.ListCheckpoints(ctx, sessionID)
	if err != nil {
		return "", err
	}
	if len(list) == 0 {
		return "no snapshots: they are taken before each turn in a git project", nil
	}
	if arg == "" {
		var b strings.Builder
		for i, c := range list {
			fmt.Fprintf(&b, "%d. %s  %s\n", i+1, c.Time.Format("Jan 2 15:04"), c.Label)
		}
		b.WriteString("put the files back with /rollback <n> (the current files are saved first)")
		return b.String(), nil
	}
	n, err := strconv.Atoi(arg)
	if err != nil || n < 1 || n > len(list) {
		return "", NewError(CodeInvalid, "give a number from 1 to %d", len(list))
	}
	if err := svc.RestoreCheckpoint(ctx, sessionID, list[n-1].ID); err != nil {
		return "", err
	}
	// The newest snapshots now: 1 the rollback, 2 the files just before it.
	return "files are back as they were " + list[n-1].Label + " (undo: /rollback 2)", nil
}

// FormatAge says how long ago something happened, for people: "just now",
// "5m ago", "3h ago", "2d ago". The CLI's and the TUI's session lists use it.
func FormatAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
