package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/viminizer/germes/internal/api"
)

// commands are the slash commands, for /help and the suggestion list.
var commands = []struct{ Name, Args, Help string }{
	{"new", "", "start a new session"},
	{"clear", "", "keep this session saved and start a new one"},
	{"sessions", "", "list sessions in this project"},
	{"switch", "<id>", "open another session"},
	{"rename", "<text>", "rename this session"},
	{"help", "", "this help"},
	{"exit", "", "quit (the session keeps running)"},
}

// helpText lists the commands and keys.
func helpText() string {
	var b strings.Builder
	b.WriteString("Commands:\n")
	for _, c := range commands {
		fmt.Fprintf(&b, "  %-16s%s\n", strings.TrimSpace("/"+c.Name+" "+c.Args), c.Help)
	}
	b.WriteString("References: @file:path[:10-20] @folder:path @diff @staged @git:\"args\" @url:address\n")
	b.WriteString("Keys: Enter send · Shift+Enter or Alt+Enter new line · Tab complete · Ctrl+C quit\n")
	b.WriteString("      Esc Esc stop the turn (queued messages then run right away)")
	return b.String()
}

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
		return notice(helpText())
	case "new", "clear":
		// The current session stays saved; a new one is only saved once a
		// message is sent, so /clear with nothing after it leaves nothing.
		old := m.sessionID
		m.open("")
		if name == "new" {
			return notice("new session")
		}
		m.restartNotice = "new session"
		if old != "" {
			m.restartNotice = "session " + old + " is saved (/switch " + old + " to go back)"
		}
		// /clear also wipes the screen, so it shows one session only.
		m.restart = true
		return tea.Quit
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
	m.transcript = transcript{dir: m.opts.Dir}
	m.queue = queueView{}
	m.status.SessionID, m.status.Busy, m.status.Note = id, false, ""
	if id == "" {
		return nil
	}
	return m.watch()
}
