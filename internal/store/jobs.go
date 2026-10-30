package store

import (
	"context"
	"database/sql"
	"encoding/json"
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
	vals, err := jobValues(j)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO jobs (`+jobColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, vals...)
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
	vals, err := jobValues(j)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET name = ?, dir = ?, prompt = ?, no_worktree = ?, every = ?, max_runs = ?,
		runs = ?, failures = ?, next_run_at = ?, status = ?, session_id = ?, error = ?, created_at = ?, updated_at = ?,
		workflow = ?, step = ?, vals = ?, visits = ?, repeat = ?, idle = ?, stall = ? WHERE id = ?`, append(vals[1:], j.ID)...)
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

const jobColumns = `id, name, dir, prompt, no_worktree, every, max_runs, runs, failures, next_run_at, status, session_id, error,
	created_at, updated_at, workflow, step, vals, visits, repeat, idle, stall`

func jobValues(j api.Job) ([]any, error) {
	vals, err := json.Marshal(j.Values)
	if err != nil {
		return nil, err
	}
	visits, err := json.Marshal(j.Visits)
	if err != nil {
		return nil, err
	}
	stall, err := json.Marshal(j.Stall)
	if err != nil {
		return nil, err
	}
	return []any{j.ID, j.Name, j.Dir, j.Prompt, j.NoWorktree, int64(j.Every), j.MaxRuns, j.Runs, j.Failures,
		j.NextRunAt.UnixNano(), j.Status, j.SessionID, j.Error, j.CreatedAt.UnixNano(), j.UpdatedAt.UnixNano(),
		j.Workflow, j.Step, string(vals), string(visits), j.Repeat, j.Idle, string(stall)}, nil
}

func scanJob(row interface{ Scan(...any) error }) (api.Job, error) {
	var j api.Job
	var every, next, created, updated int64
	var vals, visits, stall string
	if err := row.Scan(&j.ID, &j.Name, &j.Dir, &j.Prompt, &j.NoWorktree, &every, &j.MaxRuns, &j.Runs, &j.Failures,
		&next, &j.Status, &j.SessionID, &j.Error, &created, &updated, &j.Workflow, &j.Step, &vals, &visits, &j.Repeat, &j.Idle, &stall); err != nil {
		return j, err
	}
	if err := json.Unmarshal([]byte(vals), &j.Values); err != nil {
		return j, fmt.Errorf("job %s values: %w", j.ID, err)
	}
	if err := json.Unmarshal([]byte(visits), &j.Visits); err != nil {
		return j, fmt.Errorf("job %s visits: %w", j.ID, err)
	}
	if err := json.Unmarshal([]byte(stall), &j.Stall); err != nil {
		return j, fmt.Errorf("job %s stall: %w", j.ID, err)
	}
	j.Every = time.Duration(every)
	j.NextRunAt, j.CreatedAt, j.UpdatedAt = time.Unix(0, next), time.Unix(0, created), time.Unix(0, updated)
	return j, nil
}
