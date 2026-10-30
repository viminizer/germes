package skill

import (
	"context"
	"strings"
	"testing"
)

func TestUpdate(t *testing.T) {
	ctx := context.Background()
	s := New(t.TempDir())
	if err := s.Save(ctx, Skill{Summary: Summary{Name: "deploy", Description: "Use to deploy."}, Body: "1. run make\n2. run make"}); err != nil {
		t.Fatal(err)
	}
	for old, want := range map[string]string{
		"run make":     "2 times",
		"run ship":     "not found",
		"name: deploy": "cannot change",
		"":             "non-empty",
	} {
		if err := s.Update(ctx, "deploy", old, "name: other"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", old, err, want)
		}
	}
	if err := s.Update(ctx, "deploy", "2. run make", "2. run make test"); err != nil {
		t.Fatal(err)
	}
	if sk, _ := s.Load(ctx, "deploy"); sk.Body != "1. run make\n2. run make test" || sk.Description != "Use to deploy." {
		t.Fatalf("got %+v", sk)
	}
}
