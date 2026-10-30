package loop

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/viminizer/germes/internal/llm"
)

const (
	clarifyTool  = "clarify"
	maxQuestions = 4
)

// clarifySpec is the clarify tool. It lives in the loop: asking ends the
// turn, and the user's next message is the answer (in the TUI or Telegram
// alike, as the daemon cannot hold a turn open for a form).
var clarifySpec = llm.ToolSpec{
	Name: clarifyTool,
	Description: fmt.Sprintf("Ask the user when an answer would change what you do and no tool can find it out. "+
		"Put all your questions in one call (at most %d), each with 2 to 4 options, the one you recommend first. "+
		"Your turn ends after this call; the user answers in their next message. "+
		"Do not ask what you can check yourself, and do not ask when the request has an obvious meaning.", maxQuestions),
	Schema: json.RawMessage(`{"type":"object","properties":{"questions":{"type":"array","items":{"type":"object","properties":{` +
		`"question":{"type":"string"},"options":{"type":"array","items":{"type":"string"},"description":"the recommended one first"}},` +
		`"required":["question"]}}},"required":["questions"],"additionalProperties":false}`),
}

// clarify turns the questions into the message the user sees. ok false:
// the call was malformed, and text says why.
func clarify(args json.RawMessage) (text string, ok bool) {
	var a struct {
		Questions []struct {
			Question string   `json:"question"`
			Options  []string `json:"options"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "bad arguments: " + err.Error(), false
	}
	if len(a.Questions) == 0 || len(a.Questions) > maxQuestions {
		return fmt.Sprintf("give 1 to %d questions", maxQuestions), false
	}
	var b strings.Builder
	b.WriteString("Before I go on, I need your answer:\n")
	for i, q := range a.Questions {
		if strings.TrimSpace(q.Question) == "" {
			return fmt.Sprintf("question %d is empty", i+1), false
		}
		if len(a.Questions) > 1 {
			fmt.Fprintf(&b, "\n%d. %s\n", i+1, q.Question)
		} else {
			fmt.Fprintf(&b, "\n%s\n", q.Question)
		}
		for j, o := range q.Options {
			fmt.Fprintf(&b, "   %c) %s", 'a'+j, o)
			if j == 0 {
				b.WriteString(" (recommended)")
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("\nReply with your choice (like ")
	if len(a.Questions) > 1 {
		b.WriteString("1a, 2b")
	} else {
		b.WriteString("a")
	}
	b.WriteString(") or in your own words.")
	return b.String(), true
}
