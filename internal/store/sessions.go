package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/viminizer/germes/internal/api"
)

const defaultLimit = 50

// CreateSession saves a new session.
func (s *Store) CreateSession(ctx context.Context, sess api.Session) error {
	ids, err := json.Marshal(sess.ResumeIDs)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO sessions (id, title, project, dir, branch, status, engine, resume_ids, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sess.ID, sess.Title, sess.Project, sess.Dir, sess.Branch, sess.Status, sess.Engine, string(ids),
		sess.CreatedAt.UnixNano(), sess.UpdatedAt.UnixNano())
	if err != nil {
		return fmt.Errorf("create session %s: %w", sess.ID, err)
	}
	return nil
}

// GetSession reads one session. Missing: an api not_found error.
func (s *Store) GetSession(ctx context.Context, id string) (api.Session, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE id = ?`, id)
	sess, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return api.Session{}, api.NewError(api.CodeNotFound, "no session %q", id)
	}
	return sess, err
}

// UpdateSession saves every field of sess.
func (s *Store) UpdateSession(ctx context.Context, sess api.Session) error {
	ids, err := json.Marshal(sess.ResumeIDs)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE sessions SET title = ?, project = ?, dir = ?, branch = ?, status = ?, engine = ?, resume_ids = ?, updated_at = ?
		WHERE id = ?`,
		sess.Title, sess.Project, sess.Dir, sess.Branch, sess.Status, sess.Engine, string(ids), sess.UpdatedAt.UnixNano(), sess.ID)
	if err != nil {
		return fmt.Errorf("update session %s: %w", sess.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return api.NewError(api.CodeNotFound, "no session %q", sess.ID)
	}
	return nil
}

// DeleteSession deletes a session and its events.
func (s *Store) DeleteSession(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete session %s: %w", id, err)
	}
	return nil
}

// ListSessions lists sessions, newest first. The cursor is an offset.
func (s *Store) ListSessions(ctx context.Context, f api.SessionFilter) (api.Page[api.Session], error) {
	limit, offset, err := pageBounds(f.Limit, f.Cursor)
	if err != nil {
		return api.Page[api.Session]{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+sessionColumns+` FROM sessions
		WHERE (? = '' OR project = ?) AND (? = '' OR status = ?)
		ORDER BY updated_at DESC LIMIT ? OFFSET ?`,
		f.Project, f.Project, f.Status, f.Status, limit+1, offset)
	if err != nil {
		return api.Page[api.Session]{}, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()
	var sessions []api.Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return api.Page[api.Session]{}, err
		}
		sessions = append(sessions, sess)
	}
	return newPage(sessions, limit, offset), rows.Err()
}

// pageBounds reads a list's limit and offset cursor.
func pageBounds(limit int, cursor string) (int, int, error) {
	if limit <= 0 || limit > 500 {
		limit = defaultLimit
	}
	offset := 0
	if cursor != "" {
		n, err := strconv.Atoi(cursor)
		if err != nil || n < 0 {
			return 0, 0, api.NewError(api.CodeInvalid, "bad page cursor %q", cursor)
		}
		offset = n
	}
	return limit, offset, nil
}

// newPage cuts items (read with limit+1) to one page.
func newPage[T any](items []T, limit, offset int) api.Page[T] {
	page := api.Page[T]{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		page.Next = strconv.Itoa(offset + limit)
	}
	return page
}

const sessionColumns = `id, title, project, dir, branch, status, engine, resume_ids, created_at, updated_at`

func scanSession(row interface{ Scan(...any) error }) (api.Session, error) {
	var sess api.Session
	var ids string
	var created, updated int64
	if err := row.Scan(&sess.ID, &sess.Title, &sess.Project, &sess.Dir, &sess.Branch, &sess.Status, &sess.Engine, &ids, &created, &updated); err != nil {
		return sess, err
	}
	if err := json.Unmarshal([]byte(ids), &sess.ResumeIDs); err != nil {
		return sess, fmt.Errorf("session %s resume ids: %w", sess.ID, err)
	}
	sess.CreatedAt = time.Unix(0, created)
	sess.UpdatedAt = time.Unix(0, updated)
	return sess, nil
}
