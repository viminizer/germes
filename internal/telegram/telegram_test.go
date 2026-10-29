package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/viminizer/germes/internal/api"
)

const (
	chatID = -1001
	kevin  = 42
)

// fakeAPI is a Bot API: it hands out updates once and records what the bot
// sends.
type fakeAPI struct {
	mu      sync.Mutex
	updates []update
	sent    []map[string]any // sendMessage params
	methods []string         // every method called
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var params map[string]any
	json.Unmarshal(body, &params)
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	f.mu.Lock()
	f.methods = append(f.methods, method)
	f.mu.Unlock()
	result := any(true)
	switch method {
	case "getUpdates":
		f.mu.Lock()
		ups := f.updates
		f.updates = nil
		f.mu.Unlock()
		if len(ups) == 0 {
			time.Sleep(20 * time.Millisecond)
		}
		result = ups
	case "sendMessage":
		f.mu.Lock()
		f.sent = append(f.sent, params)
		f.mu.Unlock()
	}
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

func (f *fakeAPI) push(u ...update) {
	f.mu.Lock()
	f.updates = append(f.updates, u...)
	f.mu.Unlock()
}

// waitSent waits for a sent message containing text, and returns its topic.
func (f *fakeAPI) waitSent(t *testing.T, text string) int64 {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		for _, p := range f.sent {
			if strings.Contains(p["text"].(string), text) {
				thread, _ := p["message_thread_id"].(float64)
				f.mu.Unlock()
				return int64(thread)
			}
		}
		f.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the bot never sent %q", text)
	return 0
}

// fakeService records messages and streams the events the test pushes.
type fakeService struct {
	api.Service // calls not used here panic
	mu          sync.Mutex
	messages    []api.MessageRequest
	events      chan api.Event
}

func (s *fakeService) SendMessage(ctx context.Context, r api.MessageRequest) (api.MessageResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, r)
	id := r.SessionID
	if id == "" {
		id = "s1"
	}
	return api.MessageResult{SessionID: id}, nil
}

func (s *fakeService) Watch(ctx context.Context, f api.WatchFilter) (<-chan api.Event, error) {
	return s.events, nil
}

func (s *fakeService) GetSession(ctx context.Context, id string) (api.Session, error) {
	return api.Session{ID: id, Status: api.StatusIdle}, nil
}

func (s *fakeService) GetDaemon(ctx context.Context) (api.Daemon, error) {
	return api.Daemon{Version: "9.9.9"}, nil
}

func (s *fakeService) ListJobs(ctx context.Context, f api.JobFilter) (api.Page[api.Job], error) {
	return api.Page[api.Job]{Items: []api.Job{{Name: "nightly", Dir: "/elsewhere", Runs: 3}}}, nil
}

func (s *fakeService) ListSessions(ctx context.Context, f api.SessionFilter) (api.Page[api.Session], error) {
	return api.Page[api.Session]{}, nil
}

func (s *fakeService) waitMessages(t *testing.T, n int) []api.MessageRequest {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		got := append([]api.MessageRequest(nil), s.messages...)
		s.mu.Unlock()
		if len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("want %d messages to Germes", n)
	return nil
}

func msg(id, thread, from int64, text string) update {
	return update{ID: id, Message: &message{ID: id, ThreadID: thread, IsTopic: thread != 0, From: &user{ID: from}, Chat: chat{ID: chatID}, Text: text}}
}

func TestTopicsRouteToSessions(t *testing.T) {
	fake := &fakeAPI{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	svc := &fakeService{events: make(chan api.Event, 10)}
	project := t.TempDir()
	tg := New(svc, Options{Token: "SECRET", ChatID: chatID, UserID: kevin, APIBase: srv.URL,
		StatePath: filepath.Join(t.TempDir(), "telegram.json")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tg.Run(ctx)

	fake.push(
		msg(1, 0, 999, "I am not Kevin"),      // ignored
		msg(2, 0, kevin, "what is running?"),  // General: the overseer
		msg(3, 7, kevin, "/project "+project), // topic 7: bound
		msg(4, 7, kevin, "add dark mode"),     // its project's new session
		msg(5, 7, kevin, "and a toggle"),      // the same session
		update{ID: 6, Message: &message{ID: 6, ThreadID: 2, From: &user{ID: kevin}, Chat: chat{ID: chatID}, Text: "a reply in General"}},
	)
	got := svc.waitMessages(t, 4)
	if got[0].SessionID != api.OverseerID || got[0].Text != "what is running?" {
		t.Fatalf("General went to %+v", got[0])
	}
	if got[1].Dir != project || got[1].SessionID != "" || got[2].SessionID != "s1" {
		t.Fatalf("topic messages: %+v, %+v", got[1], got[2])
	}
	if got[3].SessionID != api.OverseerID {
		t.Fatalf("a reply in General went to %+v", got[3])
	}
	if fake.waitSent(t, "Bound to "+project) != 7 {
		t.Fatal("the bind reply went to the wrong topic")
	}

	// The answer goes back to the topic that asked; a job notice to the
	// project's topic.
	svc.events <- api.Event{SessionID: "s1", Kind: api.KindText, Text: "Dark mode is in."}
	svc.events <- api.Event{SessionID: "s1", Kind: api.KindTurnDone}
	if fake.waitSent(t, "Dark mode is in.") != 7 {
		t.Fatal("the answer went to the wrong topic")
	}
	svc.events <- api.Event{SessionID: "s1", Kind: api.KindNotice, Text: "job x is done"}
	if fake.waitSent(t, "job x is done") != 7 {
		t.Fatal("the notice went to the wrong topic")
	}

	// /status tells the topic's project and session; a job elsewhere is
	// not this topic's. The command menu was set.
	fake.push(msg(8, 7, kevin, "/status"))
	fake.waitSent(t, "Project: "+project)
	fake.mu.Lock()
	for _, p := range fake.sent {
		if strings.Contains(p["text"].(string), "nightly") {
			t.Error("status listed another project's job")
		}
	}
	menu := slices.Contains(fake.methods, "setMyCommands")
	fake.mu.Unlock()
	if !menu {
		t.Error("no command menu set")
	}

	// The binding survives a restart.
	var again *Telegram
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if again = New(svc, Options{StatePath: tg.opts.StatePath}); again.state.Offset == 9 {
			break // saved just after the last reply
		}
	}
	if again.state.Topics[7].Project != project || again.state.Topics[7].SessionID != "s1" || again.state.Offset != 9 {
		t.Fatalf("saved state: %+v", again.state)
	}
}

func TestErrorsHideTheToken(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close() // nothing listens: the request fails
	b := &bot{base: srv.URL + "/botSECRET", http: &http.Client{}}
	err := b.call(context.Background(), "getMe", map[string]any{}, nil)
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("error %v", err)
	}
}

func TestSplitKeepsLimits(t *testing.T) {
	long := strings.Repeat("line of text\n", 1000)
	parts := split(long, maxText)
	if len(parts) < 2 {
		t.Fatal("not split")
	}
	for _, p := range parts {
		if len([]rune(p)) > maxText {
			t.Fatalf("part of %d characters", len([]rune(p)))
		}
	}
	if strings.Join(parts, "\n") != strings.TrimSpace(long) {
		t.Fatal("text lost in splitting")
	}
}

func TestBindButtonsWhenSeveralMatch(t *testing.T) {
	fake := &fakeAPI{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	code := t.TempDir()
	for _, dir := range []string{"a/app", "b/app"} {
		if err := os.MkdirAll(filepath.Join(code, dir, ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	tg := New(&fakeService{}, Options{ChatID: chatID, UserID: kevin, APIBase: srv.URL, CodeDirs: []string{code},
		StatePath: filepath.Join(t.TempDir(), "telegram.json")})
	ctx := context.Background()
	tg.handle(ctx, msg(1, 9, kevin, "/bind app"))
	fake.waitSent(t, "Which project?")
	fake.mu.Lock()
	rows := fake.sent[0]["reply_markup"].(map[string]any)["inline_keyboard"].([]any)
	second := rows[1].([]any)[0].(map[string]any)
	fake.mu.Unlock()
	tg.handle(ctx, update{ID: 2, Callback: &callback{ID: "c", From: user{ID: kevin},
		Message: &message{ThreadID: 9, IsTopic: true, Chat: chat{ID: chatID}}, Data: second["callback_data"].(string)}})
	if got := tg.state.Topics[9].Project; got != second["text"] {
		t.Fatalf("bound %q, picked %q", got, second["text"])
	}
}
