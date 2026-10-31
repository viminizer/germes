package codex

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A login that stays good for an hour, so no refresh is tried.
func testTokens() *tokens {
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(time.Hour).Unix())))
	return &tokens{AccessToken: "x." + payload + ".y", RefreshToken: "r"}
}

// The window comes from the model list: the newest client version first,
// the ungated one when it fails, like Hermes. A later failed read keeps
// the last good window instead of falling back to 200k.
func TestContextWindowFromTheModelList(t *testing.T) {
	down := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down || r.URL.Query().Get("client_version") == "99.0.0" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		fmt.Fprint(w, `{"models":[{"slug":"other","context_window":1},{"slug":"gpt-6.1-sol","context_window":500000}]}`)
	}))
	defer srv.Close()
	c := &Client{opts: Options{Model: "gpt-6.1-sol"}, http: srv.Client(), base: srv.URL, auth: &auth{tok: testTokens()}}
	if win, err := c.GetContextWindow(context.Background()); err != nil || win != 500_000 {
		t.Fatalf("window %d, err %v; want 500000 from the 0.0.0 list", win, err)
	}
	down = true
	c.winAt = time.Now().Add(-2 * windowTTL) // due for a new read, which fails
	if win, err := c.GetContextWindow(context.Background()); err != nil || win != 500_000 {
		t.Fatalf("after a failed read: window %d, err %v; want the last good 500000", win, err)
	}
	if c.winErr == nil {
		t.Fatal("the failure is not remembered: the list would be asked on every turn")
	}
}
