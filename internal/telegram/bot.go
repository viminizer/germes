package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxText is the longest text one Telegram message may hold (4096, with a
// margin for characters that count double).
const maxText = 4000

// bot calls the Bot API. Errors never contain the token (R16).
type bot struct {
	base string // https://api.telegram.org/bot<token>
	http *http.Client
}

// apiError is an error reply from Telegram.
type apiError struct {
	Code        int
	Description string
	RetryAfter  int // seconds, on 429
}

func (e *apiError) Error() string { return fmt.Sprintf("telegram %d: %s", e.Code, e.Description) }

// call posts params to method and decodes the result into out. A rate
// limit (429) waits as long as Telegram says, once.
func (b *bot) call(ctx context.Context, method string, params, out any) error {
	err := b.post(ctx, method, params, out)
	var e *apiError
	if errors.As(err, &e) && e.RetryAfter > 0 {
		select {
		case <-time.After(time.Duration(e.RetryAfter) * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
		err = b.post(ctx, method, params, out)
	}
	return err
}

func (b *bot) post(ctx context.Context, method string, params, out any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", b.base+"/"+method, bytes.NewReader(body))
	if err != nil {
		return errors.New("telegram: bad request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // the URL holds the token
		}
		return fmt.Errorf("telegram %s: %w", method, err)
	}
	defer resp.Body.Close()
	var r struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Code        int             `json:"error_code"`
		Description string          `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("telegram %s: bad reply (%s)", method, resp.Status)
	}
	if !r.OK {
		return &apiError{Code: r.Code, Description: r.Description, RetryAfter: r.Parameters.RetryAfter}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(r.Result, out)
}

// Bot API types: only the fields Germes uses.
type (
	update struct {
		ID       int64     `json:"update_id"`
		Message  *message  `json:"message"`
		Callback *callback `json:"callback_query"`
	}
	message struct {
		ID       int64  `json:"message_id"`
		ThreadID int64  `json:"message_thread_id"` // set on topic messages, and on replies in General
		IsTopic  bool   `json:"is_topic_message"`
		From     *user  `json:"from"`
		Chat     chat   `json:"chat"`
		Text     string `json:"text"`
	}
	callback struct {
		ID      string   `json:"id"`
		From    user     `json:"from"`
		Message *message `json:"message"`
		Data    string   `json:"data"`
	}
	user struct {
		ID int64 `json:"id"`
	}
	chat struct {
		ID int64 `json:"id"`
	}
	button struct {
		Text string `json:"text"`
		Data string `json:"callback_data"`
	}
)

// topic is the message's topic; 0 = General. A reply in General has a
// thread id too, but is not a topic message.
func (m *message) topic() int64 {
	if !m.IsTopic {
		return 0
	}
	return m.ThreadID
}

// send sends text to a topic (0 = General), split into Telegram-sized
// messages, with buttons under the last one.
func (b *bot) send(ctx context.Context, chatID, thread int64, text string, buttons [][]button) error {
	parts := split(text, maxText)
	for i, part := range parts {
		params := map[string]any{"chat_id": chatID, "text": part}
		if thread != 0 {
			params["message_thread_id"] = thread
		}
		if i == len(parts)-1 && len(buttons) > 0 {
			params["reply_markup"] = map[string]any{"inline_keyboard": buttons}
		}
		if err := b.call(ctx, "sendMessage", params, nil); err != nil {
			return err
		}
	}
	return nil
}

// split cuts text into pieces of at most n characters, at a line break
// when one is near.
func split(text string, n int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return []string{"(empty reply)"}
	}
	var parts []string
	r := []rune(text)
	for len(r) > n {
		cut := n
		if i := strings.LastIndex(string(r[:n]), "\n"); i > 0 {
			if c := len([]rune(string(r[:n])[:i])); c > n/2 {
				cut = c
			}
		}
		parts = append(parts, strings.TrimSpace(string(r[:cut])))
		r = r[cut:]
	}
	return append(parts, strings.TrimSpace(string(r)))
}
