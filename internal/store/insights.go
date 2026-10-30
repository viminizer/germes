package store

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/viminizer/germes/internal/api"
)

// GetInsights counts sessions, turns, errors, tool calls and loaded skills
// since a time.
func (s *Store) GetInsights(ctx context.Context, since time.Time) (api.Insights, error) {
	in := api.Insights{Since: since}
	t := since.UnixNano()
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT session_id), COALESCE(SUM(kind = 'error'), 0)
		FROM events WHERE time >= ? AND kind IN ('turn_done', 'error')`, t).Scan(&in.Sessions, &in.Errors)
	if err != nil {
		return api.Insights{}, fmt.Errorf("insights: %w", err)
	}
	if in.Turns, err = s.listCounts(ctx, `SELECT engine, COUNT(*) FROM events
		WHERE time >= ? AND kind = 'turn_done' GROUP BY 1 ORDER BY 2 DESC, 1`, t); err != nil {
		return api.Insights{}, err
	}
	if in.Tools, err = s.listCounts(ctx, `SELECT json_extract(tool, '$.name'), COUNT(*) FROM events
		WHERE time >= ? AND kind = 'tool_call' GROUP BY 1 ORDER BY 2 DESC, 1`, t); err != nil {
		return api.Insights{}, err
	}
	if in.Skills, err = s.listSkillUses(ctx, t); err != nil {
		return api.Insights{}, err
	}
	return in, nil
}

// listCounts runs a query of (name, count) rows.
func (s *Store) listCounts(ctx context.Context, query string, args ...any) ([]api.Count, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("insights: %w", err)
	}
	defer rows.Close()
	var out []api.Count
	for rows.Next() {
		var c api.Count
		var name *string
		if err := rows.Scan(&name, &c.Count); err != nil {
			return nil, err
		}
		if name != nil {
			c.Name = *name
		}
		if c.Name == "" {
			c.Name = "unknown"
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// listSkillUses finds the skills loaded since t: skill_view, Claude's Skill
// tool, or a read of a SKILL.md.
func (s *Store) listSkillUses(ctx context.Context, t int64) ([]api.SkillUse, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT time, tool FROM events WHERE time >= ? AND kind = 'tool_call'
		AND json_extract(tool, '$.name') IN ('skill_view', 'Skill', 'Read')`, t)
	if err != nil {
		return nil, fmt.Errorf("insights: %w", err)
	}
	defer rows.Close()
	uses := map[string]*api.SkillUse{}
	for rows.Next() {
		var at int64
		var raw string
		if err := rows.Scan(&at, &raw); err != nil {
			return nil, err
		}
		var call struct {
			Args struct {
				Name     string `json:"name"`
				Skill    string `json:"skill"`
				FilePath string `json:"file_path"`
			} `json:"args"`
		}
		if json.Unmarshal([]byte(raw), &call) != nil {
			continue
		}
		name := call.Args.Name + call.Args.Skill
		if filepath.Base(call.Args.FilePath) == "SKILL.md" {
			name = filepath.Base(filepath.Dir(call.Args.FilePath))
		}
		if name == "" {
			continue
		}
		u := uses[name]
		if u == nil {
			u = &api.SkillUse{Name: name}
			uses[name] = u
		}
		u.Count++
		if when := time.Unix(0, at); when.After(u.LastUsed) {
			u.LastUsed = when
		}
	}
	out := make([]api.SkillUse, 0, len(uses))
	for _, u := range uses {
		out = append(out, *u)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	return out, rows.Err()
}
