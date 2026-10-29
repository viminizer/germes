package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/viminizer/germes/internal/api"
)

// CreateEvent saves one event. Its Seq must be the next one for its session.
func (s *Store) CreateEvent(ctx context.Context, ev api.Event) error {
	var tool sql.NullString
	if ev.Tool != nil {
		b, err := json.Marshal(ev.Tool)
		if err != nil {
			return err
		}
		tool = sql.NullString{String: string(b), Valid: true}
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO events (session_id, seq, time, kind, text, tool) VALUES (?, ?, ?, ?, ?, ?)`,
		ev.SessionID, ev.Seq, ev.Time.UnixNano(), ev.Kind, ev.Text, tool)
	if err != nil {
		return fmt.Errorf("create event %s/%d: %w", ev.SessionID, ev.Seq, err)
	}
	return nil
}

// ListEvents lists events with Seq > f.AfterSeq, oldest first.
func (s *Store) ListEvents(ctx context.Context, f api.EventFilter) (api.Page[api.Event], error) {
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx, `SELECT session_id, seq, time, kind, text, tool FROM events
		WHERE session_id = ? AND seq > ? ORDER BY seq LIMIT ?`, f.SessionID, f.AfterSeq, limit+1)
	if err != nil {
		return api.Page[api.Event]{}, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()
	var page api.Page[api.Event]
	for rows.Next() {
		var ev api.Event
		var t int64
		var tool sql.NullString
		if err := rows.Scan(&ev.SessionID, &ev.Seq, &t, &ev.Kind, &ev.Text, &tool); err != nil {
			return page, err
		}
		ev.Time = time.Unix(0, t)
		if tool.Valid {
			ev.Tool = &api.ToolCall{}
			if err := json.Unmarshal([]byte(tool.String), ev.Tool); err != nil {
				return page, fmt.Errorf("event %s/%d tool: %w", ev.SessionID, ev.Seq, err)
			}
		}
		page.Items = append(page.Items, ev)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.Next = strconv.FormatInt(page.Items[limit-1].Seq, 10)
	}
	return page, rows.Err()
}

// GetLastSeq returns the highest Seq of a session, or 0.
func (s *Store) GetLastSeq(ctx context.Context, sessionID string) (int64, error) {
	var seq int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM events WHERE session_id = ?`, sessionID).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("last seq %s: %w", sessionID, err)
	}
	return seq, nil
}
