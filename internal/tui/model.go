package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/viminizer/germes/internal/api"
)

// Messages the model sends itself.
type (
	tickMsg struct{}
	sentMsg struct {
		sessionID string
		warning   string
		err       error
	}
	sessionMsg struct{ sess api.Session }
	printMsg   struct{ msgs []message }
	watchMsg   struct {
		gen   int
		ch    <-chan api.Event
		queue []string    // what was waiting when the watch started
		sess  api.Session // the session when the watch started
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
	jobMsg        struct{ job api.Job }
	flushMsg      struct{}
)

// printDelay gathers the messages of one burst into one print.
const printDelay = 30 * time.Millisecond

// jobCheck is how often an attached job is checked for a new round.
const jobCheck = 2 * time.Second

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
	height    int
	started   bool // banner printed

	startNotice   string // printed under the banner (after /clear)
	restart       bool   // /clear: Run starts a fresh program on a clean screen
	restartNotice string

	gen         int // watch generation; events from older watches are ignored
	watchCancel context.CancelFunc

	pending   []message // finished messages waiting to be printed together
	unprinted []string  // rendered lines that did not fit in the last print
	flushing  bool      // a flush is scheduled

	escPending bool // one Esc pressed; a second within the window stops the turn
	escGen     int  // ignores timeouts of older Esc presses

	transcript transcript
	queue      queueView
	suggest    suggestBox
	rule       titleRule
	status     statusBar
	composer   composer
}

func newModel(ctx context.Context, svc api.Service, opts Options) *model {
	return &model{
		ctx: ctx, svc: svc, opts: opts, sessionID: opts.SessionID,
		transcript: transcript{dir: opts.Dir},
		rule:       titleRule{Project: opts.Project},
		status:     statusBar{Engine: opts.Engine},
		composer:   newComposer(),
	}
}

func (m *model) Init() tea.Cmd {
	var cmds []tea.Cmd
	if m.sessionID != "" {
		cmds = append(cmds, m.watch(), m.checkJob())
	}
	if m.opts.First != "" {
		cmds = append(cmds, m.send(m.opts.First, false))
	}
	return tea.Batch(cmds...)
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.composer.setWidth(msg.Width)
		if !m.started {
			m.started = true
			return m, m.printBanner()
		}
		return m, nil

	case tea.KeyPressMsg:
		return m.key(msg)

	case sentMsg:
		if msg.err != nil {
			return m, m.print(message{Kind: kindError, Text: msg.err.Error()})
		}
		var warn tea.Cmd
		if msg.warning != "" {
			warn = m.print(message{Kind: kindError, Text: msg.warning})
		}
		if m.sessionID == "" {
			m.sessionID = msg.sessionID
			m.status.Started = time.Now()
			return m, tea.Batch(warn, m.watch())
		}
		return m, warn

	case sessionMsg:
		m.showSession(msg.sess)
		return m, nil

	case printMsg:
		return m, m.print(msg.msgs...)

	case switchMsg:
		m.opts.JobID = "" // picked a session by hand: stop following the job
		return m, tea.Batch(m.print(message{Kind: kindNotice, Text: "switched to " + msg.id}), m.open(msg.id))

	case watchMsg:
		if msg.gen != m.gen {
			return m, nil
		}
		if msg.err != nil {
			return m, m.retryWatch(msg.gen)
		}
		m.queue.items = msg.queue
		m.showSession(msg.sess)
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

	case flushMsg:
		m.flushing = false
		if len(m.pending) == 0 && len(m.unprinted) == 0 {
			return m, nil
		}
		out := m.flush()
		return m, tea.Batch(tea.Println(out), m.scheduleFlush())

	case jobMsg:
		// Follow the job to its new round.
		if msg.job.SessionID != "" && msg.job.SessionID != m.sessionID {
			marker := fmt.Sprintf("round %d", msg.job.Runs)
			return m, tea.Batch(m.print(message{Kind: kindRound, Text: marker}), m.open(msg.job.SessionID), m.checkJob())
		}
		if msg.job.Status != "" && msg.job.Status != api.JobRunning {
			return m, nil // no more rounds
		}
		return m, m.checkJob()

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
	if len(m.suggest.items) > 0 {
		switch msg.String() {
		case "up":
			m.suggest.move(-1)
			return m, nil
		case "down":
			m.suggest.move(1)
			return m, nil
		case "esc":
			m.suggest.closed, m.suggest.items = m.composer.raw(), nil
			return m, nil
		case "tab", "enter":
			// Enter completes first; once nothing changes, it sends.
			if done, changed := m.suggest.complete(m.composer.raw()); changed || msg.String() == "tab" {
				m.composer.setValue(done)
				m.suggest.update(done, m.opts.Dir)
				return m, nil
			}
		}
	}
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
		m.suggest.update("", m.opts.Dir)
		if strings.HasPrefix(text, "/") {
			return m, m.command(text)
		}
		return m, m.send(text, false)
	}
	var cmd tea.Cmd
	m.composer, cmd = m.composer.update(msg)
	m.suggest.update(m.composer.raw(), m.opts.Dir)
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
			// The event's time, not now: a replayed turn keeps its real length.
			m.status.Busy, m.status.Since, m.status.Note = true, eventTime(ev), ""
			m.composer.setPlaceholder("Esc Esc to stop…")
			return tick()
		}
	case api.KindQueue:
		m.queue.items = ev.Queue
	case api.KindTurnDone:
		if m.status.Busy {
			m.status.LastTurn = eventTime(ev).Sub(m.status.Since)
		}
		if ev.Context != nil {
			m.status.Context = ev.Context
		}
		m.status.Busy = false
		m.composer.setPlaceholder("Ask me anything…")
		return m.loadSession() // the turn may have left its worktree, or got a title
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

// send sends text to the session (a new one in Dir if none). plan: a plan
// turn, which changes nothing and saves its reply as a plan.
func (m *model) send(text string, plan bool) tea.Cmd {
	req := api.MessageRequest{SessionID: m.sessionID, Dir: m.opts.Dir, Text: text, Worktree: m.opts.Worktree, Plan: plan}
	return func() tea.Msg {
		res, err := m.svc.SendMessage(m.ctx, req)
		return sentMsg{sessionID: res.SessionID, warning: res.Warning, err: err}
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
		return watchMsg{gen: gen, ch: ch, queue: s.Queued, sess: s, err: err}
	}
}

// checkJob reads the attached job after a short wait. Not attached: nothing.
func (m *model) checkJob() tea.Cmd {
	id := m.opts.JobID
	if id == "" {
		return nil
	}
	return tea.Tick(jobCheck, func(time.Time) tea.Msg {
		j, err := m.svc.GetJob(m.ctx, id)
		if err != nil {
			return jobMsg{} // the daemon may be restarting; check again later
		}
		return jobMsg{job: j}
	})
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
// Messages wait a moment and go out together in one print: several prints
// in one frame lose lines (a replayed history is a burst of them). Until
// the banner is out, they wait for it.
func (m *model) print(msgs ...message) tea.Cmd {
	if len(msgs) == 0 {
		return nil
	}
	m.pending = append(m.pending, msgs...)
	if !m.started {
		return nil
	}
	return m.scheduleFlush()
}

// scheduleFlush prints what waits a moment from now, unless that is
// already planned.
func (m *model) scheduleFlush() tea.Cmd {
	if m.flushing || (len(m.pending) == 0 && len(m.unprinted) == 0) {
		return nil
	}
	m.flushing = true
	return tea.Tick(printDelay, func(time.Time) tea.Msg { return flushMsg{} })
}

// flush renders the waiting messages for the current width and returns
// the lines that fit above the live area; the rest wait for the next
// flush. Bubble Tea prints above the live area as if it were at the bottom
// of the screen: a print taller than the screen loses lines.
func (m *model) flush() string {
	for _, msg := range m.pending {
		m.unprinted = append(m.unprinted, strings.Split(msg.view(m.width), "\n")...)
	}
	m.pending = nil
	n := min(len(m.unprinted), max(m.height-lipgloss.Height(m.View().Content), 1))
	out := strings.Join(m.unprinted[:n], "\n")
	m.unprinted = m.unprinted[n:]
	return out
}

func (m *model) View() tea.View {
	// Running tools, then the reply's unfinished line (blank when none: it
	// also spaces the transcript from the status bar).
	parts := []string{m.transcript.view(m.width, m.status.spinner())}
	if q := m.queue.view(m.width); q != "" {
		parts = append(parts, q)
	}
	// A thin rule with the title, the input, then the footer closes the app.
	parts = append(parts, m.rule.view(m.width), m.composer.view())
	if sug := m.suggest.view(m.width); sug != "" {
		parts = append(parts, sug)
	}
	parts = append(parts, m.status.view(m.width))
	return tea.NewView(strings.Join(parts, "\n"))
}

// showSession puts the session's title, start and branch in the rule and
// footer.
func (m *model) showSession(s api.Session) {
	m.rule.Title, m.status.Branch = s.Title, s.Branch
	if !s.CreatedAt.IsZero() {
		m.status.Started = s.CreatedAt
	}
}

// loadSession reads the session for the rule and footer.
func (m *model) loadSession() tea.Cmd {
	id := m.sessionID
	return func() tea.Msg {
		s, err := m.svc.GetSession(m.ctx, id)
		if err != nil {
			return nil
		}
		return sessionMsg{sess: s}
	}
}

// printBanner prints the intro (logo, version, the session's info box);
// the messages that waited for it follow in the next prints.
func (m *model) printBanner() tea.Cmd {
	b := banner{Version: m.opts.Version, Project: m.opts.Project, Session: m.sessionID, Engine: m.status.Engine}
	out := b.view(m.width)
	if m.startNotice != "" {
		out += "\n" + message{Kind: kindNotice, Text: m.startNotice}.view(m.width) + "\n"
	}
	// Blank lines put the live area at the bottom of the screen. Bubble
	// Tea prints above it as if it were there; higher up, lines land on the
	// wrong rows and get lost.
	if pad := m.height - lipgloss.Height(out) - lipgloss.Height(m.View().Content); pad > 0 {
		out += strings.Repeat("\n", pad)
	}
	return tea.Batch(tea.Println(out), m.scheduleFlush())
}

// eventTime is when ev happened; now for an event without a time.
func eventTime(ev api.Event) time.Time {
	if ev.Time.IsZero() {
		return time.Now()
	}
	return ev.Time
}
