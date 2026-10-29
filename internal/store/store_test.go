package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/viminizer/germes/internal/api"
)

func TestSessionsAndEvents(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "germes.db")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0)
	sess := api.Session{ID: "abc123", Title: "t", Project: "/p", Dir: "/p", Status: api.StatusIdle,
		ResumeIDs: map[string]string{"claude-cli": "x"}, CreatedAt: now, UpdatedAt: now}
	if err := s.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	for seq := int64(1); seq <= 3; seq++ {
		ev := api.Event{SessionID: sess.ID, Seq: seq, Time: now, Kind: api.KindToolCall, Tool: &api.ToolCall{ID: "t"}}
		if err := s.CreateEvent(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateEvent(ctx, api.Event{SessionID: sess.ID, Seq: 3, Time: now, Kind: api.KindText}); err == nil {
		t.Fatal("duplicate seq was saved")
	}

	page, err := s.ListEvents(ctx, api.EventFilter{SessionID: sess.ID, AfterSeq: 1, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Seq != 2 || page.Next != "2" || page.Items[0].Tool.ID != "t" {
		t.Fatalf("page = %+v", page)
	}
	if last, _ := s.GetLastSeq(ctx, sess.ID); last != 3 {
		t.Fatalf("last seq = %d", last)
	}

	// Reopening runs no migration twice and keeps the data.
	s.Close()
	s, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSession(ctx, sess.ID)
	if err != nil || got.ResumeIDs["claude-cli"] != "x" {
		t.Fatalf("got %+v, %v", got, err)
	}

	if err := s.DeleteSession(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	var apiErr *api.Error
	if _, err := s.GetSession(ctx, sess.ID); !errors.As(err, &apiErr) || apiErr.Code != api.CodeNotFound {
		t.Fatalf("after delete: %v", err)
	}
	if last, _ := s.GetLastSeq(ctx, sess.ID); last != 0 {
		t.Fatalf("events kept after delete: last seq %d", last)
	}
}
