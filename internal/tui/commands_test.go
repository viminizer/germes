package tui

import (
	"context"
	"slices"
	"testing"

	"github.com/viminizer/germes/internal/api"
)

// skillService lists two skills, one named like a built-in command, and
// records what is sent.
type skillService struct {
	api.Service // calls not used here panic
	sent        *[]api.MessageRequest
}

func (s skillService) ListSkills(context.Context) ([]api.Skill, error) {
	return []api.Skill{{Name: "failure-cases", Description: "what could break"}, {Name: "new", Description: "shadowed"}}, nil
}

func (s skillService) SendMessage(_ context.Context, r api.MessageRequest) (api.MessageResult, error) {
	*s.sent = append(*s.sent, r)
	return api.MessageResult{SessionID: "s1"}, nil
}

func newSkillModel(t *testing.T) (*model, *[]api.MessageRequest) {
	t.Helper()
	sent := &[]api.MessageRequest{}
	m := newModel(context.Background(), skillService{sent: sent}, Options{})
	msg, ok := m.loadSkills()().(skillsMsg)
	if !ok {
		t.Fatal("no skills loaded")
	}
	m.Update(msg)
	return m, sent
}

func TestSkillsAreSuggestedAfterCommandsAndNeverShadowThem(t *testing.T) {
	m, _ := newSkillModel(t)
	m.suggest.update("/", "")
	var labels []string
	for _, it := range m.suggest.items {
		labels = append(labels, it.Label)
	}
	if labels[len(labels)-1] != "/failure-cases" || slices.Index(labels, "/new") != 0 || len(labels) != len(commands)+1 {
		t.Fatalf("suggestions %v", labels)
	}
	m.suggest.update("/fa", "")
	if done, _ := m.suggest.complete("/fa"); done != "/failure-cases " {
		t.Fatalf("completed to %q", done)
	}
}

func TestSkillCommandSendsTheTextAsTyped(t *testing.T) {
	m, sent := newSkillModel(t)
	text := "/failure-cases look at\nthe parser"
	m.command(text)()
	if len(*sent) != 1 || (*sent)[0].Text != text || (*sent)[0].Skill != "failure-cases" {
		t.Fatalf("sent %+v", *sent)
	}
	m.command("/nope")
	if len(*sent) != 1 || len(m.pending) != 1 || m.pending[0].Text != "unknown command /nope (try /help)" {
		t.Fatalf("unknown command: %+v, sent %+v", m.pending, *sent)
	}
}
