package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Codex's model list says each model's context window (Hermes reads it the
// same way: agent/model_metadata.py). Older client versions see fewer
// models, so the newest is asked first, then the ungated one.
var modelListVersions = []string{"99.0.0", "0.0.0"}

const (
	windowTTL       = time.Hour       // how long a read window is kept
	windowFailedTTL = 5 * time.Minute // how long after a failed read it is tried again
)

// GetContextWindow returns the model's context window in tokens from
// Codex's model list. It is read once an hour, by one caller at a time:
// the others get the window read before. A failed read keeps the last good
// window and is tried again after 5 minutes; a read cut off (Esc, daemon
// stopping) is not remembered.
func (c *Client) GetContextWindow(ctx context.Context) (int, error) {
	c.winMu.Lock()
	ttl := windowTTL
	if c.winErr != nil {
		ttl = windowFailedTTL
	}
	if c.winReading || (!c.winAt.IsZero() && time.Since(c.winAt) < ttl) {
		defer c.winMu.Unlock()
		return c.lastWindow()
	}
	c.winReading = true
	c.winMu.Unlock()

	win, err := c.readContextWindow(ctx)

	c.winMu.Lock()
	defer c.winMu.Unlock()
	c.winReading = false
	switch {
	case err == nil:
		c.win, c.winErr, c.winAt = win, nil, time.Now()
	case ctx.Err() == nil:
		c.winErr, c.winAt = err, time.Now() // the last good window stays
	}
	return c.lastWindow()
}

// lastWindow is the last good window, or why there is none. Caller holds winMu.
func (c *Client) lastWindow() (int, error) {
	if c.win > 0 {
		return c.win, nil
	}
	if c.winErr == nil {
		return 0, errors.New("the context window is being read")
	}
	return 0, c.winErr
}

func (c *Client) readContextWindow(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var lastErr error
	for _, v := range modelListVersions {
		models, err := c.listModels(ctx, v)
		if err != nil {
			lastErr = err
			continue // like Hermes: the older version may still answer
		}
		for _, m := range models {
			if m.Slug == c.opts.Model && m.ContextWindow > 0 {
				return m.ContextWindow, nil
			}
		}
	}
	if lastErr != nil {
		return 0, lastErr
	}
	return 0, fmt.Errorf("model %s is not in the Codex model list", c.opts.Model)
}

type modelInfo struct {
	Slug          string `json:"slug"`
	ContextWindow int    `json:"context_window"`
}

// listModels reads the model list as one client version sees it. A
// rejected login is refreshed once, like a model call.
func (c *Client) listModels(ctx context.Context, version string) ([]modelInfo, error) {
	for force := false; ; force = true {
		tok, err := c.auth.get(ctx, force)
		if err != nil {
			return nil, err
		}
		resp, err := c.get(ctx, tok, "/codex/models?client_version="+version)
		if err != nil {
			return nil, fmt.Errorf("codex model list: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized && !force {
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("codex model list: %s", resp.Status)
		}
		var b struct {
			Models []modelInfo `json:"models"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
			return nil, fmt.Errorf("codex model list: %w", err)
		}
		return b.Models, nil
	}
}

// get sends a GET for JSON to the Codex backend with the account headers.
func (c *Client) get(ctx context.Context, tok tokens, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.base+path, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers(tok) {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "germes/"+c.opts.Version)
	return c.http.Do(req)
}
