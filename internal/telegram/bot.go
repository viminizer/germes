package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// maxText is the longest text one Telegram message may hold (4096, with a
// margin for characters that count double).
const maxText = 4000

// bot calls the Bot API. Errors never contain the token (R16).
type bot struct {
	base  string // https://api.telegram.org/bot<token>
	files string // https://api.telegram.org/file/bot<token>
	http  *http.Client
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
	return b.do(ctx, method, "application/json", bytes.NewReader(body), out)
}

// do posts body to method and decodes Telegram's reply into out.
func (b *bot) do(ctx context.Context, method, contentType string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, "POST", b.base+"/"+method, body)
	if err != nil {
		return errors.New("telegram: bad request")
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := b.http.Do(req)
	if err != nil {
		return fmt.Errorf("telegram %s: %w", method, hideToken(err))
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
		Caption  string `json:"caption"` // the text sent with a file
		Document *file  `json:"document"`
		Photo    []file `json:"photo"` // sizes, smallest first
	}
	file struct {
		ID       string `json:"file_id"`
		UniqueID string `json:"file_unique_id"`
		Name     string `json:"file_name"` // documents only
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

// file is the file the message carries: its document, or its photo in the
// largest size. nil = none.
func (m *message) file() *file {
	switch {
	case m.Document != nil:
		f := *m.Document
		if f.Name == "" {
			f.Name = "file"
		}
		return &f
	case len(m.Photo) > 0:
		f := m.Photo[len(m.Photo)-1]
		f.Name = "photo.jpg"
		return &f
	}
	return nil
}

// hideToken drops the URL from a request error: the URL holds the token.
func hideToken(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// download saves a file the user sent into dir and returns its path.
// Telegram's servers refuse files over 20 MB ("file is too big").
func (b *bot) download(ctx context.Context, f file, dir string) (string, error) {
	var got struct {
		Path string `json:"file_path"`
	}
	if err := b.call(ctx, "getFile", map[string]any{"file_id": f.ID}, &got); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", b.files+"/"+got.Path, nil)
	if err != nil {
		return "", errors.New("telegram: bad file path")
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("telegram download: %w", hideToken(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("telegram download: %s", resp.Status)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// The unique id keeps two files with one name apart; Base keeps a
	// name like "../x" inside dir.
	path := filepath.Join(dir, f.UniqueID+"-"+filepath.Base(f.Name))
	out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	_, err = io.Copy(out, resp.Body)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
		return "", fmt.Errorf("save the file: %w", err)
	}
	return path, nil
}

// sendFile uploads the file at path to a topic (0 = General).
func (b *bot) sendFile(ctx context.Context, chatID, thread int64, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	// ponytail: the upload is held in memory (send_file allows up to 50 MB);
	// stream it through io.Pipe if that becomes a problem.
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	w.WriteField("chat_id", strconv.FormatInt(chatID, 10))
	if thread != 0 {
		w.WriteField("message_thread_id", strconv.FormatInt(thread, 10))
	}
	part, err := w.CreateFormFile("document", filepath.Base(path))
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, f); err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := w.Close(); err != nil {
		return err
	}
	return b.do(ctx, "sendDocument", w.FormDataContentType(), &body, nil)
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
