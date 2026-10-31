package tui

import (
	"context"
	"fmt"
	"os/exec"
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
	sessionsMsg   struct {
		gen  int
		page api.Page[api.Session]
		err  error
	}
	flushMsg struct{}
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
	picker     sessionPicker
	status     statusBar
	composer   composer
}

func newModel(ctx context.Context, svc api.Service, opts Options) *model {
	m := &model{
		ctx: ctx, svc: svc, opts: opts, sessionID: opts.SessionID,
		transcript: transcript{dir: opts.Dir},
		status:     newStatus(opts.Engine, opts.Dir, opts.SessionID, time.Now()),
		composer:   newComposer(),
	}
	// One of Hermes' hints, a different one per window, like Hermes.
	m.composer.setPlaceholder(placeholders[time.Now().Nanosecond()%len(placeholders)])
	return m
}

func (m *model) Init() tea.Cmd {
	var cmds []tea.Cmd
	if m.sessionID != "" {
		cmds = append(cmds, m.watch(), m.checkJob())
	}
	if m.opts.First != "" {
		cmds = append(cmds, m.send(api.MessageRequest{Text: m.opts.First}))
	}
	if m.opts.Pick {
		cmds = append(cmds, m.openPicker())
	}
	return tea.Batch(append(cmds, m.loadSkills(), clock(), readBranch(m.ctx, m.status.Dir))...)
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width-marginWidth, msg.Height
		m.composer.setWidth(m.width)
		if !m.started {
			m.started = true
			return m, m.printBanner()
		}
		return m, nil

	case tea.KeyPressMsg:
		return m.key(msg)

	case tea.PasteMsg:
		if m.picker.open {
			// A paste filters the list; the hidden input stays empty.
			m.picker.setFilter(m.picker.filter + strings.TrimSpace(msg.Content))
			return m, m.loadMoreSessions()
		}

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
			return m, tea.Batch(warn, m.watch())
		}
		return m, warn

	case sessionMsg:
		return m, m.showSession(msg.sess)

	case branchMsg:
		if msg.dir == m.status.Dir {
			m.status.Branch = msg.branch
		}
		return m, nil

	case skillsMsg:
		m.suggest.skills = msg.skills
		return m, nil

	case printMsg:
		return m, m.print(msg.msgs...)

	case sessionsMsg:
		if msg.gen != m.picker.gen || !m.picker.open {
			return m, nil
		}
		m.picker.add(msg.page, msg.err)
		return m, m.loadMoreSessions()

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
		return m, tea.Batch(next(msg.gen, msg.ch), m.showSession(msg.sess))

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

	case clockMsg:
		return m, clock() // a redraw: session age and idle time count up
	}

	var cmd tea.Cmd
	m.composer, cmd = m.composer.update(msg)
	return m, cmd
}

func (m *model) key(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.picker.open {
		return m, m.pickerKey(msg)
	}
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
		return m, m.send(api.MessageRequest{Text: text})
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
		m.status.LastEnd = eventTime(ev) // idle since, like Hermes
		if ev.Context != nil {
			m.status.Context = ev.Context
			m.status.CacheInput += ev.Context.Input
			m.status.CacheRead += ev.Context.Cached
		}
		m.status.Busy = false
		m.composer.setPlaceholder("") // like Hermes: the hint is only for a fresh session
		return m.loadSession()        // the turn may have left its worktree, or got a title
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

// send sends a message to the session (a new one in Dir if none).
func (m *model) send(req api.MessageRequest) tea.Cmd {
	req.SessionID, req.Dir, req.Worktree = m.sessionID, m.opts.Dir, m.opts.Worktree
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

// clock redraws the status line once a second, so its times count up while
// nothing else happens.
func clock() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return clockMsg{} })
}

type clockMsg struct{}

// newStatus is the status line of a view of a session ("" = a new one):
// the folder (its branch comes from readBranch), and for a new session its
// age from now, like Hermes, which starts the session when it opens.
func newStatus(engine, dir, sessionID string, now time.Time) statusBar {
	s := statusBar{Engine: engine, Dir: dir}
	if sessionID == "" {
		s.Started = now
	}
	return s
}

// branchMsg is a folder's git branch, read in the background.
type branchMsg struct{ dir, branch string }

// branchTimeout bounds the git call: a slow folder must not stall the view
// (Hermes waits 500 ms too, hooks/useGitBranch.ts).
const branchTimeout = 500 * time.Millisecond

// readBranch reads the folder's git branch in the background; none for a
// folder that is not a repository or a detached HEAD, like Hermes. The TUI
// runs git itself: it may only import api (R5), so it cannot share
// prompt's git state.
func readBranch(ctx context.Context, dir string) tea.Cmd {
	if dir == "" {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, branchTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD")
		cmd.WaitDelay = branchTimeout // a child holding the pipe open does not keep us waiting
		out, err := cmd.Output()
		branch := strings.TrimSpace(string(out))
		if err != nil || branch == "HEAD" {
			branch = ""
		}
		return branchMsg{dir: dir, branch: branch}
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
		m.unprinted = append(m.unprinted, strings.Split(margin(msg.view(m.width)), "\n")...)
	}
	m.pending = nil
	n := min(len(m.unprinted), max(m.height-lipgloss.Height(m.View().Content), 1))
	out := strings.Join(m.unprinted[:n], "\n")
	m.unprinted = m.unprinted[n:]
	return out
}

func (m *model) View() tea.View {
	// The bottom first: a blank line, then the status line (or the
	// suggestions in its place) and the input close the app, like Hermes.
	now := time.Now()
	bottom := []string{""}
	if sug := m.suggest.view(m.width); sug != "" && !m.picker.open {
		bottom = append(bottom, sug)
	} else {
		bottom = append(bottom, m.status.view(m.width, now))
	}
	if m.picker.open {
		bottom = append(bottom, m.picker.view(m.width, now))
	} else {
		bottom = append(bottom, m.composer.view())
	}
	// Above it the queue, and the live part of the transcript in the rows
	// that are left: the whole live area must fit on the screen.
	var parts []string
	queue := m.queue.view(m.width)
	room := m.height - lipgloss.Height(strings.Join(bottom, "\n")) - 1 // one spare row
	if queue != "" {
		room -= lipgloss.Height(queue)
	}
	if live := m.transcript.view(m.width, room, m.status.spinner(), now); live != "" {
		parts = append(parts, live)
	}
	if queue != "" {
		parts = append(parts, queue)
	}
	return tea.NewView(margin(strings.Join(append(parts, bottom...), "\n")))
}

// marginWidth is what the app leaves free, like Hermes: one column on the
// left and one on the right (Hermes' scrollbar).
const marginWidth = 2

// margin moves every line one column right.
func margin(s string) string { return " " + strings.ReplaceAll(s, "\n", "\n ") }

// showSession puts the session's title, start and branch in the status
// line.
func (m *model) showSession(s api.Session) tea.Cmd {
	m.status.Title = s.Title
	if !s.CreatedAt.IsZero() {
		m.status.Started = s.CreatedAt
	}
	if s.Dir == "" || s.Dir == m.status.Dir {
		return nil
	}
	// The session works elsewhere (its worktree, or back in the project):
	// that folder and its branch.
	m.status.Dir, m.status.Branch = s.Dir, s.Branch
	return readBranch(m.ctx, s.Dir)
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
	out = margin(out)
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

// openPicker shows the session picker and loads its first page (/resume,
// `germes resume`).
func (m *model) openPicker() tea.Cmd {
	m.picker.start()
	return m.loadSessions("")
}

// loadMoreSessions loads the picker's next page when it needs one.
func (m *model) loadMoreSessions() tea.Cmd {
	if !m.picker.hasPageToLoad() {
		return nil
	}
	m.picker.loading = true
	return m.loadSessions(m.picker.next)
}

func (m *model) loadSessions(cursor string) tea.Cmd {
	gen, f := m.picker.gen, api.SessionFilter{Project: m.opts.Project, Limit: pickerPage, Cursor: cursor}
	return func() tea.Msg {
		page, err := m.svc.ListSessions(m.ctx, f)
		return sessionsMsg{gen: gen, page: page, err: err}
	}
}

// pickerKey handles a key while the session picker is open.
func (m *model) pickerKey(msg tea.KeyPressMsg) tea.Cmd {
	p := &m.picker
	switch msg.String() {
	case "ctrl+c":
		m.stopWatch()
		return tea.Quit
	case "up":
		p.move(-1)
	case "down":
		p.move(1)
	case "enter":
		s, ok := p.current()
		if !ok {
			return nil
		}
		p.open = false
		return func() tea.Msg { return switchMsg{id: s.ID} }
	case "esc":
		if p.filter == "" {
			p.open = false
			return nil
		}
		p.setFilter("")
	case "backspace":
		if r := []rune(p.filter); len(r) > 0 {
			p.setFilter(string(r[:len(r)-1]))
		}
	default:
		if msg.Text == "" {
			return nil
		}
		p.setFilter(p.filter + msg.Text)
	}
	return m.loadMoreSessions()
}
