package router

import (
	"context"
	"testing"
	"time"

	"github.com/viminizer/germes/internal/engine"
)

// titleEngine is an engine that can name sessions.
type titleEngine struct {
	fakeEngine
	answer string
	asked  int
}

func (e *titleEngine) CreateTitle(context.Context, string) (string, error) {
	e.asked++
	return e.answer, nil
}

func TestTitleComesFromTheFirstFreeEngineThatCanName(t *testing.T) {
	limited := &titleEngine{fakeEngine: fakeEngine{name: "a"}, answer: `{"title": "Wrong engine"}`}
	cannot := &fakeEngine{name: "b"}
	free := &titleEngine{fakeEngine: fakeEngine{name: "c"}, answer: "```json\n{\"title\": \"Add dark mode.\"}\n```"}
	st := &fakeStore{limits: map[string]time.Time{"a": now.Add(time.Hour)}}
	r := newRouter(t, st, nil, engine.Engine(limited), cannot, free)
	title, err := r.CreateTitle(context.Background(), "please add dark mode to the settings page")
	if err != nil || title != "Add dark mode" || limited.asked != 0 {
		t.Fatalf("title %q, err %v, limited engine asked %d times", title, err, limited.asked)
	}
}

func TestAnswersThatAreNotTitlesAreRefused(t *testing.T) {
	for _, answer := range []string{
		"I'm Claude, an AI assistant made by Anthropic.",
		`{"title": "Sure, here is how you can add dark mode to the settings page in a few steps"}`,
		`{"title": "Fix login button on mobile"}`,
		`{"title": ""}`,
	} {
		e := &titleEngine{fakeEngine: fakeEngine{name: "a"}, answer: answer}
		if title, err := newRouter(t, &fakeStore{}, nil, e).CreateTitle(context.Background(), "x"); err == nil {
			t.Errorf("%q: got title %q, want an error", answer, title)
		}
	}
}
