package tui

import (
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// composer is the input: no border, `❯` prompt, grows with its lines.
// Enter sends; Shift+Enter or Alt+Enter adds a line.
type composer struct {
	area textarea.Model
}

// newComposer creates the input.
func newComposer() composer {
	a := textarea.New()
	a.ShowLineNumbers = false
	a.Placeholder = "Ask me anything…"
	a.DynamicHeight = true
	a.MinHeight = 1
	a.MaxHeight = 10
	a.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("shift+enter", "alt+enter", "ctrl+j"))
	a.SetPromptFunc(2, func(info textarea.PromptInfo) string {
		if info.LineNumber == 0 {
			return lipgloss.NewStyle().Foreground(colorText).Bold(true).Render("❯ ")
		}
		return "  "
	})
	styles := textarea.DefaultStyles(true)
	styles.Focused.CursorLine = lipgloss.NewStyle()
	styles.Focused.Placeholder = styleMuted
	styles.Focused.Text = styleText
	styles.Blurred = styles.Focused
	a.SetStyles(styles)
	a.Focus()
	return composer{area: a}
}

// update passes keys to the text area.
func (c composer) update(msg tea.Msg) (composer, tea.Cmd) {
	var cmd tea.Cmd
	c.area, cmd = c.area.Update(msg)
	return c, cmd
}

// value is the text typed so far.
func (c composer) value() string { return strings.TrimSpace(c.area.Value()) }

// raw is the text as typed, spaces kept (completion needs them).
func (c composer) raw() string { return c.area.Value() }

// setValue replaces the input; the cursor goes to the end.
func (c *composer) setValue(s string) { c.area.SetValue(s) }

// reset clears the input.
func (c *composer) reset() { c.area.Reset() }

// setWidth fits the input to the terminal.
func (c *composer) setWidth(w int) { c.area.SetWidth(max(w, 10)) }

// setPlaceholder changes the hint shown when empty.
func (c *composer) setPlaceholder(s string) { c.area.Placeholder = s }

// view renders the input.
func (c composer) view() string { return c.area.View() }
