// Package cli holds the small `germes <command>` commands. Each calls the
// Service and prints the result. It imports only api.
package cli

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/viminizer/germes/internal/api"
)

// ListSessions prints the sessions, newest first.
func ListSessions(ctx context.Context, svc api.Service, out io.Writer) error {
	page, err := svc.ListSessions(ctx, api.SessionFilter{Limit: 50})
	if err != nil {
		return err
	}
	if len(page.Items) == 0 {
		fmt.Fprintln(out, "no sessions")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tUPDATED\tPROJECT\tBRANCH\tTITLE")
	for _, s := range page.Items {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", s.ID, s.Status, ago(s.UpdatedAt), s.Project, orDash(s.Branch), s.Title)
	}
	return tw.Flush()
}

// StopSession stops the current step of a session.
func StopSession(ctx context.Context, svc api.Service, id string, out io.Writer) error {
	if err := svc.StopSession(ctx, api.StopRequest{ID: id}); err != nil {
		return err
	}
	fmt.Fprintln(out, "stopped", id)
	return nil
}

// StopDaemon stops the daemon. The next `germes` starts a new one.
func StopDaemon(ctx context.Context, svc api.Service, out io.Writer) error {
	if err := svc.StopDaemon(ctx); err != nil {
		return err
	}
	fmt.Fprintln(out, "daemon stopped")
	return nil
}

// CreateJob creates a job and says how to follow it.
func CreateJob(ctx context.Context, svc api.Service, r api.JobRequest, out io.Writer) error {
	j, err := svc.CreateJob(ctx, r)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "job %s started (%s); follow it with `germes attach %s`\n", j.Name, schedule(j), j.Name)
	return nil
}

// ListJobs prints the jobs, newest first.
func ListJobs(ctx context.Context, svc api.Service, out io.Writer) error {
	page, err := svc.ListJobs(ctx, api.JobFilter{Limit: 50})
	if err != nil {
		return err
	}
	if len(page.Items) == 0 {
		fmt.Fprintln(out, "no jobs")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSTATUS\tRUNS\tSCHEDULE\tWORKFLOW\tSESSION\tDIR\tERROR")
	for _, j := range page.Items {
		runs := fmt.Sprint(j.Runs)
		if j.MaxRuns > 0 {
			runs += fmt.Sprintf("/%d", j.MaxRuns)
		}
		workflow := j.Workflow
		if j.Step != "" {
			workflow += ":" + j.Step
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", j.ID, j.Name, j.Status, runs, schedule(j),
			orDash(workflow), orDash(j.SessionID), j.Dir, orDash(j.Error))
	}
	return tw.Flush()
}

// UpdateJob starts, stops or deletes (rm) a job.
func UpdateJob(ctx context.Context, svc api.Service, action, id string, out io.Writer) error {
	var err error
	done := ""
	switch action {
	case "start":
		err, done = svc.StartJob(ctx, id), "started"
	case "stop":
		err, done = svc.StopJob(ctx, id), "stopped"
	case "rm":
		err, done = svc.DeleteJob(ctx, id), "deleted"
	default:
		return fmt.Errorf("unknown job command %q", action)
	}
	if err != nil {
		return err
	}
	fmt.Fprintln(out, done, "job", id)
	return nil
}

// schedule says how often a job runs.
func schedule(j api.Job) string {
	if j.Every == 0 {
		return "back to back"
	}
	if j.Repeat {
		return "repeat every " + j.Every.String()
	}
	return "every " + j.Every.String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func ago(t time.Time) string {
	d := time.Since(t)
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

// GetInsights prints how Germes was used since a time: sessions, turns, the
// most used tools and the skills loaded.
func GetInsights(ctx context.Context, svc api.Service, since time.Time, out io.Writer) error {
	in, err := svc.GetInsights(ctx, since)
	if err != nil {
		return err
	}
	turns, engines := 0, ""
	for _, c := range in.Turns {
		turns += c.Count
		engines += fmt.Sprintf(", %s %d", c.Name, c.Count)
	}
	fmt.Fprintf(out, "Since %s: %d sessions, %d turns, %d errors\n", in.Since.Format("Jan 2"), in.Sessions, turns, in.Errors)
	if engines != "" {
		fmt.Fprintf(out, "Turns by engine: %s\n", engines[2:])
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "\nTOOL\tCALLS")
	for _, c := range in.Tools[:min(len(in.Tools), 15)] {
		fmt.Fprintf(tw, "%s\t%d\n", c.Name, c.Count)
	}
	if len(in.Skills) == 0 {
		fmt.Fprintln(tw, "\nno skills loaded")
		return tw.Flush()
	}
	fmt.Fprintln(tw, "\nSKILL\tUSES\tLAST USED")
	for _, u := range in.Skills {
		fmt.Fprintf(tw, "%s\t%d\t%s\n", u.Name, u.Count, u.LastUsed.Format("Jan 2"))
	}
	return tw.Flush()
}
