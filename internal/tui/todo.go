package tui

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// todoItem is one line of the model's task list.
type todoItem struct {
	Depth int
	Mark  string // [ ] pending, [>] in progress, [x] completed, [-] cancelled
	Text  string
}

// reTodoLine reads a line of the todo_list tool's output: `  - [>] 2. Text`;
// an id may have dots or spaces (`1.1`).
var reTodoLine = regexp.MustCompile(`^(\s*)- (\[.\]) .+?\. (.*)$`)

// parseTodos reads the task list from the todo_list tool's output.
func parseTodos(out string) []todoItem {
	var items []todoItem
	for _, line := range strings.Split(out, "\n") {
		if m := reTodoLine.FindStringSubmatch(line); m != nil {
			items = append(items, todoItem{Depth: len(m[1]) / 2, Mark: m[2], Text: m[3]})
		}
	}
	return items
}

func (it todoItem) open() bool { return it.Mark == "[ ]" || it.Mark == "[>]" }

// maxLiveTodos is the most items the live area shows, from just before
// the first open one; the printed list has them all.
const maxLiveTodos = 10

// todoView draws the task list like the Hermes todo panel (hermes/ui-tui/
// src/components/todoPanel.tsx): `▾ Todo (2/5)`, then the items, the one
// in progress bright, pending ones grey, finished ones dim. Closed, only
// the header shows. A list left open at the end of a turn says so.
// maxRows > 0 keeps the list to that many rows under the header (the
// live area).
func todoView(items []todoItem, width int, closed, incomplete bool, maxRows int) string {
	done, pending := 0, 0
	for _, it := range items {
		if it.Mark == "[x]" {
			done++
		}
		if it.open() {
			pending++
		}
	}
	arrow := "▾ "
	if closed {
		arrow = "▸ "
	}
	head := styleAccent.Render(arrow) + styleText.Bold(true).Render("Todo") + " " +
		styleTime.Render(fmt.Sprintf("(%d/%d)", done, len(items)))
	if incomplete && pending > 0 {
		state := "pending/in_progress"
		if pending == 1 {
			state = "pending"
		}
		head += styleDim.Render(fmt.Sprintf(" · incomplete · %d still %s", pending, state))
	}
	lines := []string{ansi.Truncate(head, max(width, 10), "…")}
	if closed {
		return lines[0]
	}
	shown, more := items, 0
	if maxRows > 0 && len(items) > maxRows {
		first := 0
		for first < len(items) && !items[first].open() {
			first++
		}
		n := max(maxRows-2, 1) // room for "… N done" and "… N more"
		start := max(0, min(first-1, len(items)-n))
		shown, more = items[start:start+n], len(items)-start-n
		if start > 0 {
			lines = append(lines, styleDim.Render(fmt.Sprintf("  … %d done", start)))
		}
	}
	for _, it := range shown {
		style := styleDim
		switch it.Mark {
		case "[>]":
			style = styleText
		case "[ ]":
			style = styleStatus
		}
		row := strings.Repeat("  ", 1+min(it.Depth, 4)) + it.Mark + " " + it.Text
		lines = append(lines, style.Render(ansi.Truncate(row, max(width, 10), "…")))
	}
	if more > 0 {
		lines = append(lines, styleDim.Render(fmt.Sprintf("  … %d more", more)))
	}
	return strings.Join(lines, "\n")
}

// isTodoDone says whether every item is completed or cancelled.
func isTodoDone(items []todoItem) bool {
	for _, it := range items {
		if it.open() {
			return false
		}
	}
	return len(items) > 0
}
