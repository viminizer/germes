package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/viminizer/germes/internal/api"
)

// lifecycleService holds a turn-done handler immediately before it changes
// and saves ReplyTo. Startup's check still sees the saved turn as working.
type lifecycleService struct {
	fakeService
	watchContext chan context.Context
	finishing    chan struct{}
	release      chan struct{}
	checked      bool
}

func (s *lifecycleService) Watch(ctx context.Context, f api.WatchFilter) (<-chan api.Event, error) {
	s.watchContext <- ctx
	return s.fakeService.Watch(ctx, f)
}

func (s *lifecycleService) GetSession(ctx context.Context, id string) (api.Session, error) {
	if !s.checked {
		s.checked = true
		return api.Session{ID: id, Status: api.StatusWorking}, nil
	}
	close(s.finishing)
	<-s.release // model an in-flight call that takes time to unwind after cancellation
	return api.Session{ID: id, Status: api.StatusIdle}, nil
}

// lifecycleTransport holds both the poll and the typing request without a
// real network. The typing request deliberately finishes after cancellation.
type lifecycleTransport struct {
	polling       chan struct{}
	panicPoll     chan struct{}
	typingContext chan context.Context
	releaseTyping chan struct{}
}

func (tr *lifecycleTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	switch filepath.Base(r.URL.Path) {
	case "getUpdates":
		close(tr.polling)
		select {
		case <-r.Context().Done():
			return nil, r.Context().Err()
		case <-tr.panicPoll:
			panic("poll failed")
		}
	case "sendChatAction":
		tr.typingContext <- r.Context()
		<-tr.releaseTyping
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":true}`)), Header: make(http.Header)}, nil
}

// recoveryTransport records replies without a network; reaching the poll
// means startup has finished checking saved reply destinations.
type recoveryTransport struct {
	polling chan struct{}
	sent    []map[string]any
}

func (tr *recoveryTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	switch filepath.Base(r.URL.Path) {
	case "getUpdates":
		close(tr.polling)
		<-r.Context().Done()
		return nil, r.Context().Err()
	case "sendMessage":
		var params map[string]any
		if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
			return nil, err
		}
		tr.sent = append(tr.sent, params)
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":true}`)), Header: make(http.Header)}, nil
}

func TestRunPreservesPendingAnswerAfterRestart(t *testing.T) {
	for _, tc := range []struct {
		name    string
		session api.Session
	}{
		{name: "paused", session: api.Session{Status: api.StatusPaused}},
		{name: "working", session: api.Session{Status: api.StatusWorking}},
		{name: "idle_queued", session: api.Session{Status: api.StatusIdle, Queued: []string{"next message"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				path := savedState(t, "s1", 7)
				current := tc.session
				svc := &turnDoneService{fakeService: fakeService{events: make(chan api.Event, 10)}}
				svc.lookup = func(context.Context, string) (api.Session, error) { return current, nil }
				tr := &recoveryTransport{polling: make(chan struct{})}
				tg := New(svc, Options{ChatID: chatID, UserID: kevin, StatePath: path})
				tg.bot.http.Transport = tr
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan error, 1)
				go func() { done <- tg.Run(ctx) }()
				<-tr.polling
				synctest.Wait()

				if got := New(svc, Options{StatePath: path}).state.ReplyTo["s1"]; got != 7 {
					t.Errorf("pending session's saved reply topic = %d, want 7", got)
				}
				if len(tr.sent) != 0 {
					t.Errorf("startup falsely reported a lost answer: %v", tr.sent)
				}

				current = api.Session{Status: api.StatusWorking}
				svc.events <- api.Event{SessionID: "s1", Kind: api.KindStatus, Text: "Provider recovered; resuming."}
				synctest.Wait()
				current = api.Session{Status: api.StatusIdle}
				svc.events <- api.Event{SessionID: "s1", Kind: api.KindText, Text: "Done after recovery."}
				svc.events <- api.Event{SessionID: "s1", Kind: api.KindTurnDone}
				synctest.Wait()

				want := []string{"⏳ Provider recovered; resuming.", "Done after recovery."}
				if len(tr.sent) != len(want) {
					t.Errorf("recovery replies = %v, want %q", tr.sent, want)
				} else {
					for i, text := range want {
						if tr.sent[i]["text"] != text || tr.sent[i]["message_thread_id"] != float64(7) || tr.sent[i]["chat_id"] != float64(chatID) {
							t.Errorf("reply %d = %v, want %q in original chat/topic", i, tr.sent[i], text)
						}
					}
				}
				if _, waiting := New(svc, Options{StatePath: path}).state.ReplyTo["s1"]; waiting {
					t.Error("finished session is still saved as waiting")
				}
				cancel()
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			})
		})
	}
}

func TestRunJoinsChildren(t *testing.T) {
	for _, exit := range []string{"cancel", "panic"} {
		t.Run(exit, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "telegram.json")
				if err := os.WriteFile(path, []byte(`{"offset":42,"topics":{"7":{"project":"/project","session_id":"s1"}},"reply_to":{"s1":7}}`), 0o600); err != nil {
					t.Fatal(err)
				}
				svc := &lifecycleService{
					fakeService:  fakeService{events: make(chan api.Event)},
					watchContext: make(chan context.Context, 1), finishing: make(chan struct{}), release: make(chan struct{}),
				}
				tr := &lifecycleTransport{
					polling: make(chan struct{}), panicPoll: make(chan struct{}),
					typingContext: make(chan context.Context, 1), releaseTyping: make(chan struct{}),
				}
				tg := New(svc, Options{ChatID: chatID, UserID: kevin, StatePath: path})
				tg.bot.http.Transport = tr
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan struct{})
				var runErr error
				var panicked any
				go func() {
					defer close(done)
					defer func() { panicked = recover() }()
					runErr = tg.Run(ctx)
				}()
				<-tr.polling // startup has checked the waiting turn
				watchCtx := <-svc.watchContext
				time.Sleep(typingEvery) // synctest's virtual clock, not a wall-clock wait
				typingCtx := <-tr.typingContext
				svc.events <- api.Event{SessionID: "s1", Kind: api.KindTurnDone}
				<-svc.finishing

				if exit == "cancel" {
					cancel()
				} else {
					close(tr.panicPoll)
				}
				// Wait until every other goroutine is durably blocked: unlike a
				// short timeout, this proves Run has had a chance to return.
				synctest.Wait()
				if watchCtx.Err() != context.Canceled || typingCtx.Err() != context.Canceled {
					t.Errorf("children not canceled: watch=%v typing=%v", watchCtx.Err(), typingCtx.Err())
				}
				select {
				case <-done:
					t.Error("Run ended while the state-writing watcher and typing request were still in flight")
				default:
				}

				close(svc.release)
				synctest.Wait()
				again := New(&fakeService{}, Options{StatePath: path})
				if len(again.state.ReplyTo) != 0 || again.state.Offset != 42 || again.state.Topics[7].SessionID != "s1" {
					t.Errorf("watcher's final save missing or damaged: %+v", again.state)
				}
				select {
				case <-done:
					t.Error("Run ended while its typing request was still in flight")
				default:
				}
				close(tr.releaseTyping)
				<-done
				if runErr != nil || (exit == "panic" && panicked != "poll failed") || (exit == "cancel" && panicked != nil) {
					t.Errorf("Run result: err=%v panic=%v", runErr, panicked)
				}
				if exit == "panic" && ctx.Err() != nil {
					t.Error("Run canceled its caller's context")
				}
				cancel() // also stop the old implementation's leaked children on failure
				synctest.Wait()
			})
		})
	}
}

// turnDoneService controls the lookup after startup without changing Watch's
// context-owned stream contract.
type turnDoneService struct {
	fakeService
	lookup func(context.Context, string) (api.Session, error)
}

func (s *turnDoneService) GetSession(ctx context.Context, id string) (api.Session, error) {
	return s.lookup(ctx, id)
}

func TestRunPreservesQueuedAnswerOnCanceledTurnDone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		path := savedState(t, "s1", 7)
		finishing := make(chan struct{})
		startup := true
		svc := &turnDoneService{fakeService: fakeService{events: make(chan api.Event)}}
		svc.lookup = func(ctx context.Context, id string) (api.Session, error) {
			if startup {
				startup = false
				return api.Session{ID: id, Status: api.StatusWorking, Queued: []string{"next message"}}, nil
			}
			close(finishing)
			<-ctx.Done() // hand-over cancels a lookup while the next turn is still pending
			return api.Session{}, fmt.Errorf("get session: %w", ctx.Err())
		}
		tr := &recoveryTransport{polling: make(chan struct{})}
		tg := New(svc, Options{ChatID: chatID, UserID: kevin, StatePath: path})
		tg.bot.http.Transport = tr
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- tg.Run(ctx) }()
		<-tr.polling
		svc.events <- api.Event{SessionID: "s1", Kind: api.KindTurnDone}
		<-finishing
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if got := New(svc, Options{StatePath: path}).state.ReplyTo["s1"]; got != 7 {
			t.Errorf("canceled lookup erased saved reply topic: got %d, want 7", got)
		}

		// The queued turn is now running in the replacement daemon. Load the
		// destination from disk, not from the old gateway's memory.
		next := &fakeService{events: make(chan api.Event, 2), statuses: map[string]api.Status{"s1": api.StatusWorking}}
		replies := &recoveryTransport{polling: make(chan struct{})}
		again := New(next, Options{ChatID: chatID, UserID: kevin, StatePath: path})
		again.bot.http.Transport = replies
		nextCtx, stop := context.WithCancel(context.Background())
		defer stop()
		go func() { done <- again.Run(nextCtx) }()
		<-replies.polling
		synctest.Wait()
		if len(replies.sent) != 0 {
			t.Errorf("replacement falsely reported a lost answer: %v", replies.sent)
		}
		next.mu.Lock()
		next.statuses["s1"] = api.StatusIdle
		next.mu.Unlock()
		next.events <- api.Event{SessionID: "s1", Kind: api.KindText, Text: "Queued answer after hand-over."}
		next.events <- api.Event{SessionID: "s1", Kind: api.KindTurnDone}
		synctest.Wait()
		if len(replies.sent) != 1 {
			t.Errorf("queued answer replies = %v, want one", replies.sent)
		} else if p := replies.sent[0]; p["text"] != "Queued answer after hand-over." || p["message_thread_id"] != float64(7) || p["chat_id"] != float64(chatID) {
			t.Errorf("queued answer went to wrong destination: %v", p)
		}
		if _, waiting := New(next, Options{StatePath: path}).state.ReplyTo["s1"]; waiting {
			t.Error("confirmed finished session is still saved as waiting")
		}
		stop()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

func TestRunTurnDoneReplyRetention(t *testing.T) {
	for _, tc := range []struct {
		name    string
		session api.Session
		err     error
		keep    bool
	}{
		{name: "transient", err: api.NewError(api.CodeUnavailable, "session lookup unavailable"), keep: true},
		{name: "not_found", err: fmt.Errorf("get session: %w", api.NewError(api.CodeNotFound, "session gone"))},
		{name: "idle", session: api.Session{Status: api.StatusIdle}},
		{name: "queued", session: api.Session{Status: api.StatusIdle, Queued: []string{"next message"}}, keep: true},
		{name: "working", session: api.Session{Status: api.StatusWorking}, keep: true},
		{name: "paused", session: api.Session{Status: api.StatusPaused}, keep: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				path := savedState(t, "s1", 7)
				startup := true
				svc := &turnDoneService{fakeService: fakeService{events: make(chan api.Event)}}
				svc.lookup = func(ctx context.Context, id string) (api.Session, error) {
					if startup {
						startup = false
						return api.Session{ID: id, Status: api.StatusWorking}, nil
					}
					return tc.session, tc.err
				}
				tr := &recoveryTransport{polling: make(chan struct{})}
				tg := New(svc, Options{ChatID: chatID, UserID: kevin, StatePath: path})
				tg.bot.http.Transport = tr
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan error, 1)
				go func() { done <- tg.Run(ctx) }()
				<-tr.polling
				svc.events <- api.Event{SessionID: "s1", Kind: api.KindTurnDone}
				synctest.Wait()
				if got, waiting := New(svc, Options{StatePath: path}).state.ReplyTo["s1"]; waiting != tc.keep || (waiting && got != 7) {
					t.Errorf("saved reply topic = %d, waiting = %v; want waiting = %v", got, waiting, tc.keep)
				}
				cancel()
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			})
		})
	}
}

type pollTransport struct {
	recoveryTransport
	polls   chan struct{}
	updates chan []update
}

func (tr *pollTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if filepath.Base(r.URL.Path) != "getUpdates" {
		return tr.recoveryTransport.RoundTrip(r)
	}
	tr.polls <- struct{}{}
	select {
	case <-r.Context().Done():
		return nil, r.Context().Err()
	case updates := <-tr.updates:
		b, err := json.Marshal(map[string]any{"ok": true, "result": updates})
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(b))), Header: make(http.Header)}, nil
	}
}
func TestRunNewMessageSurvivesOlderCompletionLookup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "telegram.json")
		b, err := json.Marshal(state{Topics: map[int64]topic{7: {Project: "/project", SessionID: "s1"}}, ReplyTo: map[string]int64{"s1": 7}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
		lookup, release := make(chan struct{}), make(chan struct{})
		startup := true
		var finished atomic.Bool
		svc := &turnDoneService{fakeService: fakeService{events: make(chan api.Event, 3)}}
		svc.lookup = func(context.Context, string) (api.Session, error) {
			if startup {
				startup = false
				return api.Session{ID: "s1", Status: api.StatusWorking}, nil
			}
			select {
			case <-lookup:
			default:
				close(lookup)
				<-release
				return api.Session{ID: "s1", Status: api.StatusIdle}, nil // first response captured before new message
			}
			svc.mu.Lock()
			accepted := len(svc.messages) > 0
			svc.mu.Unlock()
			if accepted && !finished.Load() {
				return api.Session{ID: "s1", Status: api.StatusWorking}, nil // the accepted message's turn
			}
			return api.Session{ID: "s1", Status: api.StatusIdle}, nil
		}
		tr := &pollTransport{polls: make(chan struct{}, 10), updates: make(chan []update)}
		tg := New(svc, Options{ChatID: chatID, UserID: kevin, StatePath: path})
		tg.bot.http.Transport = tr
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- tg.Run(ctx) }()
		<-tr.polls
		svc.events <- api.Event{SessionID: "s1", Kind: api.KindTurnDone}
		<-lookup
		tr.updates <- []update{msg(1, 7, kevin, "next message")}
		<-tr.polls // poll handled the message and saved ReplyTo before issuing another poll
		if len(svc.messages) != 1 || svc.messages[0].SessionID != "s1" {
			t.Fatalf("new message not sent to s1: %+v", svc.messages)
		}
		close(release)
		synctest.Wait()
		if got := New(svc, Options{StatePath: path}).state.ReplyTo["s1"]; got != 7 {
			t.Errorf("older idle response erased newer message reply destination: %d", got)
		}
		finished.Store(true)
		svc.events <- api.Event{SessionID: "s1", Kind: api.KindText, Text: "answer to next message"}
		svc.events <- api.Event{SessionID: "s1", Kind: api.KindTurnDone}
		synctest.Wait()
		if len(tr.sent) != 1 || tr.sent[0]["text"] != "answer to next message" || tr.sent[0]["message_thread_id"] != float64(7) {
			t.Errorf("new answer lost or misrouted: sent=%v", tr.sent)
		}
		if _, waiting := New(svc, Options{StatePath: path}).state.ReplyTo["s1"]; waiting {
			t.Error("confirmed final completion left the reply destination saved")
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

// registrationService delays message acceptance while the real poller and
// watcher continue to exercise reply registration through Run.
type registrationService struct {
	turnDoneService
	send func(context.Context, api.MessageRequest) (api.MessageResult, error)
}

func (s *registrationService) SendMessage(ctx context.Context, r api.MessageRequest) (api.MessageResult, error) {
	return s.send(ctx, r)
}

func TestRunReplyRegistrationDuringSend(t *testing.T) {
	for _, name := range []string{"existing", "failed", "new", "deleted"} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				id := "s1"
				if name == "new" {
					id = ""
				}
				initial := state{Topics: map[int64]topic{8: {Project: "/project", SessionID: id}}, ReplyTo: map[string]int64{}}
				if id != "" {
					initial.ReplyTo[id] = 7 // a failed send must restore this older topic
				}
				path := filepath.Join(t.TempDir(), "telegram.json")
				b, err := json.Marshal(initial)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, b, 0o600); err != nil {
					t.Fatal(err)
				}
				started, release := make(chan api.MessageRequest, 1), make(chan struct{})
				svc := &registrationService{turnDoneService: turnDoneService{fakeService: fakeService{events: make(chan api.Event, 3)}}}
				startup := id != ""
				// An accepted message keeps the session busy until its turn
				// ends; for "failed", the older turn's successor still runs.
				var busy atomic.Bool
				busy.Store(name == "failed")
				svc.lookup = func(context.Context, string) (api.Session, error) {
					if startup {
						startup = false
						return api.Session{Status: api.StatusWorking}, nil
					}
					if busy.Load() {
						return api.Session{Status: api.StatusWorking}, nil
					}
					return api.Session{Status: api.StatusIdle}, nil
				}
				svc.send = func(ctx context.Context, r api.MessageRequest) (api.MessageResult, error) {
					if name == "deleted" && r.SessionID != "" {
						return api.MessageResult{}, api.NewError(api.CodeNotFound, "session deleted")
					}
					started <- r
					<-release
					if name == "failed" {
						return api.MessageResult{}, api.NewError(api.CodeUnavailable, "send unavailable")
					}
					busy.Store(true)
					return api.MessageResult{SessionID: "s1"}, nil
				}
				tr := &pollTransport{polls: make(chan struct{}, 10), updates: make(chan []update)}
				tg := New(svc, Options{ChatID: chatID, UserID: kevin, StatePath: path})
				tg.bot.http.Transport = tr
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan error, 1)
				go func() { done <- tg.Run(ctx) }()
				<-tr.polls
				tr.updates <- []update{msg(1, 8, kevin, "next message")}
				r := <-started
				if name == "new" || name == "deleted" {
					if r.SessionID != "" || r.Dir != "/project" {
						t.Errorf("new-session request = %+v", r)
					}
				} else {
					if r.SessionID != "s1" {
						t.Errorf("bound-session request = %+v", r)
					}
					// This lookup begins AFTER reservation, so a generation
					// comparison alone cannot prevent premature cleanup.
					svc.events <- api.Event{SessionID: "s1", Kind: api.KindTurnDone}
					synctest.Wait()
					tg.mu.Lock()
					got := tg.state.ReplyTo["s1"]
					tg.mu.Unlock()
					if got != 8 {
						t.Errorf("completion during send erased reservation: topic = %d", got)
					}
				}
				close(release)
				<-tr.polls
				synctest.Wait()
				want := int64(8)
				if name == "failed" {
					want = 7
				}
				again := New(svc, Options{StatePath: path})
				if got := again.state.ReplyTo["s1"]; got != want {
					t.Errorf("saved reply topic = %d, want %d", got, want)
				}
				if _, empty := again.state.ReplyTo[""]; empty {
					t.Error("registered an unknown session ID")
				}
				if name != "failed" {
					busy.Store(false)
					svc.events <- api.Event{SessionID: "s1", Kind: api.KindText, Text: "next answer"}
					svc.events <- api.Event{SessionID: "s1", Kind: api.KindTurnDone}
					synctest.Wait()
					if len(tr.sent) != 1 || tr.sent[0]["text"] != "next answer" || tr.sent[0]["message_thread_id"] != float64(8) {
						t.Errorf("registered answer = %v", tr.sent)
					}
					if _, waiting := New(svc, Options{StatePath: path}).state.ReplyTo["s1"]; waiting {
						t.Error("finished answer remains registered")
					}
				}
				cancel()
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			})
		})
	}
}

// Creation responses and the independent Watch stream have no delivery ordering.
type creationService struct {
	registrationService
	history  []api.Event
	failures int
}

func (s *creationService) ListEvents(_ context.Context, f api.EventFilter) (api.Page[api.Event], error) {
	if s.failures > 0 {
		s.failures--
		return api.Page[api.Event]{}, api.NewError(api.CodeUnavailable, "history temporarily unavailable")
	}
	var page api.Page[api.Event]
	for _, ev := range s.history {
		if ev.SessionID == f.SessionID && ev.Seq > f.AfterSeq {
			page.Items = append(page.Items, ev)
		}
	}
	// Force a page boundary between the answer and completion.
	if len(page.Items) > 1 {
		page.Items = page.Items[:1]
		page.Next = "more"
	}
	return page, nil
}
func TestRunCreationAnswerBeforeResponse(t *testing.T) {
	for _, binding := range []string{"new", "deleted"} {
		for _, kind := range []api.EventKind{api.KindText, api.KindError} {
			for _, pending := range []bool{false, true} {
				suffix := "/finished"
				if pending {
					suffix = "/continuing"
				}
				t.Run(binding+"/"+string(kind)+suffix, func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						path := filepath.Join(t.TempDir(), "telegram.json")
						id := ""
						if binding == "deleted" {
							id = "deleted"
						}
						b, err := json.Marshal(state{Topics: map[int64]topic{8: {Project: "/project", SessionID: id}}, ReplyTo: map[string]int64{}})
						if err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(path, b, 0600); err != nil {
							t.Fatal(err)
						}
						accepted, response := make(chan struct{}), make(chan struct{})
						svc := &creationService{failures: 1, registrationService: registrationService{turnDoneService: turnDoneService{fakeService: fakeService{events: make(chan api.Event, 10)}}}}
						status := api.StatusIdle
						if pending {
							status = api.StatusWorking
						}
						svc.lookup = func(context.Context, string) (api.Session, error) {
							return api.Session{ID: "created", Status: status}, nil
						}
						svc.send = func(_ context.Context, r api.MessageRequest) (api.MessageResult, error) {
							if r.SessionID != "" {
								return api.MessageResult{}, api.NewError(api.CodeNotFound, "deleted")
							}
							close(accepted)
							<-response
							return api.MessageResult{SessionID: "created"}, nil
						}
						tr := &pollTransport{polls: make(chan struct{}, 10), updates: make(chan []update)}
						tg := New(svc, Options{ChatID: chatID, UserID: kevin, StatePath: path})
						tg.bot.http.Transport = tr
						ctx, cancel := context.WithCancel(context.Background())
						defer cancel()
						done := make(chan error, 1)
						go func() { done <- tg.Run(ctx) }()
						<-tr.polls
						tr.updates <- []update{msg(1, 8, kevin, "first request")}
						<-accepted
						svc.history = []api.Event{{SessionID: "created", Seq: 1, Kind: kind, Text: "first result"}, {SessionID: "created", Seq: 2, Kind: api.KindTurnDone}}
						for _, ev := range svc.history {
							svc.events <- ev
						}
						svc.events <- api.Event{SessionID: "private", Seq: 1, Kind: api.KindText, Text: "private answer"}
						svc.events <- api.Event{SessionID: "private", Seq: 2, Kind: api.KindTurnDone}
						synctest.Wait() // watcher consumed completion before the creation response
						close(response)
						<-tr.polls
						synctest.Wait()
						if len(tr.sent) != 1 {
							t.Errorf("answer missing before any delayed live replay: %v", tr.sent)
						}
						if _, waiting := New(svc, Options{StatePath: path}).state.ReplyTo["created"]; waiting != pending {
							t.Errorf("creation waiting=%v, want %v", waiting, pending)
						}
						for _, ev := range svc.history {
							svc.events <- ev
						}
						synctest.Wait() // delayed live copies of replayed events
						want := "first result"
						if kind == api.KindError {
							want = "⚠ " + want
						}
						if len(tr.sent) != 1 || tr.sent[0]["text"] != want || tr.sent[0]["message_thread_id"] != float64(8) || tr.sent[0]["chat_id"] != float64(chatID) {
							t.Errorf("creation result lost, duplicated or misrouted: %v", tr.sent)
						}
						if pending {
							// Live duplicates must not replay the first result even
							// while a successor still owns the saved destination.
							status = api.StatusIdle
							svc.events <- api.Event{SessionID: "created", Seq: 3, Kind: api.KindText, Text: "continued result"}
							svc.events <- api.Event{SessionID: "created", Seq: 4, Kind: api.KindTurnDone}
							synctest.Wait()
							if len(tr.sent) != 2 || tr.sent[1]["text"] != "continued result" {
								t.Errorf("continuation result: %v", tr.sent)
							}
						}
						again := New(svc, Options{StatePath: path})
						if len(again.state.ReplyTo) != 0 || again.state.Topics[8].SessionID != "created" {
							t.Errorf("finished creation state: %+v", again.state)
						}
						cancel()
						if err := <-done; err != nil {
							t.Fatal(err)
						}
					})
				})
			}
		}
	}
}

type planNoticeService struct {
	creationService
	failed chan struct{}
	replay chan struct{}
}

func (s *planNoticeService) ListEvents(ctx context.Context, f api.EventFilter) (api.Page[api.Event], error) {
	if s.failures > 0 {
		s.failures--
		close(s.failed)
		return api.Page[api.Event]{}, api.NewError(api.CodeUnavailable, "temporary history outage")
	}
	select {
	case <-ctx.Done():
		return api.Page[api.Event]{}, ctx.Err()
	case <-s.replay:
	}
	return s.creationService.ListEvents(ctx, f)
}
func TestRunCreationPlanNoticeCannotOvertakeReplay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "telegram.json")
		if err := os.WriteFile(path, []byte(`{"topics":{"8":{"project":"/project"}}}`), 0600); err != nil {
			t.Fatal(err)
		}
		accepted, response := make(chan struct{}), make(chan struct{})
		svc := &planNoticeService{failed: make(chan struct{}), replay: make(chan struct{}), creationService: creationService{failures: 1, registrationService: registrationService{turnDoneService: turnDoneService{fakeService: fakeService{events: make(chan api.Event, 10)}}}}}
		svc.lookup = func(context.Context, string) (api.Session, error) {
			return api.Session{ID: "created", Status: api.StatusIdle}, nil
		}
		svc.send = func(_ context.Context, r api.MessageRequest) (api.MessageResult, error) {
			if !r.Plan || r.SessionID != "" || r.Dir != "/project" {
				t.Errorf("first plan request = %+v", r)
			}
			close(accepted)
			<-response
			return api.MessageResult{SessionID: "created"}, nil
		}
		tr := &pollTransport{polls: make(chan struct{}, 10), updates: make(chan []update)}
		tg := New(svc, Options{ChatID: chatID, UserID: kevin, StatePath: path})
		tg.bot.http.Transport = tr
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- tg.Run(ctx) }()
		<-tr.polls
		tr.updates <- []update{msg(1, 8, kevin, "/plan first request")}
		<-accepted
		// Production savePlan emits a durable notice between the final text and turn_done.
		svc.history = []api.Event{{SessionID: "created", Seq: 1, Kind: api.KindText, Text: "full plan answer"}, {SessionID: "created", Seq: 2, Kind: api.KindNotice, Text: "plan saved"}, {SessionID: "created", Seq: 3, Kind: api.KindTurnDone}}
		svc.events <- svc.history[0]
		synctest.Wait() // text beat response, so durable replay is required
		close(response)
		<-svc.failed
		svc.events <- svc.history[1]
		svc.events <- svc.history[1] // duplicate live notice must still be deduplicated
		synctest.Wait()              // globally routed notice arrives during replay backoff
		if len(tr.sent) != 1 || tr.sent[0]["text"] != "🔔 plan saved" {
			t.Errorf("notice during replay backoff = %v, want one immediate notice", tr.sent)
		}
		close(svc.replay) // order the assertion before replay can send its recovered answer
		time.Sleep(retryWait)
		<-tr.polls
		synctest.Wait()
		want := []string{"🔔 plan saved", "full plan answer"}
		if len(tr.sent) != len(want) {
			t.Errorf("creation answer lost or notice duplicated during stored replay: sent=%v", tr.sent)
		} else {
			for i, text := range want {
				if p := tr.sent[i]; p["text"] != text || p["message_thread_id"] != float64(8) || p["chat_id"] != float64(chatID) {
					t.Errorf("reply %d = %v, want %q in original chat/topic", i, p, text)
				}
			}
		}
		again := New(svc, Options{StatePath: path})
		if len(again.state.ReplyTo) != 0 || again.state.Topics[8].SessionID != "created" {
			t.Errorf("finished plan creation state: %+v", again.state)
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

// A queued turn can finish before the earlier turn's completion is handled
// (the answer waits on Telegram). Idle with nothing queued is then not the
// end: the later turn's stored events still have to reach the topic.
func TestRunLaterTurnFinishedBeforeEarlierCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		path := savedState(t, "s1", 7)
		svc := &creationService{registrationService: registrationService{turnDoneService: turnDoneService{fakeService: fakeService{events: make(chan api.Event, 5)}}}}
		svc.history = []api.Event{
			{SessionID: "s1", Seq: 1, Kind: api.KindText, Text: "first answer"},
			{SessionID: "s1", Seq: 2, Kind: api.KindTurnDone},
			{SessionID: "s1", Seq: 3, Kind: api.KindUserMessage, Text: "queued"},
			{SessionID: "s1", Seq: 4, Kind: api.KindText, Text: "second answer"},
			{SessionID: "s1", Seq: 5, Kind: api.KindTurnDone},
		}
		startup := true
		svc.lookup = func(context.Context, string) (api.Session, error) {
			if startup {
				startup = false
				return api.Session{ID: "s1", Status: api.StatusWorking}, nil
			}
			return api.Session{ID: "s1", Status: api.StatusIdle}, nil // both turns are over
		}
		tr := &pollTransport{polls: make(chan struct{}, 10), updates: make(chan []update)}
		tg := New(svc, Options{ChatID: chatID, UserID: kevin, StatePath: path})
		tg.bot.http.Transport = tr
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- tg.Run(ctx) }()
		<-tr.polls
		for _, ev := range svc.history {
			svc.events <- ev
		}
		synctest.Wait()
		if len(tr.sent) != 2 || tr.sent[1]["text"] != "second answer" || tr.sent[1]["message_thread_id"] != float64(7) {
			t.Errorf("later answer lost: sent=%v", tr.sent)
		}
		if _, waiting := New(svc, Options{StatePath: path}).state.ReplyTo["s1"]; waiting {
			t.Error("the last completion left the reply destination saved")
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

// A completion handled while a send is in flight is checked again when the
// send returns, so a finished turn does not stay "waiting" forever.
func TestRunCompletionDuringSend(t *testing.T) {
	for _, name := range []string{"accepted", "failed"} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "telegram.json")
				b, err := json.Marshal(state{Topics: map[int64]topic{8: {Project: "/project", SessionID: "s1"}}, ReplyTo: map[string]int64{"s1": 7}})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, b, 0o600); err != nil {
					t.Fatal(err)
				}
				started, release := make(chan struct{}), make(chan struct{})
				svc := &registrationService{turnDoneService: turnDoneService{fakeService: fakeService{events: make(chan api.Event, 2)}}}
				startup := true
				svc.lookup = func(context.Context, string) (api.Session, error) {
					if startup {
						startup = false
						return api.Session{ID: "s1", Status: api.StatusWorking}, nil
					}
					return api.Session{ID: "s1", Status: api.StatusIdle}, nil
				}
				svc.send = func(context.Context, api.MessageRequest) (api.MessageResult, error) {
					close(started)
					<-release
					if name == "failed" {
						return api.MessageResult{}, api.NewError(api.CodeUnavailable, "send unavailable")
					}
					return api.MessageResult{SessionID: "s1"}, nil
				}
				tr := &pollTransport{polls: make(chan struct{}, 10), updates: make(chan []update)}
				tg := New(svc, Options{ChatID: chatID, UserID: kevin, StatePath: path})
				tg.bot.http.Transport = tr
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan error, 1)
				go func() { done <- tg.Run(ctx) }()
				<-tr.polls
				tr.updates <- []update{msg(1, 8, kevin, "next message")}
				<-started
				// The whole turn ends before the acceptance response.
				svc.events <- api.Event{SessionID: "s1", Seq: 1, Kind: api.KindText, Text: "the answer"}
				svc.events <- api.Event{SessionID: "s1", Seq: 2, Kind: api.KindTurnDone}
				synctest.Wait()
				if len(tr.sent) != 1 || tr.sent[0]["text"] != "the answer" || tr.sent[0]["message_thread_id"] != float64(8) {
					t.Errorf("answer = %v", tr.sent)
				}
				close(release)
				<-tr.polls
				synctest.Wait()
				if got, waiting := New(svc, Options{StatePath: path}).state.ReplyTo["s1"]; waiting {
					t.Errorf("finished turn still waiting in topic %d", got)
				}
				cancel()
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			})
		})
	}
}

// The send can end while the completion lookup is still running, before the
// completion is recorded for finishSent: the lookup must check again.
func TestRunSendEndsDuringCompletionLookup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "telegram.json")
		b, err := json.Marshal(state{Topics: map[int64]topic{8: {Project: "/project", SessionID: "s1"}}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
		started, release := make(chan struct{}), make(chan struct{})
		looking, answer := make(chan struct{}), make(chan struct{})
		svc := &registrationService{turnDoneService: turnDoneService{fakeService: fakeService{events: make(chan api.Event, 2)}}}
		first := true
		svc.lookup = func(context.Context, string) (api.Session, error) {
			if first {
				first = false
				close(looking)
				<-answer
			}
			return api.Session{ID: "s1", Status: api.StatusIdle}, nil
		}
		svc.send = func(context.Context, api.MessageRequest) (api.MessageResult, error) {
			close(started)
			<-release
			return api.MessageResult{SessionID: "s1"}, nil
		}
		tr := &pollTransport{polls: make(chan struct{}, 10), updates: make(chan []update)}
		tg := New(svc, Options{ChatID: chatID, UserID: kevin, StatePath: path})
		tg.bot.http.Transport = tr
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- tg.Run(ctx) }()
		<-tr.polls
		tr.updates <- []update{msg(1, 8, kevin, "next message")}
		<-started
		svc.events <- api.Event{SessionID: "s1", Seq: 1, Kind: api.KindText, Text: "the answer"}
		svc.events <- api.Event{SessionID: "s1", Seq: 2, Kind: api.KindTurnDone}
		<-looking
		close(release)
		<-tr.polls // the send and its finishSent are over
		close(answer)
		synctest.Wait()
		if len(tr.sent) != 1 || tr.sent[0]["text"] != "the answer" {
			t.Errorf("answer = %v", tr.sent)
		}
		if got, waiting := New(svc, Options{StatePath: path}).state.ReplyTo["s1"]; waiting {
			t.Errorf("finished turn still waiting in topic %d", got)
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

// A turn_done that could not be saved arrives live with Seq 0 (issue #25).
// Older stored history must not keep the topic waiting forever, whether or
// not this bot saw that history.
func TestRunUnsavedTurnDoneFinishesReply(t *testing.T) {
	for _, name := range []string{"seen", "fresh"} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				path := savedState(t, "s1", 7)
				svc := &creationService{registrationService: registrationService{turnDoneService: turnDoneService{fakeService: fakeService{events: make(chan api.Event, 4)}}}}
				svc.history = []api.Event{
					{SessionID: "s1", Seq: 1, Kind: api.KindUserMessage, Text: "question"},
					{SessionID: "s1", Seq: 2, Kind: api.KindText, Text: "partial"},
				}
				startup := true
				svc.lookup = func(context.Context, string) (api.Session, error) {
					if startup {
						startup = false
						return api.Session{ID: "s1", Status: api.StatusWorking}, nil
					}
					return api.Session{ID: "s1", Status: api.StatusIdle}, nil
				}
				tr := &pollTransport{polls: make(chan struct{}, 10), updates: make(chan []update)}
				tg := New(svc, Options{ChatID: chatID, UserID: kevin, StatePath: path})
				tg.bot.http.Transport = tr
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan error, 1)
				go func() { done <- tg.Run(ctx) }()
				<-tr.polls
				if name == "seen" {
					for _, ev := range svc.history {
						svc.events <- ev
					}
				}
				svc.events <- api.Event{SessionID: "s1", Kind: api.KindError, Text: "could not save the conversation"}
				svc.events <- api.Event{SessionID: "s1", Kind: api.KindTurnDone}
				synctest.Wait()
				if got, waiting := New(svc, Options{StatePath: path}).state.ReplyTo["s1"]; waiting {
					t.Errorf("unsaved completion left the topic waiting: %d", got)
				}
				cancel()
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			})
		})
	}
}

// A saved turn can end while no bot watched (during a hand-over: "missed")
// or while the new bot checks it at start ("live"). Either way its answer
// is sent once from the stored history, with no false "answer was lost".
func TestRunStartupRecoversFinishedTurn(t *testing.T) {
	for _, name := range []string{"missed", "live"} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				path := savedState(t, "s1", 7)
				svc := &creationService{registrationService: registrationService{turnDoneService: turnDoneService{fakeService: fakeService{events: make(chan api.Event, 3)}}}}
				svc.history = []api.Event{
					{SessionID: "s1", Seq: 1, Kind: api.KindUserMessage, Text: "question"},
					{SessionID: "s1", Seq: 2, Kind: api.KindText, Text: "the answer"},
					{SessionID: "s1", Seq: 3, Kind: api.KindTurnDone},
				}
				looking, answer := make(chan struct{}), make(chan struct{})
				first := true
				svc.lookup = func(context.Context, string) (api.Session, error) {
					if first && name == "live" {
						first = false
						close(looking)
						<-answer // the turn ends while startup checks it
					}
					return api.Session{ID: "s1", Status: api.StatusIdle}, nil
				}
				tr := &pollTransport{polls: make(chan struct{}, 10), updates: make(chan []update)}
				tg := New(svc, Options{ChatID: chatID, UserID: kevin, StatePath: path})
				tg.bot.http.Transport = tr
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan error, 1)
				go func() { done <- tg.Run(ctx) }()
				if name == "live" {
					<-looking
					svc.events <- svc.history[1]
					svc.events <- svc.history[2]
					close(answer)
				}
				<-tr.polls
				synctest.Wait()
				if len(tr.sent) != 1 || tr.sent[0]["text"] != "the answer" || tr.sent[0]["message_thread_id"] != float64(7) {
					t.Errorf("sent = %v, want the answer once in topic 7", tr.sent)
				}
				if _, waiting := New(svc, Options{StatePath: path}).state.ReplyTo["s1"]; waiting {
					t.Error("the finished turn is still waiting")
				}
				cancel()
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			})
		})
	}
}

// A completion that could not be saved (Seq 0, issue #25) can arrive while
// creation replay waits for history. Replay cannot find it, so it is held
// and handled after the stored events.
func TestRunCreationKeepsUnsavedCompletion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "telegram.json")
		if err := os.WriteFile(path, []byte(`{"topics":{"8":{"project":"/project"}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		svc := &planNoticeService{failed: make(chan struct{}), replay: make(chan struct{}), creationService: creationService{failures: 1, registrationService: registrationService{turnDoneService: turnDoneService{fakeService: fakeService{events: make(chan api.Event, 10)}}}}}
		svc.history = []api.Event{
			{SessionID: "created", Seq: 1, Kind: api.KindUserMessage, Text: "first request"},
			{SessionID: "created", Seq: 2, Kind: api.KindText, Text: "partial"},
		}
		svc.lookup = func(context.Context, string) (api.Session, error) {
			return api.Session{ID: "created", Status: api.StatusIdle}, nil
		}
		svc.send = func(context.Context, api.MessageRequest) (api.MessageResult, error) {
			return api.MessageResult{SessionID: "created"}, nil
		}
		tr := &pollTransport{polls: make(chan struct{}, 10), updates: make(chan []update)}
		tg := New(svc, Options{ChatID: chatID, UserID: kevin, StatePath: path})
		tg.bot.http.Transport = tr
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- tg.Run(ctx) }()
		<-tr.polls
		tr.updates <- []update{msg(1, 8, kevin, "first request")}
		<-svc.failed
		svc.events <- api.Event{SessionID: "created", Kind: api.KindError, Text: "could not save the conversation"}
		svc.events <- api.Event{SessionID: "created", Kind: api.KindTurnDone}
		synctest.Wait() // both arrive during replay backoff
		close(svc.replay)
		time.Sleep(retryWait)
		<-tr.polls
		synctest.Wait()
		want := []string{"⚠ could not save the conversation", "partial"}
		if len(tr.sent) != len(want) {
			t.Errorf("sent = %v, want %q", tr.sent, want)
		} else {
			for i, text := range want {
				if p := tr.sent[i]; p["text"] != text || p["message_thread_id"] != float64(8) {
					t.Errorf("reply %d = %v, want %q in topic 8", i, p, text)
				}
			}
		}
		if _, waiting := New(svc, Options{StatePath: path}).state.ReplyTo["created"]; waiting {
			t.Error("the unsaved completion left the topic waiting")
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

// The live stream can miss a waiting session's final text: the bot attached
// during a hand-over after it was saved ("none-seen"), or the hub dropped
// the stream in the middle of the turn ("partly-seen"). The turn_done then
// sends the missed stored events first.
func TestRunTurnDoneReplaysMissedEvents(t *testing.T) {
	for _, name := range []string{"none-seen", "partly-seen"} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				path := savedState(t, "s1", 7)
				svc := &creationService{registrationService: registrationService{turnDoneService: turnDoneService{fakeService: fakeService{events: make(chan api.Event, 3)}}}}
				svc.history = []api.Event{
					{SessionID: "s1", Seq: 1, Kind: api.KindUserMessage, Text: "question"},
					{SessionID: "s1", Seq: 2, Kind: api.KindText, Text: "the answer"},
					{SessionID: "s1", Seq: 3, Kind: api.KindTurnDone},
				}
				startup := true
				svc.lookup = func(context.Context, string) (api.Session, error) {
					if startup {
						startup = false
						return api.Session{ID: "s1", Status: api.StatusWorking}, nil
					}
					return api.Session{ID: "s1", Status: api.StatusIdle}, nil
				}
				tr := &pollTransport{polls: make(chan struct{}, 10), updates: make(chan []update)}
				tg := New(svc, Options{ChatID: chatID, UserID: kevin, StatePath: path})
				tg.bot.http.Transport = tr
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan error, 1)
				go func() { done <- tg.Run(ctx) }()
				<-tr.polls
				if name == "partly-seen" {
					svc.events <- svc.history[0]
				}
				svc.events <- svc.history[2] // the text went out before this stream saw it
				synctest.Wait()
				if len(tr.sent) != 1 || tr.sent[0]["text"] != "the answer" || tr.sent[0]["message_thread_id"] != float64(7) {
					t.Errorf("sent = %v, want the answer once in topic 7", tr.sent)
				}
				if _, waiting := New(svc, Options{StatePath: path}).state.ReplyTo["s1"]; waiting {
					t.Error("the finished turn is still waiting")
				}
				cancel()
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			})
		})
	}
}

// A new session's first turn can fail to save and end before the creation
// response (Seq 0 error and turn_done, issue #25). Nothing stored finishes
// it, so the topic is told at once instead of typing until the next start.
func TestRunCreationSettlesUnsavedTurnBeforeResponse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "telegram.json")
		if err := os.WriteFile(path, []byte(`{"topics":{"8":{"project":"/project"}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		svc := &creationService{registrationService: registrationService{turnDoneService: turnDoneService{fakeService: fakeService{events: make(chan api.Event, 2)}}}}
		svc.history = []api.Event{{SessionID: "created", Seq: 1, Kind: api.KindUserMessage, Text: "first request"}}
		svc.lookup = func(context.Context, string) (api.Session, error) {
			return api.Session{ID: "created", Status: api.StatusIdle}, nil
		}
		started, release := make(chan struct{}), make(chan struct{})
		svc.send = func(context.Context, api.MessageRequest) (api.MessageResult, error) {
			close(started)
			<-release
			return api.MessageResult{SessionID: "created"}, nil
		}
		tr := &pollTransport{polls: make(chan struct{}, 10), updates: make(chan []update)}
		tg := New(svc, Options{ChatID: chatID, UserID: kevin, StatePath: path})
		tg.bot.http.Transport = tr
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- tg.Run(ctx) }()
		<-tr.polls
		tr.updates <- []update{msg(1, 8, kevin, "first request")}
		<-started
		svc.events <- api.Event{SessionID: "created", Kind: api.KindError, Text: "could not save the conversation"}
		svc.events <- api.Event{SessionID: "created", Kind: api.KindTurnDone}
		synctest.Wait() // both went out before the session was registered
		close(release)
		<-tr.polls
		synctest.Wait()
		if len(tr.sent) != 1 || !strings.Contains(tr.sent[0]["text"].(string), "without a saved answer") || tr.sent[0]["message_thread_id"] != float64(8) {
			t.Errorf("sent = %v, want the lost-answer notice in topic 8", tr.sent)
		}
		if _, waiting := New(svc, Options{StatePath: path}).state.ReplyTo["created"]; waiting {
			t.Error("the unsaved turn left the topic waiting")
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

// The worker shows idle just before it saves turn_done. A start that looks
// in that moment must not call the answer lost.
func TestRunStartupWaitsForFinalSave(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		path := savedState(t, "s1", 7)
		svc := &creationService{registrationService: registrationService{turnDoneService: turnDoneService{fakeService: fakeService{events: make(chan api.Event, 1)}}}}
		svc.history = []api.Event{
			{SessionID: "s1", Seq: 1, Kind: api.KindUserMessage, Text: "question"},
			{SessionID: "s1", Seq: 2, Kind: api.KindText, Text: "the answer"},
		}
		svc.lookup = func(context.Context, string) (api.Session, error) {
			return api.Session{ID: "s1", Status: api.StatusIdle}, nil
		}
		tr := &pollTransport{polls: make(chan struct{}, 10), updates: make(chan []update)}
		tg := New(svc, Options{ChatID: chatID, UserID: kevin, StatePath: path})
		tg.bot.http.Transport = tr
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- tg.Run(ctx) }()
		synctest.Wait() // the start saw idle without a stored turn_done
		final := api.Event{SessionID: "s1", Seq: 3, Kind: api.KindTurnDone}
		svc.history = append(svc.history, final)
		svc.events <- final
		<-tr.polls
		synctest.Wait()
		if len(tr.sent) != 1 || tr.sent[0]["text"] != "the answer" || tr.sent[0]["message_thread_id"] != float64(7) {
			t.Errorf("sent = %v, want the answer once in topic 7", tr.sent)
		}
		if _, waiting := New(svc, Options{StatePath: path}).state.ReplyTo["s1"]; waiting {
			t.Error("the finished turn is still waiting")
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

// dropService hands out the streams the test gives it, so a test can drop
// the watcher like the hub does when it falls behind.
type dropService struct {
	creationService
	streams chan chan api.Event
}

func (s *dropService) Watch(ctx context.Context, _ api.WatchFilter) (<-chan api.Event, error) {
	select {
	case ch := <-s.streams:
		return ch, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// The hub drops a watcher that falls behind; a waiting turn that finishes
// before it subscribes again sends nothing live. The new subscription sends
// the stored events after the last one seen ("partly-seen"), or settles a
// session this bot saw nothing of yet ("none-seen", kept at start).
func TestRunWatchCatchesUpAfterDrop(t *testing.T) {
	for _, name := range []string{"partly-seen", "none-seen"} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				path := savedState(t, "s1", 7)
				svc := &dropService{streams: make(chan chan api.Event)}
				svc.history = []api.Event{
					{SessionID: "s1", Seq: 1, Kind: api.KindUserMessage, Text: "question"},
					{SessionID: "s1", Seq: 2, Kind: api.KindText, Text: "the answer"},
				}
				var finished atomic.Bool
				svc.lookup = func(context.Context, string) (api.Session, error) {
					if finished.Load() {
						return api.Session{ID: "s1", Status: api.StatusIdle}, nil
					}
					return api.Session{ID: "s1", Status: api.StatusWorking}, nil
				}
				tr := &pollTransport{polls: make(chan struct{}, 10), updates: make(chan []update)}
				tg := New(svc, Options{ChatID: chatID, UserID: kevin, StatePath: path})
				tg.bot.http.Transport = tr
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan error, 1)
				go func() { done <- tg.Run(ctx) }()
				<-tr.polls
				first := make(chan api.Event, 2)
				svc.streams <- first
				if name == "partly-seen" {
					first <- svc.history[0]
					first <- svc.history[1]
				}
				synctest.Wait()
				svc.history = append(svc.history, api.Event{SessionID: "s1", Seq: 3, Kind: api.KindTurnDone})
				finished.Store(true)
				close(first) // dropped: the turn ended while nobody listened
				second := make(chan api.Event)
				svc.streams <- second
				synctest.Wait()
				if len(tr.sent) != 1 || tr.sent[0]["text"] != "the answer" || tr.sent[0]["message_thread_id"] != float64(7) {
					t.Errorf("sent = %v, want the answer once in topic 7", tr.sent)
				}
				if _, waiting := New(svc, Options{StatePath: path}).state.ReplyTo["s1"]; waiting {
					t.Error("the finished turn is still waiting")
				}
				cancel()
				close(second)
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			})
		})
	}
}
