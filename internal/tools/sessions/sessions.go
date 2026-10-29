// Package sessions has the overseer's session tools: list, read, send,
// stop, rename and delete sessions, through the daemon's Service.
package sessions

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/viminizer/germes/internal/api"
	"github.com/viminizer/germes/internal/text"
	"github.com/viminizer/germes/internal/tool"
)

// readEvents is how many recent events session_read shows by default.
const readEvents = 30

// New returns the session tools.
func New(svc api.Service) []tool.Tool {
	idSchema := json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"],"additionalProperties":false}`)
	type idArgs struct {
		ID string `json:"id"`
	}
	return []tool.Tool{
		{
			Name: "session_list", Toolset: "sessions",
			Description: "List Germes sessions, newest first: id, status, project, title, worktree branch. Optionally only one project folder.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"project":{"type":"string"}},"additionalProperties":false}`),
			Run: tool.NewRun(func(ctx context.Context, c tool.Call, a struct {
				Project string `json:"project"`
			}) (string, error) {
				page, err := svc.ListSessions(ctx, api.SessionFilter{Project: a.Project, Limit: 50})
				if err != nil {
					return "", err
				}
				if len(page.Items) == 0 {
					return "no sessions", nil
				}
				var b strings.Builder
				for _, s := range page.Items {
					fmt.Fprintf(&b, "%s  %-7s  %s  %s", s.ID, s.Status, s.Project, s.Title)
					if s.Branch != "" {
						fmt.Fprintf(&b, "  (branch %s)", s.Branch)
					}
					b.WriteString("\n")
				}
				return b.String(), nil
			}),
		},
		{
			Name: "session_read", Toolset: "sessions",
			Description: "Read what a session did: its status and its recent messages and tool calls.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"events":{"type":"integer","description":"how many recent events (default 30)"}},"required":["id"],"additionalProperties":false}`),
			Run: tool.NewRun(func(ctx context.Context, c tool.Call, a struct {
				ID     string `json:"id"`
				Events int    `json:"events"`
			}) (string, error) {
				s, err := svc.GetSession(ctx, a.ID)
				if err != nil {
					return "", err
				}
				events, err := api.ListAllEvents(ctx, svc, api.EventFilter{SessionID: a.ID})
				if err != nil {
					return "", err
				}
				n := a.Events
				if n <= 0 {
					n = readEvents
				}
				events = events[max(len(events)-n, 0):]
				return fmt.Sprintf("session %s (%s) in %s, %s\n\n%s", s.ID, s.Title, s.Dir, s.Status, text.Transcript(events)), nil
			}),
		},
		{
			Name: "session_send", Toolset: "sessions",
			Description: "Send a message to a session (it runs as that session's next turn). Without id, start a new session in dir (a project folder; it gets its own git worktree).",
			Schema:      json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"dir":{"type":"string"},"text":{"type":"string"}},"required":["text"],"additionalProperties":false}`),
			Run: tool.NewRun(func(ctx context.Context, c tool.Call, a struct {
				ID   string `json:"id"`
				Dir  string `json:"dir"`
				Text string `json:"text"`
			}) (string, error) {
				res, err := svc.SendMessage(ctx, api.MessageRequest{SessionID: a.ID, Dir: a.Dir, Text: a.Text})
				if err != nil {
					return "", err
				}
				out := "sent to session " + res.SessionID
				if res.Warning != "" {
					out += " (" + res.Warning + ")"
				}
				return out, nil
			}),
		},
		{
			Name: "session_stop", Toolset: "sessions",
			Description: "Stop a session's running turn; its queued messages are dropped.",
			Schema:      idSchema,
			Run: tool.NewRun(func(ctx context.Context, c tool.Call, a idArgs) (string, error) {
				return "stopped " + a.ID, svc.StopSession(ctx, api.StopRequest{ID: a.ID})
			}),
		},
		{
			Name: "session_rename", Toolset: "sessions",
			Description: "Rename a session.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"title":{"type":"string"}},"required":["id","title"],"additionalProperties":false}`),
			Run: tool.NewRun(func(ctx context.Context, c tool.Call, a struct {
				ID    string `json:"id"`
				Title string `json:"title"`
			}) (string, error) {
				return "renamed " + a.ID, svc.UpdateSession(ctx, api.SessionUpdate{ID: a.ID, Title: a.Title})
			}),
		},
		{
			Name: "session_delete", Toolset: "sessions",
			Description: "Delete a session and its history. Refused while its worktree has unmerged work.",
			Schema:      idSchema,
			Run: tool.NewRun(func(ctx context.Context, c tool.Call, a idArgs) (string, error) {
				return "deleted " + a.ID, svc.DeleteSession(ctx, a.ID)
			}),
		},
	}
}
