package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/viminizer/germes/internal/api"
)

// CreateJob saves a new job. A taken name: an api invalid error.
func (s *Store) CreateJob(ctx context.Context, j api.Job) error {
	if _, err := s.GetJob(ctx, j.Name); err == nil {
		return api.NewError(api.CodeInvalid, "a job named %q already exists", j.Name)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO jobs (`+jobColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, jobValues(j)...)
	if err != nil {
		return fmt.Errorf("create job %s: %w", j.ID, err)
	}
	return nil
}

// GetJob reads one job by id or name. Missing: an api not_found error.
func (s *Store) GetJob(ctx context.Context, idOrName string) (api.Job, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id = ? OR name = ? ORDER BY id = ? DESC LIMIT 1`,
		idOrName, idOrName, idOrName)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return api.Job{}, api.NewError(api.CodeNotFound, "no job %q", idOrName)
	}
	return j, err
}

// UpdateJob saves every field of j.
func (s *Store) UpdateJob(ctx context.Context, j api.Job) error {
	vals := append(jobValues(j)[1:], j.ID)
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET name = ?, dir = ?, prompt = ?, no_worktree = ?, every = ?, max_runs = ?,
		runs = ?, failures = ?, next_run_at = ?, status = ?, session_id = ?, error = ?, created_at = ?, updated_at = ? WHERE id = ?`, vals...)
	if err != nil {
		return fmt.Errorf("update job %s: %w", j.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return api.NewError(api.CodeNotFound, "no job %q", j.ID)
	}
	return nil
}

// DeleteJob deletes a job. Its round sessions stay.
func (s *Store) DeleteJob(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM jobs WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete job %s: %w", id, err)
	}
	return nil
}

// ListJobs lists jobs, newest first. The cursor is an offset.
func (s *Store) ListJobs(ctx context.Context, f api.JobFilter) (api.Page[api.Job], error) {
	limit, offset, err := pageBounds(f.Limit, f.Cursor)
	if err != nil {
		return api.Page[api.Job]{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE (? = '' OR status = ?)
		ORDER BY created_at DESC LIMIT ? OFFSET ?`, f.Status, f.Status, limit+1, offset)
	if err != nil {
		return api.Page[api.Job]{}, fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()
	var jobs []api.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return api.Page[api.Job]{}, err
		}
		jobs = append(jobs, j)
	}
	return newPage(jobs, limit, offset), rows.Err()
}

const jobColumns = `id, name, dir, prompt, no_worktree, every, max_runs, runs, failures, next_run_at, status, session_id, error, created_at, updated_at`

func jobValues(j api.Job) []any {
	return []any{j.ID, j.Name, j.Dir, j.Prompt, j.NoWorktree, int64(j.Every), j.MaxRuns, j.Runs, j.Failures,
		j.NextRunAt.UnixNano(), j.Status, j.SessionID, j.Error, j.CreatedAt.UnixNano(), j.UpdatedAt.UnixNano()}
}

func scanJob(row interface{ Scan(...any) error }) (api.Job, error) {
	var j api.Job
	var every, next, created, updated int64
	if err := row.Scan(&j.ID, &j.Name, &j.Dir, &j.Prompt, &j.NoWorktree, &every, &j.MaxRuns, &j.Runs, &j.Failures,
		&next, &j.Status, &j.SessionID, &j.Error, &created, &updated); err != nil {
		return j, err
	}
	j.Every = time.Duration(every)
	j.NextRunAt, j.CreatedAt, j.UpdatedAt = time.Unix(0, next), time.Unix(0, created), time.Unix(0, updated)
	return j, nil
}
