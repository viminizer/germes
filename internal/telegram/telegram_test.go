package telegram

import (
	"context"
	"encoding/json"
	"fmt"
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

// fakeAPI is a Bot API: it hands out updates once, serves every file as
// fileBody, and records what the bot sends.
type fakeAPI struct {
	mu      sync.Mutex
	updates []update
	sent    []map[string]any // sendMessage params
	docs    []sentDoc        // sendDocument uploads
	reacted []string         // setMessageReaction calls, as "<message id> <emoji>"
	methods []string         // every method called
}

type sentDoc struct {
	thread, name, content string
}

const fileBody = "the file's bytes"

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.URL.Path, "/file/bot") {
		io.WriteString(w, fileBody)
		return
	}
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	f.mu.Lock()
	f.methods = append(f.methods, method)
	f.mu.Unlock()
	var params map[string]any
	if method == "sendDocument" {
		f.recordDoc(r)
	} else {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &params)
	}
	result := any(true)
	switch method {
	case "getFile":
		result = map[string]any{"file_path": "documents/file_1"}
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
	case "setMessageReaction":
		emoji := params["reaction"].([]any)[0].(map[string]any)["emoji"]
		f.mu.Lock()
		f.reacted = append(f.reacted, fmt.Sprintf("%v %v", params["message_id"], emoji))
		f.mu.Unlock()
	}
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

func (f *fakeAPI) recordDoc(r *http.Request) {
	r.ParseMultipartForm(1 << 20)
	part, head, err := r.FormFile("document")
	if err != nil {
		return
	}
	content, _ := io.ReadAll(part)
	f.mu.Lock()
	f.docs = append(f.docs, sentDoc{thread: r.FormValue("message_thread_id"), name: head.Filename, content: string(content)})
	f.mu.Unlock()
}

func (f *fakeAPI) waitDoc(t *testing.T) sentDoc {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		f.mu.Lock()
		if len(f.docs) > 0 {
			d := f.docs[0]
			f.mu.Unlock()
			return d
		}
		f.mu.Unlock()
	}
	t.Fatal("the bot never sent a document")
	return sentDoc{}
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
	working     map[string]bool // sessions GetSession reports working; others are idle
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.working[id] {
		return api.Session{ID: id, Status: api.StatusWorking}, nil
	}
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

// startBot runs a bot against a fake Bot API and a fake Germes.
func startBot(t *testing.T) (*fakeAPI, *fakeService, *Telegram) {
	svc := &fakeService{events: make(chan api.Event, 10)}
	fake, tg := startBotFrom(t, svc, filepath.Join(t.TempDir(), "telegram.json"))
	return fake, svc, tg
}

// startBotFrom runs a bot on a saved state, as after a daemon hand-over.
func startBotFrom(t *testing.T, svc *fakeService, statePath string) (*fakeAPI, *Telegram) {
	fake := &fakeAPI{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	tg := New(svc, Options{Token: "SECRET", ChatID: chatID, UserID: kevin, APIBase: srv.URL,
		StatePath: statePath, FilesDir: t.TempDir()})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go tg.Run(ctx)
	return fake, tg
}

func TestFileSentToTheBotReachesTheSession(t *testing.T) {
	fake, svc, _ := startBot(t)
	u := msg(1, 0, kevin, "")
	u.Message.Caption = "what is in this log?"
	u.Message.Document = &file{ID: "f1", UniqueID: "u1", Name: "../app.log"}
	fake.push(u)

	got := svc.waitMessages(t, 1)[0].Text
	caption, rest, ok := strings.Cut(got, "\n\n[file from Telegram: ")
	if !ok || caption != "what is in this log?" {
		t.Fatalf("message to the session: %q", got)
	}
	path := strings.TrimSuffix(rest, "]")
	if filepath.Base(path) != "u1-app.log" {
		t.Fatalf("saved as %s", path)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != fileBody {
		t.Fatalf("saved file: %q, %v", b, err)
	}
}

func TestSendFileUploadsToTheTopicThatAsked(t *testing.T) {
	fake, svc, tg := startBot(t)
	project := t.TempDir()
	fake.push(msg(1, 7, kevin, "/bind "+project), msg(2, 7, kevin, "send me the report"))
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		tg.mu.Lock()
		_, asked := tg.state.ReplyTo["s1"]
		tg.mu.Unlock()
		if asked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the topic never asked the session")
		}
	}
	report := filepath.Join(t.TempDir(), "report.txt")
	if err := os.WriteFile(report, []byte(fileBody), 0o600); err != nil {
		t.Fatal(err)
	}
	svc.events <- api.Event{SessionID: "s1", Kind: api.KindToolCall, Tool: &api.ToolCall{ID: "c1", Name: api.ToolSendFile}}
	svc.events <- api.Event{SessionID: "s1", Kind: api.KindToolResult, Tool: &api.ToolCall{ID: "c1", Output: report}}

	if d := fake.waitDoc(t); d.thread != "7" || d.name != "report.txt" || d.content != fileBody {
		t.Fatalf("uploaded %+v", d)
	}
}

func TestMessagesToSessionsGetEyes(t *testing.T) {
	fake, svc, _ := startBot(t)
	withFile := msg(3, 0, kevin, "")
	withFile.Message.Document = &file{ID: "f1", UniqueID: "u1", Name: "a.txt"}
	fake.push(msg(1, 0, kevin, "what is running?"), msg(2, 0, kevin, "/help"), withFile)
	fake.waitSent(t, "/bind") // the /help answer
	svc.waitMessages(t, 2)    // the text and the file reached the overseer
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if want := []string{"1 👀", "3 👀"}; !slices.Equal(fake.reacted, want) {
		t.Fatalf("reactions %q, want %q (a command gets none)", fake.reacted, want)
	}
}

// savedState writes a telegram.json in which the session waits for its
// answer in the topic.
func savedState(t *testing.T, session string, thread int64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "telegram.json")
	b, _ := json.Marshal(state{ReplyTo: map[string]int64{session: thread}})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAnswerAfterAHandOverReachesTheTopic(t *testing.T) {
	svc := &fakeService{events: make(chan api.Event, 10), working: map[string]bool{"s1": true}}
	fake, _ := startBotFrom(t, svc, savedState(t, "s1", 7))
	svc.events <- api.Event{SessionID: "s1", Kind: api.KindText, Text: "Done in the old daemon."}
	svc.events <- api.Event{SessionID: "s1", Kind: api.KindTurnDone}
	if fake.waitSent(t, "Done in the old daemon.") != 7 {
		t.Fatal("the answer went to the wrong topic")
	}
}

func TestStatusGoesOnlyToTheTopicThatAsked(t *testing.T) {
	svc := &fakeService{events: make(chan api.Event, 10), working: map[string]bool{"s1": true}}
	fake, _ := startBotFrom(t, svc, savedState(t, "s1", 7))
	svc.events <- api.Event{SessionID: "s2", Kind: api.KindStatus, Text: "retry for a TUI session"} // nobody asked from Telegram
	svc.events <- api.Event{SessionID: "s1", Kind: api.KindStatus, Text: "Trying again in 6s"}
	if fake.waitSent(t, "⏳ Trying again in 6s") != 7 {
		t.Fatal("the status went to the wrong topic")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, p := range fake.sent {
		if strings.Contains(p["text"].(string), "TUI session") {
			t.Fatal("a status of a session not asked from Telegram was sent")
		}
	}
}

func TestAnswerLostWhileNoBotWatchedIsTold(t *testing.T) {
	svc := &fakeService{events: make(chan api.Event, 10)} // s1 is idle: its turn ended unseen
	statePath := savedState(t, "s1", 7)
	fake, _ := startBotFrom(t, svc, statePath)
	if fake.waitSent(t, "the answer was lost") != 7 {
		t.Fatal("the notice went to the wrong topic")
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, waiting := New(svc, Options{StatePath: statePath}).state.ReplyTo["s1"]; !waiting {
			return // forgotten, so the next start does not tell it again
		}
		if time.Now().After(deadline) {
			t.Fatal("the lost session is still saved as waiting")
		}
	}
}

// A long outage repeats its limit line every cooldown: the topic hears it
// once per turn.
func TestLimitLinesOncePerTurn(t *testing.T) {
	svc := &fakeService{events: make(chan api.Event, 10), working: map[string]bool{"s1": true}}
	fake, _ := startBotFrom(t, svc, savedState(t, "s1", 7))
	svc.events <- api.Event{SessionID: "s1", Kind: api.KindLimitHit, Text: "codex: overloaded; tried again after 12:01"}
	svc.events <- api.Event{SessionID: "s1", Kind: api.KindLimitHit, Text: "every engine is limited or unavailable; paused until 12:03"}
	svc.events <- api.Event{SessionID: "s1", Kind: api.KindText, Text: "Done after the outage."}
	svc.events <- api.Event{SessionID: "s1", Kind: api.KindTurnDone}
	fake.waitSent(t, "Done after the outage.")
	fake.mu.Lock()
	defer fake.mu.Unlock()
	n := 0
	for _, p := range fake.sent {
		if strings.HasPrefix(p["text"].(string), "⚠") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d limit lines; want 1 per turn", n)
	}
}
