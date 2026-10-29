package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/viminizer/germes/internal/api"
)

const helpText = `Commands:
  /new            start a new session
  /sessions       list sessions in this project
  /switch <id>    open another session
  /rename <text>  rename this session
  /help           this help
  /exit           quit (the session keeps running)
Keys: Enter send · Shift+Enter or Alt+Enter new line · Esc stop (drops the queue)
      Esc Esc stop and send the queue now · Ctrl+C quit`

// command runs a slash command.
func (m *model) command(text string) tea.Cmd {
	name, arg, _ := strings.Cut(strings.TrimPrefix(text, "/"), " ")
	arg = strings.TrimSpace(arg)
	notice := func(s string) tea.Cmd { return m.print(message{Kind: kindNotice, Text: s}) }
	fail := func(err error) tea.Msg { return printMsg{[]message{{Kind: kindError, Text: err.Error()}}} }

	switch name {
	case "exit", "quit":
		m.stopWatch()
		return tea.Quit
	case "help":
		return notice(helpText)
	case "new":
		m.open("")
		return notice("new session")
	case "switch":
		if arg == "" {
			return notice("usage: /switch <id>")
		}
		return func() tea.Msg {
			if _, err := m.svc.GetSession(m.ctx, arg); err != nil {
				return fail(err)
			}
			return switchMsg{id: arg}
		}
	case "rename":
		if m.sessionID == "" {
			return notice("send a message first, then rename")
		}
		id := m.sessionID
		return func() tea.Msg {
			if err := m.svc.UpdateSession(m.ctx, api.SessionUpdate{ID: id, Title: arg}); err != nil {
				return fail(err)
			}
			return printMsg{[]message{{Kind: kindNotice, Text: "renamed to " + arg}}}
		}
	case "sessions":
		project := m.opts.Project
		return func() tea.Msg {
			page, err := m.svc.ListSessions(m.ctx, api.SessionFilter{Project: project, Limit: 20})
			if err != nil {
				return fail(err)
			}
			if len(page.Items) == 0 {
				return printMsg{[]message{{Kind: kindNotice, Text: "no sessions in this project"}}}
			}
			var b strings.Builder
			for _, s := range page.Items {
				fmt.Fprintf(&b, "%s  %-8s %s\n", s.ID, s.Status, s.Title)
			}
			b.WriteString("open one with /switch <id>")
			return printMsg{[]message{{Kind: kindNotice, Text: b.String()}}}
		}
	}
	return notice("unknown command /" + name + " (try /help)")
}

type switchMsg struct{ id string }

// open shows another session ("" = a new one).
func (m *model) open(id string) tea.Cmd {
	m.stopWatch()
	m.gen++
	m.sessionID = id
	m.transcript = transcript{}
	m.queue = queueView{}
	m.status.SessionID, m.status.Busy, m.status.Note = id, false, ""
	if id == "" {
		return nil
	}
	return m.watch()
}
