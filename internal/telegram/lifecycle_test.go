package telegram

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
