// Package search has session_search: full-text search over past sessions.
package search

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/viminizer/germes/internal/text"
	"strings"

	"github.com/viminizer/germes/internal/api"
	"github.com/viminizer/germes/internal/tool"
)

// Searcher runs the search (the store).
type Searcher interface {
	ListEvents(ctx context.Context, f api.EventFilter) (api.Page[api.Event], error)
}

// New returns the session_search tool.
func New(s Searcher) []tool.Tool {
	return []tool.Tool{{
		Name: "session_search", Toolset: "search",
		Description: "Search past conversations (all sessions) for words. Use it to recall earlier work, decisions or things the user said.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`),
		Run: tool.NewRun(func(ctx context.Context, c tool.Call, a struct{ Query string }) (string, error) {
			if strings.TrimSpace(a.Query) == "" {
				return "", fmt.Errorf("empty query")
			}
			page, err := s.ListEvents(ctx, api.EventFilter{Query: a.Query, Limit: 20})
			if err != nil {
				return "", err
			}
			if len(page.Items) == 0 {
				return "no matches", nil
			}
			var b strings.Builder
			for _, ev := range page.Items {
				snippet := text.Shorten(strings.Join(strings.Fields(ev.Text), " "), 300)
				fmt.Fprintf(&b, "[session %s, %s, %s] %s\n", ev.SessionID, ev.Time.Format("2006-01-02 15:04"), ev.Kind, snippet)
			}
			return b.String(), nil
		}),
	}}
}
