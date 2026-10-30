// Package jobs has the overseer's job tools: create, list, start, stop and
// delete jobs, through the daemon's Service.
package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/viminizer/germes/internal/api"
	"github.com/viminizer/germes/internal/tool"
)

// New returns the job tools.
func New(svc api.Service) []tool.Tool {
	tools := []tool.Tool{
		{
			Name: "job_create", Toolset: "jobs",
			Description: "Create and start a job in a project folder: a prompt that runs round after round in a new session each time (until the agent replies [DONE]; with repeat, until stopped), " +
				"or a workflow (a file in ~/.germes/workflows or <project>/.germes/workflows; the prompt is then its {GOAL}).",
			Schema: json.RawMessage(`{"type":"object","properties":{"dir":{"type":"string"},"prompt":{"type":"string"},"workflow":{"type":"string"},` +
				`"name":{"type":"string"},"every":{"type":"string","description":"time between round starts, like 30m or 1h; empty = back to back"},` +
				`"max_runs":{"type":"integer"},"no_worktree":{"type":"boolean"},"repeat":{"type":"boolean","description":"run until stopped: [DONE] or the workflow reaching done only means nothing to do now, and the job waits a while"}},"required":["dir"],"additionalProperties":false}`),
			Run: tool.NewRun(func(ctx context.Context, c tool.Call, a struct {
				Dir        string `json:"dir"`
				Prompt     string `json:"prompt"`
				Workflow   string `json:"workflow"`
				Name       string `json:"name"`
				Every      string `json:"every"`
				MaxRuns    int    `json:"max_runs"`
				NoWorktree bool   `json:"no_worktree"`
				Repeat     bool   `json:"repeat"`
			}) (string, error) {
				var every time.Duration
				if a.Every != "" {
					d, err := time.ParseDuration(a.Every)
					if err != nil {
						return "", fmt.Errorf("every: %w", err)
					}
					every = d
				}
				j, err := svc.CreateJob(ctx, api.JobRequest{Name: a.Name, Dir: a.Dir, Prompt: a.Prompt, Workflow: a.Workflow,
					Every: every, MaxRuns: a.MaxRuns, NoWorktree: a.NoWorktree, Repeat: a.Repeat})
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("job %s (%s) started", j.Name, j.ID), nil
			}),
		},
		{
			Name: "job_list", Toolset: "jobs",
			Description: "List jobs: id, name, status, rounds run, workflow step, latest round's session, last error.",
			Schema:      json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
			Run: tool.NewRun(func(ctx context.Context, c tool.Call, _ struct{}) (string, error) {
				page, err := svc.ListJobs(ctx, api.JobFilter{Limit: 50})
				if err != nil {
					return "", err
				}
				if len(page.Items) == 0 {
					return "no jobs", nil
				}
				var b strings.Builder
				for _, j := range page.Items {
					fmt.Fprintf(&b, "%s  %s  %s  runs %d", j.ID, j.Name, j.Status, j.Runs)
					if j.Workflow != "" {
						fmt.Fprintf(&b, "  workflow %s", j.Workflow)
						if j.Step != "" {
							fmt.Fprintf(&b, " (step %s)", j.Step)
						}
					}
					if j.SessionID != "" {
						fmt.Fprintf(&b, "  session %s", j.SessionID)
					}
					fmt.Fprintf(&b, "  in %s", j.Dir)
					if j.Error != "" {
						fmt.Fprintf(&b, "  error: %s", j.Error)
					}
					b.WriteString("\n")
				}
				return b.String(), nil
			}),
		},
	}
	// The tools that only take a job and change it.
	idSchema := json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","description":"job id or name"}},"required":["id"],"additionalProperties":false}`)
	for _, c := range []struct {
		name, help, done string
		call             func(context.Context, string) error
	}{
		{"job_start", "Start a stopped, done or failed job again.", "started", svc.StartJob},
		{"job_stop", "Stop a job and its running round.", "stopped", svc.StopJob},
		{"job_delete", "Stop and delete a job. Its round sessions stay.", "deleted", svc.DeleteJob},
	} {
		tools = append(tools, tool.Tool{Name: c.name, Toolset: "jobs", Description: c.help, Schema: idSchema,
			Run: tool.NewRun(func(ctx context.Context, _ tool.Call, a struct {
				ID string `json:"id"`
			}) (string, error) {
				return c.done + " " + a.ID, c.call(ctx, a.ID)
			})})
	}
	return tools
}
