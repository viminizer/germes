package loop

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/viminizer/germes/internal/llm"
)

func TestPruneKeepsHowOldOutputsEnded(t *testing.T) {
	long := strings.Repeat("log line\n", 2000)
	cases := []struct {
		name, output string
		keeps        []string
	}{
		{"failed tests", "=== RUN TestX\n" + long + "FAIL\tgithub.com/x/y\n[exit code 1]",
			[]string{"=== RUN TestX", "FAIL\tgithub.com/x/y\n[exit code 1]"}},
		{"passed tests", "=== RUN TestX\n" + long + "ok\tgithub.com/x/y\t0.4s",
			[]string{"ok\tgithub.com/x/y\t0.4s"}},
		{"refused write", "blocked by config (Write(**/.env*)). Do not try to work around it; ask the user.",
			[]string{"blocked by config (Write(**/.env*)). Do not try to work around it; ask the user."}},
		{"saved output", "start\n" + long + "\n\n[Germes] The output is 90000 characters; the middle is not shown. " +
			"The full output is in /tmp/out/1-Bash.txt: read parts of it with Read (offset, limit) or search it with Grep.\n\n" + long + "end",
			[]string{"start", "/tmp/out/1-Bash.txt", "end"}},
	}
	var msgs []llm.Message
	for _, c := range cases {
		msgs = append(msgs, llm.Message{Role: llm.RoleTool, Text: c.output})
	}
	msgs = append(msgs, llm.Message{Role: llm.RoleTool, Text: long}) // the newest

	prune(msgs, 1)
	for i, c := range cases {
		got := msgs[i].Text
		for _, want := range c.keeps {
			if !strings.Contains(got, want) {
				t.Errorf("%s lost %q", c.name, want)
			}
		}
		if len(c.output) > 2000 && len(got) > 1500 {
			t.Errorf("%s was not shortened: %d characters", c.name, len(got))
		}
	}
	if msgs[len(msgs)-1].Text != long {
		t.Error("the newest output was pruned")
	}

	before := msgs[0].Text
	prune(msgs, 1)
	if msgs[0].Text != before {
		t.Errorf("pruning again changed an output:\n%s", msgs[0].Text)
	}
}

// windowModel is a model client that knows its context window.
type windowModel struct {
	fakeModel
	window int
	err    error
}

func (w *windowModel) GetContextWindow(context.Context) (int, error) { return w.window, w.err }

// History is compacted at half of the model's real window (500k for
// gpt-6.1-sol on Codex), not at half of a fixed 200k; when the client
// cannot say, 200k is used.
func TestLoopUsesTheModelsWindow(t *testing.T) {
	for _, c := range []struct {
		model *windowModel
		want  int
	}{
		{&windowModel{window: 500_000}, 500_000},
		{&windowModel{err: errors.New("model list down")}, 200_000},
	} {
		l := New(c.model, nil, nil, nil, Options{})
		if got := l.window(context.Background()); got != c.want {
			t.Errorf("window %d, want %d", got, c.want)
		}
	}
}
