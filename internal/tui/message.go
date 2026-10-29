package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/viminizer/germes/internal/api"
)

// message is one finished entry of the transcript.
type message struct {
	Kind messageKind
	Text string
	Tool *api.ToolCall // call with its result, for kindTool
}

type messageKind int

const (
	kindUser messageKind = iota
	kindAssistant
	kindTool
	kindError
	kindNotice
)

// view renders the message for the given width.
func (m message) view(width int) string {
	switch m.Kind {
	case kindUser:
		return "\n" + gutter(styleLabel.Bold(true).Render("❯ "), styleLabel, m.Text, width) + "\n"
	case kindAssistant:
		return gutter(styleBorder.Render("┊ "), styleText, m.Text, width)
	case kindTool:
		return toolView(m.Tool, width)
	case kindError:
		return gutter(styleError.Render("✗ "), styleError, m.Text, width)
	default:
		return gutter(styleMuted.Render("· "), styleMuted, m.Text, width)
	}
}

// gutter wraps text to width and puts mark before the first line.
func gutter(mark string, style lipgloss.Style, text string, width int) string {
	w := max(width-lipgloss.Width(mark), 10)
	lines := strings.Split(style.Width(w).Render(text), "\n")
	pad := strings.Repeat(" ", lipgloss.Width(mark))
	for i := range lines {
		if i == 0 {
			lines[i] = mark + lines[i]
		} else {
			lines[i] = pad + lines[i]
		}
	}
	return strings.Join(lines, "\n")
}

// toolView is `● Name("preview")` and, once done, one dim result line.
func toolView(t *api.ToolCall, width int) string {
	dot, label := styleAccent.Render("● "), styleText
	if t.IsError {
		dot, label = styleError.Render("✗ "), styleError
	}
	head := dot + label.Render(ansi.Truncate(toolLabel(t), max(width-2, 10), "…"))
	if t.Output == "" {
		return head
	}
	result := firstLine(t.Output)
	if n := strings.Count(strings.TrimSpace(t.Output), "\n"); n > 0 {
		result += fmt.Sprintf(" (+%d lines)", n)
	}
	return head + "\n" + styleDim.Render(ansi.Truncate("  └─ "+result, max(width, 10), "…"))
}

// toolLabel is Name("preview"), with the most telling argument as preview.
func toolLabel(t *api.ToolCall) string {
	var args map[string]any
	json.Unmarshal(t.Args, &args)
	preview := ""
	for _, k := range []string{"command", "file_path", "pattern", "url", "query", "description"} {
		if s, ok := args[k].(string); ok && s != "" {
			preview = s
			break
		}
	}
	if preview == "" {
		return t.Name
	}
	return fmt.Sprintf("%s(%q)", t.Name, ansi.Truncate(firstLine(preview), 64, "…"))
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}
