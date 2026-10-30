package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/viminizer/germes/internal/api"
)

func TestNewPreservesFilesystemPath(t *testing.T) {
	for _, relative := range []bool{false, true} {
		mode := "absolute"
		if relative {
			mode = "relative"
		}
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			if relative {
				t.Chdir(root)
			}
			for _, name := range []string{"plain", "home%2Farchive", "percent%", "query?_pragma=foreign_keys(0)&mode=memory", "fragment#", "space name", "日本語", "file:literal"} {
				t.Run(name, func(t *testing.T) {
					dir := filepath.Join(root, name)
					if err := os.MkdirAll(dir, 0700); err != nil {
						t.Fatal(err)
					}
					path := filepath.Join(dir, "germes.db")
					if relative {
						path = filepath.Join(name, "germes.db")
					}

					// A decoded %2F must not select another installation's database.
					var decoyPath string
					var decoyData []byte
					if name == "home%2Farchive" {
						decoyDir := filepath.Join(root, "home", "archive")
						if err := os.MkdirAll(decoyDir, 0700); err != nil {
							t.Fatal(err)
						}
						decoyPath = filepath.Join(decoyDir, "germes.db")
						decoy, err := New(decoyPath)
						if err != nil {
							t.Fatal(err)
						}
						if err := decoy.Close(); err != nil {
							t.Fatal(err)
						}
						decoyData, err = os.ReadFile(decoyPath)
						if err != nil {
							t.Fatal(err)
						}
					}

					ctx := context.Background()
					now := time.Unix(100, 0)
					for reopen := range 2 {
						s, err := New(path)
						if err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() { s.Close() })
						if _, err := os.Stat(filepath.Join(dir, "germes.db")); err != nil {
							t.Fatalf("requested database missing: %v", err)
						}
						if reopen == 0 {
							if err := s.CreateSession(ctx, api.Session{ID: "literal", Title: name, Project: "/p", Dir: "/p", Status: api.StatusIdle, CreatedAt: now, UpdatedAt: now}); err != nil {
								t.Fatal(err)
							}
						} else if got, err := s.GetSession(ctx, "literal"); err != nil || got.Title != name {
							t.Fatalf("reopened session = %+v, %v", got, err)
						}
						if err := s.Close(); err != nil {
							t.Fatal(err)
						}
					}
					if decoyPath != "" {
						got, err := os.ReadFile(decoyPath)
						if err != nil || string(got) != string(decoyData) {
							t.Fatalf("decoded-path database changed: %v", err)
						}
					}
				})
			}
		})
	}
}

func TestNewConnectionSettings(t *testing.T) {
	ctx := context.Background()
	s, err := New(filepath.Join(t.TempDir(), "settings?_pragma=foreign_keys(0)#.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// Hold both connections open to check settings on newly pooled connections,
	// not just the connection that ran migrations.
	for range 2 {
		conn, err := s.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		for _, pragma := range []struct {
			name string
			want string
		}{{"busy_timeout", "5000"}, {"journal_mode", "wal"}, {"foreign_keys", "1"}} {
			var got string
			if err := conn.QueryRowContext(ctx, "PRAGMA "+pragma.name).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != pragma.want {
				t.Fatalf("%s = %q, want %q", pragma.name, got, pragma.want)
			}
		}
	}
}

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

func TestSearchEvents(t *testing.T) {
	ctx := context.Background()
	s, err := New(filepath.Join(t.TempDir(), "germes.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Unix(100, 0)
	for _, id := range []string{"a", "b"} {
		s.CreateSession(ctx, api.Session{ID: id, Title: id, Project: "/p", Dir: "/p", Status: api.StatusIdle, CreatedAt: now, UpdatedAt: now})
	}
	s.CreateEvent(ctx, api.Event{SessionID: "a", Seq: 1, Time: now, Kind: api.KindUserMessage, Text: "the lighthouse keeper story"})
	s.CreateEvent(ctx, api.Event{SessionID: "b", Seq: 1, Time: now, Kind: api.KindText, Text: "deploy the API to staging"})
	page, err := s.ListEvents(ctx, api.EventFilter{Query: `lighthouse "keeper`})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].SessionID != "a" {
		t.Fatalf("search = %+v", page.Items)
	}
	s.DeleteSession(ctx, "a")
	if page, _ := s.ListEvents(ctx, api.EventFilter{Query: "lighthouse"}); len(page.Items) != 0 {
		t.Fatalf("deleted session still found: %+v", page.Items)
	}
}

func TestGetInsightsFindsSkills(t *testing.T) {
	ctx := context.Background()
	s, err := New(filepath.Join(t.TempDir(), "germes.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	old, now := time.Unix(100, 0), time.Unix(200, 0)
	s.CreateSession(ctx, api.Session{ID: "a", Title: "a", Project: "/p", Dir: "/p", Status: api.StatusIdle, CreatedAt: old, UpdatedAt: now})
	for i, c := range []struct {
		at   time.Time
		name string
		args string
	}{
		{now, "skill_view", `{"name":"deploy"}`},                  // Germes' loop
		{now, "Skill", `{"skill":"deploy"}`},                      // Claude's Skill tool
		{now, "Read", `{"file_path":"/h/skills/audit/SKILL.md"}`}, // read as a file
		{now, "Read", `{"file_path":"/p/main.go"}`},               // not a skill
		{old, "skill_view", `{"name":"stale"}`},                   // before since
	} {
		s.CreateEvent(ctx, api.Event{SessionID: "a", Seq: int64(i + 1), Time: c.at, Kind: api.KindToolCall,
			Tool: &api.ToolCall{ID: "x", Name: c.name, Args: []byte(c.args)}})
	}
	in, err := s.GetInsights(ctx, time.Unix(150, 0))
	if err != nil {
		t.Fatal(err)
	}
	want := []api.SkillUse{{Name: "deploy", Count: 2, LastUsed: now}, {Name: "audit", Count: 1, LastUsed: now}}
	if len(in.Skills) != 2 || in.Skills[0] != want[0] || in.Skills[1] != want[1] {
		t.Fatalf("skills = %+v", in.Skills)
	}
	if len(in.Tools) != 3 || in.Tools[0] != (api.Count{Name: "Read", Count: 2}) {
		t.Fatalf("tools = %+v", in.Tools)
	}
}
