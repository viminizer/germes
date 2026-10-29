package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/viminizer/germes/internal/stream"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
)

// Client calls a Service over the daemon's unix socket.
type Client struct {
	http *http.Client
}

var _ Service = (*Client)(nil)

// NewClient creates a client for the socket at path.
func NewClient(socketPath string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
	return &Client{http: &http.Client{Transport: tr}}
}

// GetDaemon asks the daemon who it is.
func (c *Client) GetDaemon(ctx context.Context) (Daemon, error) {
	var d Daemon
	return d, c.call(ctx, "GET", "/daemon", nil, &d)
}

// StopDaemon stops the daemon.
func (c *Client) StopDaemon(ctx context.Context) error {
	return c.call(ctx, "POST", "/daemon/stop", nil, nil)
}

// ListSessions lists sessions.
func (c *Client) ListSessions(ctx context.Context, f SessionFilter) (Page[Session], error) {
	q := url.Values{}
	set(q, "project", f.Project)
	set(q, "status", string(f.Status))
	set(q, "cursor", f.Cursor)
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	var p Page[Session]
	return p, c.call(ctx, "GET", "/sessions?"+q.Encode(), nil, &p)
}

// GetSession reads one session.
func (c *Client) GetSession(ctx context.Context, id string) (Session, error) {
	var s Session
	return s, c.call(ctx, "GET", "/sessions/"+url.PathEscape(id), nil, &s)
}

// UpdateSession renames a session.
func (c *Client) UpdateSession(ctx context.Context, u SessionUpdate) error {
	return c.call(ctx, "PATCH", "/sessions/"+url.PathEscape(u.ID), u, nil)
}

// DeleteSession deletes a session.
func (c *Client) DeleteSession(ctx context.Context, id string) error {
	return c.call(ctx, "DELETE", "/sessions/"+url.PathEscape(id), nil, nil)
}

// SendMessage sends text to a session.
func (c *Client) SendMessage(ctx context.Context, r MessageRequest) (MessageResult, error) {
	var res MessageResult
	return res, c.call(ctx, "POST", "/messages", r, &res)
}

// StopSession stops the current step of a session.
func (c *Client) StopSession(ctx context.Context, r StopRequest) error {
	return c.call(ctx, "POST", "/sessions/"+url.PathEscape(r.ID)+"/stop", r, nil)
}

// ListEvents lists stored events.
func (c *Client) ListEvents(ctx context.Context, f EventFilter) (Page[Event], error) {
	q := url.Values{}
	q.Set("session", f.SessionID)
	q.Set("after", strconv.FormatInt(f.AfterSeq, 10))
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	var p Page[Event]
	return p, c.call(ctx, "GET", "/events?"+q.Encode(), nil, &p)
}

// Watch streams events until ctx ends or the connection drops.
func (c *Client) Watch(ctx context.Context, f WatchFilter) (<-chan Event, error) {
	q := url.Values{}
	set(q, "session", f.SessionID)
	q.Set("after", strconv.FormatInt(f.AfterSeq, 10))
	req, err := http.NewRequestWithContext(ctx, "GET", "http://germes/watch?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, unavailable(err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, readError(resp)
	}
	out := make(chan Event, 64)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
		for sc.Scan() {
			var ev Event
			if json.Unmarshal(sc.Bytes(), &ev) != nil {
				continue
			}
			if !stream.Send(ctx, out, ev) {
				return
			}
		}
	}()
	return out, nil
}

// CreateJob creates and starts a job.
func (c *Client) CreateJob(ctx context.Context, r JobRequest) (Job, error) {
	var j Job
	return j, c.call(ctx, "POST", "/jobs", r, &j)
}

// ListJobs lists jobs.
func (c *Client) ListJobs(ctx context.Context, f JobFilter) (Page[Job], error) {
	q := url.Values{}
	set(q, "status", string(f.Status))
	set(q, "cursor", f.Cursor)
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	var p Page[Job]
	return p, c.call(ctx, "GET", "/jobs?"+q.Encode(), nil, &p)
}

// GetJob reads one job by id or name.
func (c *Client) GetJob(ctx context.Context, idOrName string) (Job, error) {
	var j Job
	return j, c.call(ctx, "GET", "/jobs/"+url.PathEscape(idOrName), nil, &j)
}

// StartJob starts a stopped job again.
func (c *Client) StartJob(ctx context.Context, id string) error {
	return c.call(ctx, "POST", "/jobs/"+url.PathEscape(id)+"/start", nil, nil)
}

// StopJob stops a job and its running round.
func (c *Client) StopJob(ctx context.Context, id string) error {
	return c.call(ctx, "POST", "/jobs/"+url.PathEscape(id)+"/stop", nil, nil)
}

// DeleteJob stops and deletes a job. Its round sessions stay.
func (c *Client) DeleteJob(ctx context.Context, id string) error {
	return c.call(ctx, "DELETE", "/jobs/"+url.PathEscape(id), nil, nil)
}

// ListTools lists the Germes tools a CLI engine (Claude) gets for a session
// through MCP.
func (c *Client) ListTools(ctx context.Context, sessionID string) ([]ToolInfo, error) {
	var tools []ToolInfo
	return tools, c.call(ctx, "GET", "/tools?session="+url.QueryEscape(sessionID), nil, &tools)
}

// RunTool runs one tool in a session's folder.
func (c *Client) RunTool(ctx context.Context, r ToolRequest) (ToolResult, error) {
	var res ToolResult
	return res, c.call(ctx, "POST", "/tools/run", r, &res)
}

func (c *Client) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://germes"+path, body)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return unavailable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return readError(resp)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("read %s reply: %w", path, err)
	}
	return nil
}

func readError(resp *http.Response) error {
	var e Error
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil || e.Code == "" {
		return NewError(CodeInternal, "daemon replied %s", resp.Status)
	}
	return &e
}

func unavailable(err error) error {
	return NewError(CodeUnavailable, "cannot reach the daemon: %v", err)
}

func set(q url.Values, k, v string) {
	if v != "" {
		q.Set(k, v)
	}
}
