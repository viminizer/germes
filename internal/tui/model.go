package tui

import (
	"context"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/viminizer/germes/internal/api"
)

// Messages the model sends itself.
type (
	tickMsg struct{}
	sentMsg struct {
		sessionID string
		err       error
	}
	printMsg struct{ msgs []message }
	watchMsg struct {
		gen   int
		ch    <-chan api.Event
		queue []string // what was waiting when the watch started
		err   error
	}
	eventMsg struct {
		gen int
		ch  <-chan api.Event
		ev  api.Event
	}
	watchEndMsg   struct{ gen int }
	escTimeoutMsg struct{ gen int }
	reconnectMsg  struct{ gen int }
)

// doubleEscWindow is how long a second Esc may follow the first. One Esc
// never interrupts; Esc Esc does.
const doubleEscWindow = 800 * time.Millisecond

// model is the root: it owns the components and talks to the Service.
type model struct {
	ctx  context.Context
	svc  api.Service
	opts Options

	sessionID string
	width     int
	started   bool // banner printed

	gen         int // watch generation; events from older watches are ignored
	watchCancel context.CancelFunc

	escPending bool // one Esc pressed; a second within the window stops the turn
	escGen     int  // ignores timeouts of older Esc presses

	transcript transcript
	queue      queueView
	status     statusBar
	composer   composer
}

func newModel(ctx context.Context, svc api.Service, opts Options) *model {
	return &model{
		ctx: ctx, svc: svc, opts: opts, sessionID: opts.SessionID,
		transcript: transcript{dir: opts.Dir},
		status:     statusBar{Engine: opts.Engine, SessionID: opts.SessionID, Project: opts.Project},
		composer:   newComposer(),
	}
}

func (m *model) Init() tea.Cmd {
	if m.sessionID != "" {
		return m.watch()
	}
	return nil
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.composer.setWidth(msg.Width)
		if !m.started {
			m.started = true
			b := banner{Version: m.opts.Version, Project: m.opts.Project, Session: m.sessionID, Engine: m.opts.Engine}
			return m, tea.Println(b.view(msg.Width))
		}
		return m, nil

	case tea.KeyPressMsg:
		return m.key(msg)

	case sentMsg:
		if msg.err != nil {
			return m, m.print(message{Kind: kindError, Text: msg.err.Error()})
		}
		if m.sessionID == "" {
			m.sessionID = msg.sessionID
			m.status.SessionID = msg.sessionID
			return m, m.watch()
		}
		return m, nil

	case printMsg:
		return m, m.print(msg.msgs...)

	case switchMsg:
		return m, tea.Batch(m.print(message{Kind: kindNotice, Text: "switched to " + msg.id}), m.open(msg.id))

	case watchMsg:
		if msg.gen != m.gen {
			return m, nil
		}
		if msg.err != nil {
			return m, m.retryWatch(msg.gen)
		}
		m.queue.items = msg.queue
		return m, next(msg.gen, msg.ch)

	case eventMsg:
		if msg.gen != m.gen {
			return m, nil
		}
		cmds := []tea.Cmd{next(msg.gen, msg.ch), m.print(m.transcript.apply(msg.ev)...), m.track(msg.ev)}
		return m, tea.Batch(cmds...)

	case watchEndMsg:
		if msg.gen != m.gen || m.ctx.Err() != nil {
			return m, nil
		}
		return m, m.retryWatch(msg.gen)

	case reconnectMsg:
		if msg.gen != m.gen {
			return m, nil
		}
		return m, m.watch()

	case escTimeoutMsg:
		if !m.escPending || msg.gen != m.escGen {
			return m, nil
		}
		m.escPending, m.status.Hint = false, "" // one Esc alone does nothing
		return m, nil

	case tickMsg:
		m.status.Frame++
		if m.status.Busy {
			return m, tick()
		}
		return m, nil
	}

	var cmd tea.Cmd
	m.composer, cmd = m.composer.update(msg)
	return m, cmd
}

func (m *model) key(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		m.stopWatch()
		return m, tea.Quit
	case "esc":
		if (!m.status.Busy && len(m.queue.items) == 0) || m.sessionID == "" {
			return m, nil
		}
		if m.escPending {
			// Esc Esc: stop the turn; queued messages run right away.
			m.escPending, m.status.Hint = false, ""
			return m, m.stop(true)
		}
		// One Esc does not interrupt: it only arms the second one.
		m.escPending, m.status.Hint = true, "Esc again to stop"

		m.escGen++
		gen := m.escGen
		return m, tea.Tick(doubleEscWindow, func(time.Time) tea.Msg { return escTimeoutMsg{gen: gen} })
	case "enter":
		text := m.composer.value()
		if text == "" {
			return m, nil
		}
		m.composer.reset()
		if strings.HasPrefix(text, "/") {
			return m, m.command(text)
		}
		return m, m.send(text)
	}
	var cmd tea.Cmd
	m.composer, cmd = m.composer.update(msg)
	return m, cmd
}

// track updates the status bar from an event.
func (m *model) track(ev api.Event) tea.Cmd {
	if ev.Engine != "" {
		m.status.Engine = ev.Engine
	}
	switch ev.Kind {
	case api.KindUserMessage:
		if !m.status.Busy {
			m.status.Busy, m.status.Since, m.status.Note = true, time.Now(), ""
			m.composer.setPlaceholder("Esc Esc to stop…")
			return tick()
		}
	case api.KindQueue:
		m.queue.items = ev.Queue
	case api.KindTurnDone:
		m.status.Busy = false
		m.composer.setPlaceholder("Ask me anything…")
	case api.KindError:
		if ev.Text == "stopped" {
			m.status.Note = "interrupted"
		}
	}
	return nil
}

// stop stops the current turn. sendQueue runs the queue right away instead
// of dropping it.
func (m *model) stop(sendQueue bool) tea.Cmd {
	req := api.StopRequest{ID: m.sessionID, SendQueue: sendQueue}
	return func() tea.Msg {
		if err := m.svc.StopSession(m.ctx, req); err != nil {
			return printMsg{[]message{{Kind: kindError, Text: err.Error()}}}
		}
		return nil
	}
}

func (m *model) send(text string) tea.Cmd {
	req := api.MessageRequest{SessionID: m.sessionID, Dir: m.opts.Dir, Text: text}
	return func() tea.Msg {
		res, err := m.svc.SendMessage(m.ctx, req)
		return sentMsg{sessionID: res.SessionID, err: err}
	}
}

// watch starts a new watch of the current session from the last event shown.
func (m *model) watch() tea.Cmd {
	m.stopWatch()
	m.gen++
	gen, id, after := m.gen, m.sessionID, m.transcript.lastSeqShown()
	ctx, cancel := context.WithCancel(m.ctx)
	m.watchCancel = cancel
	return func() tea.Msg {
		ch, err := m.svc.Watch(ctx, api.WatchFilter{SessionID: id, AfterSeq: after})
		if err != nil {
			return watchMsg{gen: gen, err: err}
		}
		s, err := m.svc.GetSession(ctx, id)
		return watchMsg{gen: gen, ch: ch, queue: s.Queued, err: err}
	}
}

func (m *model) stopWatch() {
	if m.watchCancel != nil {
		m.watchCancel()
		m.watchCancel = nil
	}
}

// retryWatch reconnects after a short wait, starting the daemon if it is gone.
func (m *model) retryWatch(gen int) tea.Cmd {
	return func() tea.Msg {
		time.Sleep(500 * time.Millisecond)
		if m.opts.EnsureDaemon != nil {
			m.opts.EnsureDaemon(m.ctx)
		}
		return reconnectMsg{gen: gen}
	}
}

func next(gen int, ch <-chan api.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return watchEndMsg{gen: gen}
		}
		return eventMsg{gen: gen, ch: ch, ev: ev}
	}
}

func tick() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(time.Time) tea.Msg { return tickMsg{} })
}

// print puts finished messages above the live area, in the scrollback.
func (m *model) print(msgs ...message) tea.Cmd {
	if len(msgs) == 0 {
		return nil
	}
	views := make([]string, len(msgs))
	for i, msg := range msgs {
		views[i] = msg.view(m.width)
	}
	return tea.Println(strings.Join(views, "\n"))
}

func (m *model) View() tea.View {
	parts := []string{""} // a blank line between the transcript and the live area
	if live := m.transcript.view(m.width, m.status.spinner()); live != "" {
		parts = append(parts, live)
	}
	if q := m.queue.view(m.width); q != "" {
		parts = append(parts, q)
	}
	// A rule under the input closes the app at the bottom.
	parts = append(parts, m.status.view(m.width), m.composer.view(), styleBorder.Render(strings.Repeat("─", max(m.width, 1))))
	return tea.NewView(strings.Join(parts, "\n"))
}
