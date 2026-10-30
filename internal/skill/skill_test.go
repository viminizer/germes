package skill

import (
	"context"
	"errors"
	"testing"

	"github.com/viminizer/germes/internal/text"
)

func TestUpdate(t *testing.T) {
	ctx := context.Background()
	s := New(t.TempDir())
	if err := s.Save(ctx, Skill{Summary: Summary{Name: "deploy", Description: "Use to deploy."}, Body: "1. run make\n2. run make"}); err != nil {
		t.Fatal(err)
	}
	for old, want := range map[string]error{
		"run make":     text.ErrManyMatches,
		"run ship":     text.ErrNoMatch,
		"name: deploy": ErrRename,
	} {
		if err := s.Update(ctx, "deploy", old, "name: other"); !errors.Is(err, want) {
			t.Errorf("%q: got %v, want %v", old, err, want)
		}
	}
	if err := s.Update(ctx, "deploy", "2. run make", "2. run make test"); err != nil {
		t.Fatal(err)
	}
	if sk, _ := s.Load(ctx, "deploy"); sk.Body != "1. run make\n2. run make test" || sk.Description != "Use to deploy." {
		t.Fatalf("got %+v", sk)
	}
}
