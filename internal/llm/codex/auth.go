package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/viminizer/germes/internal/file"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	tokenURL     = "https://auth.openai.com/oauth/token"
	clientID     = "app_EMoamEEZ73f0CkXaXp7hrann"
	refreshSkew  = 120 * time.Second
	authClaimKey = "https://api.openai.com/auth"
)

// ErrLogin means the Codex login is gone or expired for good.
var ErrLogin = errors.New("codex login expired: run `codex login`, then try again")

// tokens is the login. Germes keeps its own copy (germesFile) and only reads
// the Codex CLI's file (codexFile), so one app's refresh cannot break the
// other's (same as Hermes).
type tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id,omitempty"`
	IDToken      string `json:"id_token,omitempty"`
}

type auth struct {
	germesFile string
	codexFile  string
	http       *http.Client

	mu  sync.Mutex
	tok *tokens
}

// get returns a valid access token. The Codex CLI refreshes its login on its
// own, so Germes first takes the CLI's fresh token. Only when both are about
// to expire does Germes refresh, and then it writes the new token back to the
// CLI's file: refresh tokens are single-use, so not writing back would log
// the Codex CLI out.
func (a *auth) get(ctx context.Context, force bool) (tokens, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tok == nil {
		t, err := a.load()
		if err != nil {
			return tokens{}, err
		}
		a.tok = &t
	}
	if !force && !expiresSoon(a.tok.AccessToken) {
		return *a.tok, nil
	}
	cli, cliErr := readFile(a.codexFile)
	if cliErr == nil && cli.AccessToken != a.tok.AccessToken && !expiresSoon(cli.AccessToken) {
		return a.use(cli)
	}
	base := *a.tok
	if cliErr == nil && cli.RefreshToken != base.RefreshToken {
		base = cli // the CLI's refresh token is newer
	}
	t, err := a.refresh(ctx, base)
	if err != nil {
		return tokens{}, err
	}
	if err := a.writeBack(t); err != nil {
		slog.Warn("could not update the Codex CLI login; run `codex login` if it complains", "err", err)
	}
	return a.use(t)
}

func (a *auth) use(t tokens) (tokens, error) {
	a.tok = &t
	if err := a.save(t); err != nil {
		return t, fmt.Errorf("save codex login: %w", err)
	}
	return t, nil
}

// writeBack puts refreshed tokens into the Codex CLI's auth.json, keeping
// every other field of that file.
func (a *auth) writeBack(t tokens) error {
	b, err := os.ReadFile(a.codexFile)
	if err != nil {
		return err
	}
	var f map[string]any
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	tok, _ := f["tokens"].(map[string]any)
	if tok == nil {
		tok = map[string]any{}
	}
	tok["access_token"], tok["refresh_token"] = t.AccessToken, t.RefreshToken
	if t.IDToken != "" {
		tok["id_token"] = t.IDToken
	}
	f["tokens"] = tok
	f["last_refresh"] = time.Now().UTC().Format(time.RFC3339Nano)
	out, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return file.Save(a.codexFile, out, 0o600)
}

// load reads Germes' copy, or imports the Codex CLI login on first use.
func (a *auth) load() (tokens, error) {
	if t, err := readFile(a.germesFile); err == nil {
		return t, nil
	}
	t, err := readFile(a.codexFile)
	if err != nil {
		return tokens{}, fmt.Errorf("no codex login found (%v): run `codex login`", err)
	}
	return t, a.save(t)
}

func readFile(path string) (tokens, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return tokens{}, err
	}
	var f struct {
		Tokens tokens `json:"tokens"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return tokens{}, fmt.Errorf("%s: %w", path, err)
	}
	if f.Tokens.AccessToken == "" || f.Tokens.RefreshToken == "" {
		return tokens{}, fmt.Errorf("%s has no tokens", path)
	}
	return f.Tokens, nil
}

func (a *auth) save(t tokens) error {
	b, err := json.MarshalIndent(map[string]any{"tokens": t, "saved_at": time.Now().UTC()}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(a.germesFile), 0o700); err != nil {
		return err
	}
	return file.Save(a.germesFile, b, 0o600)
}

func (a *auth) refresh(ctx context.Context, t tokens) (tokens, error) {
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {t.RefreshToken}, "client_id": {clientID}}
	req, err := http.NewRequestWithContext(ctx, "POST", tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return tokens{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return tokens{}, fmt.Errorf("refresh codex login: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return tokens{}, fmt.Errorf("refresh codex login: rate limited, try again later")
	case resp.StatusCode == 401 || resp.StatusCode == 403 || (resp.StatusCode == 400 && strings.Contains(string(body), "invalid")):
		return tokens{}, ErrLogin
	case resp.StatusCode != http.StatusOK:
		return tokens{}, fmt.Errorf("refresh codex login: %s", resp.Status)
	}
	var r struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &r); err != nil || r.AccessToken == "" {
		return tokens{}, fmt.Errorf("refresh codex login: bad reply")
	}
	t.AccessToken = r.AccessToken
	if r.RefreshToken != "" {
		t.RefreshToken = r.RefreshToken // single-use: always keep the newest
	}
	if r.IDToken != "" {
		t.IDToken = r.IDToken
	}
	return t, nil
}

// claims reads the JWT payload without checking it (the server does that).
func claims(jwt string) map[string]any {
	parts := strings.Split(jwt, ".")
	if len(parts) < 2 {
		return nil
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var c map[string]any
	json.Unmarshal(b, &c)
	return c
}

func expiresSoon(jwt string) bool {
	exp, ok := claims(jwt)["exp"].(float64)
	if !ok {
		return true
	}
	return time.Until(time.Unix(int64(exp), 0)) < refreshSkew
}

// headers are the account headers every Codex request needs.
func headers(t tokens) map[string]string {
	h := map[string]string{"Authorization": "Bearer " + t.AccessToken}
	auth, _ := claims(t.AccessToken)[authClaimKey].(map[string]any)
	account := t.AccountID
	if account == "" {
		account, _ = auth["chatgpt_account_id"].(string)
	}
	if account != "" {
		h["ChatGPT-Account-ID"] = account
	}
	residency, _ := auth["chatgpt_data_residency"].(string)
	if residency == "" {
		residency, _ = auth["chatgpt_compute_residency"].(string)
	}
	if residency != "" {
		h["x-openai-internal-codex-residency"] = residency
	}
	return h
}
