// Package telegram talks to Germes from a Telegram group with topics. A
// topic bound to a project (/bind) talks to that project's session; General
// and unbound topics talk to the overseer. Answers come back when a turn
// ends; job notices go to the project's topic. Only the configured user in
// the configured chat is heard. It imports only api.
package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/viminizer/germes/internal/api"
)

const (
	pollTimeout = 30              // seconds a getUpdates call waits for news
	retryWait   = 5 * time.Second // after a failed poll or watch
	typingEvery = 4 * time.Second // Telegram shows "typing" for about 5 s
)

// commands are the bot's slash commands, for the group's command menu and
// /help. /project also works, as another name for /bind (tg-supervisor's).
var commands = []struct{ Name, Help string }{
	{"bind", "Tie this topic to a project: /bind <name or path>"},
	{"status", "This topic's project, session and running jobs"},
	{"plan", "Plan a task first: changes nothing, saves the plan"},
	{"new", "A fresh session in this topic's project"},
	{"stop", "Stop the running turn"},
	{"sessions", "Recent sessions"},
	{"unbind", "This topic talks to the overseer again"},
	{"help", "What the bot does"},
}

// Options configure the bot.
type Options struct {
	Token     string
	ChatID    int64
	UserID    int64
	CodeDirs  []string // where /bind looks for projects
	StatePath string   // topic bindings and the update offset (telegram.json)
	APIBase   string   // "" = https://api.telegram.org (tests use a fake)
}

// Telegram is the bot.
type Telegram struct {
	svc  api.Service
	opts Options
	bot  *bot

	mu      sync.Mutex
	state   state
	replyTo map[string]int64    // session -> topic waiting for its answer
	choices map[string][]string // /bind buttons: token -> project folders
}

// state is saved in telegram.json.
type state struct {
	Offset int64           `json:"offset"` // the next update to read
	Topics map[int64]topic `json:"topics"` // topic id -> binding
}

// topic is a topic bound to a project, with its current session.
type topic struct {
	Project   string `json:"project"`
	SessionID string `json:"session_id,omitempty"` // "" = the next message starts one
}

// New creates the bot. It reads its saved state; a broken file starts
// fresh, with a warning.
func New(svc api.Service, opts Options) *Telegram {
	base := opts.APIBase
	if base == "" {
		base = "https://api.telegram.org"
	}
	t := &Telegram{svc: svc, opts: opts, bot: &bot{base: base + "/bot" + opts.Token, http: &http.Client{}},
		replyTo: map[string]int64{}, choices: map[string][]string{}, state: state{Topics: map[int64]topic{}}}
	if b, err := os.ReadFile(opts.StatePath); err == nil {
		if err := json.Unmarshal(b, &t.state); err != nil {
			slog.Warn("telegram: state file is broken; starting without bindings", "path", opts.StatePath, "err", err)
			t.state = state{Topics: map[int64]topic{}}
		}
	}
	if t.state.Topics == nil {
		t.state.Topics = map[int64]topic{}
	}
	return t
}

// Run polls Telegram and sends answers until ctx ends.
func (t *Telegram) Run(ctx context.Context) error {
	slog.Info("telegram bot started")
	t.setCommands(ctx)
	go recovered("telegram watch", func() { t.watch(ctx) })
	go recovered("telegram typing", func() { t.showTyping(ctx) })
	for ctx.Err() == nil {
		var updates []update
		err := t.bot.call(ctx, "getUpdates", map[string]any{"offset": t.offset(), "timeout": pollTimeout,
			"allowed_updates": []string{"message", "callback_query"}}, &updates)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("telegram poll", "err", err) // 409: another poller (an old daemon) is finishing
				wait(ctx, retryWait)
			}
			continue
		}
		for _, u := range updates {
			t.handle(ctx, u)
			t.mu.Lock()
			t.state.Offset = u.ID + 1
			t.mu.Unlock()
		}
		if len(updates) > 0 {
			t.save()
		}
	}
	return nil
}

func (t *Telegram) offset() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state.Offset
}

// handle runs one update from the allowed user; others are ignored.
func (t *Telegram) handle(ctx context.Context, u update) {
	switch {
	case u.Message != nil && t.isAllowed(u.Message.Chat.ID, u.Message.From):
		t.handleMessage(ctx, u.Message)
	case u.Callback != nil && u.Callback.Message != nil && t.isAllowed(u.Callback.Message.Chat.ID, &u.Callback.From):
		if err := t.bot.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": u.Callback.ID}, nil); err != nil {
			slog.Warn("telegram answer button", "err", err)
		}
		t.handleChoice(ctx, u.Callback.Message.topic(), u.Callback.Data)
	}
}

func (t *Telegram) isAllowed(chatID int64, from *user) bool {
	return chatID == t.opts.ChatID && from != nil && from.ID == t.opts.UserID
}

func (t *Telegram) handleMessage(ctx context.Context, m *message) {
	text := strings.TrimSpace(m.Text)
	if text == "" {
		t.reply(ctx, m.topic(), "Only text messages for now.")
		return
	}
	if strings.HasPrefix(text, "/") {
		t.command(ctx, m.topic(), text)
		return
	}
	t.ask(ctx, m.topic(), text, false)
}

// ask sends text to the topic's session and waits for its answer there.
// plan: a plan turn.
func (t *Telegram) ask(ctx context.Context, thread int64, text string, plan bool) {
	id, err := t.send(ctx, thread, text, plan)
	if err != nil {
		t.reply(ctx, thread, "Could not send it: "+err.Error())
		return
	}
	t.mu.Lock()
	t.replyTo[id] = thread
	t.mu.Unlock()
}

// send sends text to the topic's session: its project's session (started
// on the first message), or the overseer.
func (t *Telegram) send(ctx context.Context, thread int64, text string, plan bool) (string, error) {
	t.mu.Lock()
	tp, bound := t.state.Topics[thread]
	t.mu.Unlock()
	if !bound {
		res, err := t.svc.SendMessage(ctx, api.MessageRequest{SessionID: api.OverseerID, Text: text, Plan: plan})
		return res.SessionID, err
	}
	if tp.SessionID != "" {
		res, err := t.svc.SendMessage(ctx, api.MessageRequest{SessionID: tp.SessionID, Text: text, Plan: plan})
		var e *api.Error
		if !errors.As(err, &e) || e.Code != api.CodeNotFound {
			return res.SessionID, err
		}
		// The session was deleted: start a new one below.
	}
	res, err := t.svc.SendMessage(ctx, api.MessageRequest{Dir: tp.Project, Text: text, Plan: plan})
	if err != nil {
		return "", err
	}
	t.mu.Lock()
	tp.SessionID = res.SessionID
	t.state.Topics[thread] = tp
	t.mu.Unlock()
	t.save()
	if res.Warning != "" {
		t.reply(ctx, thread, res.Warning)
	}
	return res.SessionID, nil
}

// command runs a slash command. "/bind@MyBot x" works too.
func (t *Telegram) command(ctx context.Context, thread int64, text string) {
	name, arg, _ := strings.Cut(strings.TrimPrefix(text, "/"), " ")
	name, _, _ = strings.Cut(name, "@")
	arg = strings.TrimSpace(arg)
	t.mu.Lock()
	tp, bound := t.state.Topics[thread]
	t.mu.Unlock()
	switch name {
	case "bind", "project":
		t.bind(ctx, thread, arg)
	case "status":
		t.status(ctx, thread)
	case "plan":
		if arg == "" {
			t.reply(ctx, thread, "Usage: /plan <task>. The session plans without changing anything and saves the plan.")
			return
		}
		t.ask(ctx, thread, arg, true)
	case "unbind":
		t.mu.Lock()
		delete(t.state.Topics, thread)
		t.mu.Unlock()
		t.save()
		t.reply(ctx, thread, "Unbound. This topic talks to the overseer now.")
	case "new":
		if !bound {
			t.reply(ctx, thread, "This topic talks to the overseer; /bind it to a project first.")
			return
		}
		t.mu.Lock()
		tp.SessionID = ""
		t.state.Topics[thread] = tp
		t.mu.Unlock()
		t.save()
		t.reply(ctx, thread, "The next message starts a new session in "+tp.Project+".")
	case "stop":
		id := api.OverseerID
		if bound {
			id = tp.SessionID
		}
		if id == "" {
			t.reply(ctx, thread, "Nothing is running here.")
			return
		}
		if err := t.svc.StopSession(ctx, api.StopRequest{ID: id}); err != nil {
			t.reply(ctx, thread, err.Error())
			return
		}
		t.reply(ctx, thread, "Stopped.")
	case "sessions":
		t.listSessions(ctx, thread)
	default:
		var b strings.Builder
		b.WriteString("A topic bound to a project talks to that project's session; General and other topics talk to the overseer.\n\n")
		for _, c := range commands {
			fmt.Fprintf(&b, "/%s - %s\n", c.Name, c.Help)
		}
		t.reply(ctx, thread, b.String())
	}
}

// setCommands shows the bot's commands in the group's menu (replacing a
// menu an earlier bot left there).
func (t *Telegram) setCommands(ctx context.Context) {
	var list []map[string]string
	for _, c := range commands {
		list = append(list, map[string]string{"command": c.Name, "description": c.Help})
	}
	params := map[string]any{"commands": list, "scope": map[string]any{"type": "chat", "chat_id": t.opts.ChatID}}
	if err := t.bot.call(ctx, "setMyCommands", params, nil); err != nil {
		slog.Warn("telegram set commands", "err", err)
	}
}

// status tells what the topic talks to, how its session is doing, and
// which jobs run (in its project, or all of them in General).
func (t *Telegram) status(ctx context.Context, thread int64) {
	var b strings.Builder
	if d, err := t.svc.GetDaemon(ctx); err == nil {
		fmt.Fprintf(&b, "Germes %s\n", d.Version)
	}
	t.mu.Lock()
	tp, bound := t.state.Topics[thread]
	t.mu.Unlock()
	sessionID := api.OverseerID
	if bound {
		fmt.Fprintf(&b, "Project: %s\n", tp.Project)
		sessionID = tp.SessionID
	} else {
		b.WriteString("Talks to: the overseer\n")
	}
	s, err := api.Session{}, errors.New("no session")
	if sessionID != "" {
		s, err = t.svc.GetSession(ctx, sessionID)
	}
	if err == nil {
		engine := s.Engine
		if engine == "" {
			engine = "none yet"
		}
		where := "the project folder"
		if s.Branch != "" {
			where = "worktree " + s.Branch
		}
		fmt.Fprintf(&b, "Session: %s, %s, engine %s, in %s", s.ID, s.Status, engine, where)
		if len(s.Queued) > 0 {
			fmt.Fprintf(&b, ", %d queued", len(s.Queued))
		}
		b.WriteString("\n")
	} else {
		b.WriteString("Session: none yet (the next message starts one)\n")
	}
	jobs, err := t.svc.ListJobs(ctx, api.JobFilter{Status: api.JobRunning, Limit: 50})
	var lines []string
	for _, j := range jobs.Items {
		if bound && j.Dir != tp.Project && !strings.HasPrefix(j.Dir, tp.Project+"/") {
			continue
		}
		line := fmt.Sprintf("- %s: round %d", j.Name, j.Runs)
		if j.Step != "" {
			line += ", step " + j.Step
		}
		lines = append(lines, line)
	}
	switch {
	case err != nil:
		fmt.Fprintf(&b, "Jobs: %v\n", err)
	case len(lines) == 0:
		b.WriteString("Jobs: none running\n")
	default:
		b.WriteString("Running jobs:\n" + strings.Join(lines, "\n"))
	}
	t.reply(ctx, thread, b.String())
}

func (t *Telegram) listSessions(ctx context.Context, thread int64) {
	page, err := t.svc.ListSessions(ctx, api.SessionFilter{Limit: 15})
	if err != nil {
		t.reply(ctx, thread, err.Error())
		return
	}
	var b strings.Builder
	for _, s := range page.Items {
		fmt.Fprintf(&b, "%s  %s  %s\n", s.ID, s.Status, s.Title)
	}
	t.reply(ctx, thread, b.String())
}

// bind binds the topic to the project name finds: one match binds, several
// show buttons. No name: recent projects as buttons.
func (t *Telegram) bind(ctx context.Context, thread int64, name string) {
	if thread == 0 {
		t.reply(ctx, thread, "General always talks to the overseer. Use /bind inside a topic.")
		return
	}
	var found []string
	if name == "" {
		found = t.recentProjects(ctx)
	} else {
		found = t.findProjects(ctx, name)
	}
	switch {
	case len(found) == 0:
		t.reply(ctx, thread, "No project matches. Give its folder name or full path.")
	case len(found) == 1 && name != "":
		t.bindTo(ctx, thread, found[0])
	default:
		found = found[:min(len(found), 8)]
		token := api.NewID()
		t.mu.Lock()
		t.choices[token] = found
		t.mu.Unlock()
		var rows [][]button
		for i, p := range found {
			rows = append(rows, []button{{Text: p, Data: fmt.Sprintf("b:%s:%d", token, i)}})
		}
		if err := t.bot.send(ctx, t.opts.ChatID, thread, "Which project?", rows); err != nil {
			slog.Warn("telegram send", "err", err)
		}
	}
}

// handleChoice binds the project picked with a /bind button.
func (t *Telegram) handleChoice(ctx context.Context, thread int64, data string) {
	var token string
	var i int
	if _, err := fmt.Sscanf(strings.ReplaceAll(data, ":", " "), "b %s %d", &token, &i); err != nil {
		return
	}
	t.mu.Lock()
	found := t.choices[token]
	t.mu.Unlock()
	if i < 0 || i >= len(found) {
		t.reply(ctx, thread, "That choice is old; run /bind again.")
		return
	}
	t.bindTo(ctx, thread, found[i])
}

func (t *Telegram) bindTo(ctx context.Context, thread int64, project string) {
	t.mu.Lock()
	t.state.Topics[thread] = topic{Project: project}
	t.mu.Unlock()
	t.save()
	t.reply(ctx, thread, "Bound to "+project+". Messages here go to a session there (ask it for a worktree if you want one).")
}

// reply sends text to a topic, logging failures (the user cannot be told).
func (t *Telegram) reply(ctx context.Context, thread int64, text string) {
	if err := t.bot.send(ctx, t.opts.ChatID, thread, text, nil); err != nil {
		slog.Warn("telegram send", "err", err)
	}
}

// save writes the state. It is small and rebuilt by /bind if lost.
func (t *Telegram) save() {
	t.mu.Lock()
	b, err := json.MarshalIndent(t.state, "", "  ")
	t.mu.Unlock()
	if err == nil {
		err = os.WriteFile(t.opts.StatePath, b, 0o600)
	}
	if err != nil {
		slog.Warn("telegram: save state", "err", err)
	}
}

// recovered runs fn and logs a panic instead of crashing the daemon.
func recovered(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error(name+" panicked", "panic", r, "stack", string(debug.Stack()))
		}
	}()
	fn()
}

func wait(ctx context.Context, d time.Duration) {
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
}
