package store

import (
	"context"
	"fmt"
	"time"

	"github.com/viminizer/germes/internal/engine"
)

// ListLimits returns, per engine, until when it is limited.
func (s *Store) ListLimits(ctx context.Context) (map[string]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT engine, until FROM engine_limits`)
	if err != nil {
		return nil, fmt.Errorf("list limits: %w", err)
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var name string
		var until int64
		if err := rows.Scan(&name, &until); err != nil {
			return nil, err
		}
		out[name] = time.Unix(0, until)
	}
	return out, rows.Err()
}

// UpdateLimit saves until when an engine is limited.
func (s *Store) UpdateLimit(ctx context.Context, name string, until time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO engine_limits (engine, until) VALUES (?, ?)
		ON CONFLICT (engine) DO UPDATE SET until = excluded.until`, name, until.UnixNano())
	if err != nil {
		return fmt.Errorf("update limit %s: %w", name, err)
	}
	return nil
}

// ListUsage returns the last known usage of every engine and window.
func (s *Store) ListUsage(ctx context.Context) ([]engine.Usage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT engine, window, used_percent, reset_at FROM engine_usage`)
	if err != nil {
		return nil, fmt.Errorf("list usage: %w", err)
	}
	defer rows.Close()
	var out []engine.Usage
	for rows.Next() {
		var u engine.Usage
		var reset int64
		if err := rows.Scan(&u.Engine, &u.Window, &u.UsedPercent, &reset); err != nil {
			return nil, err
		}
		u.ResetAt = time.Unix(0, reset)
		out = append(out, u)
	}
	return out, rows.Err()
}

// UpdateUsage saves the latest usage of one engine window.
func (s *Store) UpdateUsage(ctx context.Context, u engine.Usage) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO engine_usage (engine, window, used_percent, reset_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (engine, window) DO UPDATE SET used_percent = excluded.used_percent, reset_at = excluded.reset_at`,
		u.Engine, u.Window, u.UsedPercent, u.ResetAt.UnixNano())
	if err != nil {
		return fmt.Errorf("update usage %s/%s: %w", u.Engine, u.Window, err)
	}
	return nil
}
