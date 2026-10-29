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
	fmt.Fprintln(tw, "ID\tSTATUS\tUPDATED\tPROJECT\tTITLE")
	for _, s := range page.Items {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", s.ID, s.Status, ago(s.UpdatedAt), s.Project, s.Title)
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
