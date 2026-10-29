// Package text turns things into short plain text: shortened strings and
// session transcripts.
package text

import (
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

// Transcript is a session's events, written as plain text for a summary, a
// handoff or a review.
type Transcript []api.Event

func (t Transcript) String() string {
	var b strings.Builder
	for _, ev := range t {
		switch ev.Kind {
		case api.KindUserMessage:
			fmt.Fprintf(&b, "User: %s\n", Shorten(ev.Text, 2000))
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
