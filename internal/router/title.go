package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/viminizer/germes/internal/engine"
	"github.com/viminizer/germes/internal/text"
)

// Session titles, like Hermes (hermes/agent/title_generator.py).
const (
	maxTitleInput = 1000 // characters of the message the model sees
	maxTitleWords = 12   // longer is an answer, not a title
)

// titleExamples are the prompt's examples: a model that repeats one says
// nothing about the session.
var titleExamples = map[string]bool{"fix login button on mobile": true, "postgres connection pool exhaustion": true,
	"code changes": true}

// CreateTitle names a session from its first message, on the first engine
// that is not blocked and can name (the one the turn runs on, when it
// can). An error means no engine is free or the answer is not a title.
func (r *Router) CreateTitle(ctx context.Context, message string) (string, error) {
	t, err := r.titler()
	if err != nil {
		return "", err
	}
	answer, err := t.CreateTitle(ctx, "<message>\n"+text.Shorten(message, maxTitleInput)+"\n</message>")
	if err != nil {
		return "", err
	}
	return parseTitle(answer)
}

func (r *Router) titler() (engine.Titler, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.engines {
		if t, ok := e.(engine.Titler); ok {
			if until, _ := r.blocked(e.Name(), r.now()); until.IsZero() {
				return t, nil
			}
		}
	}
	return nil, errors.New("no engine free to name the session")
}

// parseTitle reads {"title": "..."} from the answer, even with text or a
// code fence around it, as one line without quotes or end punctuation.
func parseTitle(answer string) (string, error) {
	var v struct {
		Title string `json:"title"`
	}
	start, end := strings.Index(answer, "{"), strings.LastIndex(answer, "}")
	if start < 0 || end < start || json.Unmarshal([]byte(answer[start:end+1]), &v) != nil {
		return "", fmt.Errorf("not a title: %q", text.Shorten(answer, 80))
	}
	title := strings.Join(strings.Fields(v.Title), " ")
	title = strings.TrimRight(strings.Trim(title, `"'`), ".!?:; ")
	if n := len(strings.Fields(title)); n == 0 || n > maxTitleWords || titleExamples[strings.ToLower(title)] {
		return "", fmt.Errorf("not a title: %q", title)
	}
	return title, nil
}
