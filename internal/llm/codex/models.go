package codex

import (
	"context"
	"encoding/json"
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
	windowFailedTTL = 5 * time.Minute // how long a failed read is not tried again
)

// GetContextWindow returns the model's context window in tokens from
// Codex's model list. It is read once and kept for an hour; after a
// failure it is not asked again for 5 minutes.
func (c *Client) GetContextWindow(ctx context.Context) (int, error) {
	c.winMu.Lock()
	defer c.winMu.Unlock()
	if !c.winAt.IsZero() && time.Since(c.winAt) < windowTTL && (c.winErr == nil || time.Since(c.winAt) < windowFailedTTL) {
		return c.win, c.winErr
	}
	c.win, c.winErr = c.readContextWindow(ctx)
	c.winAt = time.Now()
	return c.win, c.winErr
}

func (c *Client) readContextWindow(ctx context.Context) (int, error) {
	tok, err := c.auth.get(ctx, false)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for _, v := range modelListVersions {
		models, err := c.listModels(ctx, tok, v)
		if err != nil {
			return 0, err
		}
		for _, m := range models {
			if m.Slug == c.opts.Model && m.ContextWindow > 0 {
				return m.ContextWindow, nil
			}
		}
	}
	return 0, fmt.Errorf("model %s is not in the Codex model list", c.opts.Model)
}

type modelInfo struct {
	Slug          string `json:"slug"`
	ContextWindow int    `json:"context_window"`
}

func (c *Client) listModels(ctx context.Context, tok tokens, version string) ([]modelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", baseURL+"/codex/models?client_version="+version, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers(tok) {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "germes/"+c.opts.Version)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("codex model list: %w", err)
	}
	defer resp.Body.Close()
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
