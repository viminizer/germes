// Package text works on plain text: shortened strings, exact replacements
// and session transcripts.
package text

import (
	"errors"
	"fmt"
	"strings"

	"github.com/viminizer/germes/internal/api"
)

// Shorten cuts s to n characters, ending with "…" when it was cut.
func Shorten(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// Replace puts text in the place old appears in s, or in every place with
// all. Models often escape text that is not escaped in s (\\ \" \'); that
// is undone only when the result appears exactly once. It returns the new s
// and how many places changed.
func Replace(s, old, text string, all bool) (string, int, error) {
	if old == "" || old == text {
		return "", 0, errors.New("the old text must be non-empty and differ from the new text")
	}
	count := strings.Count(s, old)
	if count == 0 {
		unescape := strings.NewReplacer(`\\`, `\`, `\"`, `"`, `\'`, `'`).Replace
		if u := unescape(old); u != old && strings.Count(s, u) == 1 {
			old, text, count = u, unescape(text), 1
		}
	}
	switch {
	case count == 0:
		return "", 0, errors.New("the old text was not found; read it again and copy it exactly")
	case count > 1 && !all:
		return "", 0, fmt.Errorf("the old text was found %d times; quote more of it", count)
	case all:
		return strings.ReplaceAll(s, old, text), count, nil
	}
	return strings.Replace(s, old, text, 1), 1, nil
}

// Transcript is a session's events, written as plain text for a summary, a
// handoff or a review.
type Transcript []api.Event

func (t Transcript) String() string {
	var b strings.Builder
	for _, ev := range t {
		switch ev.Kind {
		case api.KindUserMessage:
			fmt.Fprintf(&b, "User: %s\n", Shorten(ev.Text, 2000))
		case api.KindAttachment:
			fmt.Fprintf(&b, "Attached: %s\n", Shorten(ev.Text, 1000))
		case api.KindText:
			fmt.Fprintf(&b, "Assistant: %s\n", Shorten(ev.Text, 2000))
		case api.KindToolCall:
			if ev.Tool != nil {
				fmt.Fprintf(&b, "Tool call: %s %s\n", ev.Tool.Name, Shorten(string(ev.Tool.Args), 500))
			}
		case api.KindToolResult:
			if ev.Tool != nil {
				fmt.Fprintf(&b, "Tool result: %s\n", Shorten(ev.Tool.Output, 800))
			}
		case api.KindError, api.KindLimitHit:
			fmt.Fprintf(&b, "Note: %s\n", ev.Text)
		}
	}
	return b.String()
}
