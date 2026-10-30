// Package todo has the todo_list tool: a task list the model keeps for
// multi-step work. Each session's list is saved in its own file, so it
// survives compaction and daemon restarts; after a compaction the loop gives
// the model its open items again (GetActive).
package todo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/viminizer/germes/internal/file"
	"github.com/viminizer/germes/internal/tool"
)

const (
	maxItems   = 100
	maxContent = 300 // characters of one item
)

// Item is one task. Parent makes it a subtask of another item.
type Item struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Status  string `json:"status"`
	Parent  string `json:"parent,omitempty"`
}

// marks show each status in the rendered list.
var marks = map[string]string{"pending": "[ ]", "in_progress": "[>]", "completed": "[x]", "cancelled": "[-]"}

// Lists keeps every session's task list in a folder.
type Lists struct {
	dir string
	mu  sync.Mutex
}

// NewLists keeps the lists in dir.
func NewLists(dir string) *Lists { return &Lists{dir: dir} }

// New returns the todo_list tool.
func New(lists *Lists) []tool.Tool {
	return []tool.Tool{{
		Name: "todo_list", Toolset: "todo",
		Description: "Keep a task list for work with 3 or more steps, or when the user gives several tasks. " +
			"For \"all N items\" work, list each one, so none is dropped. Call with no arguments to read the list. " +
			"The order is the priority. Keep only one item in_progress. Split big steps into subtasks with parent. " +
			"Mark an item completed only after you checked it is done, never because you meant to. If something fails, cancel it and add a new item. " +
			"Returns the whole list.",
		Schema: json.RawMessage(`{"type":"object","properties":{` +
			`"todos":{"type":"array","items":{"type":"object","properties":{` +
			`"id":{"type":"string"},"content":{"type":"string"},` +
			`"status":{"type":"string","enum":["pending","in_progress","completed","cancelled"]},` +
			`"parent":{"type":"string","description":"id of the item this is a subtask of"}},` +
			`"required":["id","content","status"]}},` +
			`"replace":{"type":"boolean","description":"true: a new plan, replacing the whole list. Default: update the given items by id and add new ones; send only what changed"}},` +
			`"additionalProperties":false}`),
		Run: tool.NewRun(func(ctx context.Context, c tool.Call, a struct {
			Todos   []Item `json:"todos"`
			Replace bool   `json:"replace"`
		}) (string, error) {
			items, err := lists.update(c.SessionID, a.Todos, a.Replace)
			if err != nil {
				return "", err
			}
			if len(items) == 0 {
				return "the task list is empty", nil
			}
			return render(items, false), nil
		}),
	}}
}

// GetActive renders the session's open items (pending, in progress, and
// the parents of open subtasks) for the model, or "" when none are open.
func (l *Lists) GetActive(sessionID string) string {
	l.mu.Lock()
	items, err := l.load(sessionID)
	l.mu.Unlock()
	if err != nil {
		slog.Warn("read task list", "session", sessionID, "err", err)
		return ""
	}
	open := render(items, true)
	if open == "" {
		return ""
	}
	return "[Germes] Your task list, kept from before the summary (open items only):\n" + open
}

// update applies a write to the session's list and returns the list: the
// given items change by id or are added, or with replace they are the new
// list. No items: a read.
func (l *Lists) update(sessionID string, todos []Item, replace bool) ([]Item, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	items, err := l.load(sessionID)
	if err != nil {
		return nil, err
	}
	if len(todos) == 0 {
		return items, nil
	}
	if replace {
		items = nil
	}
	for _, t := range todos {
		t.ID, t.Content = strings.TrimSpace(t.ID), strings.TrimSpace(t.Content)
		if t.ID == "" {
			return nil, errors.New("every item needs an id")
		}
		if _, ok := marks[t.Status]; !ok {
			t.Status = "pending"
		}
		if r := []rune(t.Content); len(r) > maxContent {
			t.Content = string(r[:maxContent]) + "…"
		}
		i := index(items, t.ID)
		switch {
		case i >= 0:
			if t.Content == "" {
				t.Content = items[i].Content // an update may change only the status
			}
			items[i] = t
		case t.Content == "":
			return nil, fmt.Errorf("item %s needs a content", t.ID)
		default:
			items = append(items, t)
		}
	}
	if len(items) > maxItems {
		return nil, fmt.Errorf("the list has %d items; keep it under %d (split the work or drop finished items)", len(items), maxItems)
	}
	for i := range items {
		// An unknown parent, or one that loops back, makes it a top-level item.
		if p := items[i].Parent; p != "" && (index(items, p) < 0 || isLoop(items, i)) {
			items[i].Parent = ""
		}
	}
	return items, l.save(sessionID, items)
}

func (l *Lists) path(sessionID string) string {
	return filepath.Join(l.dir, filepath.Base(sessionID)+".json")
}

func (l *Lists) load(sessionID string) ([]Item, error) {
	b, err := os.ReadFile(l.path(sessionID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var items []Item
	if err := json.Unmarshal(b, &items); err != nil {
		return nil, fmt.Errorf("task list %s is broken: %w", l.path(sessionID), err)
	}
	return items, nil
}

func (l *Lists) save(sessionID string, items []Item) error {
	b, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return err
	}
	return file.Save(l.path(sessionID), b, 0o600)
}

func index(items []Item, id string) int {
	for i, it := range items {
		if it.ID == id {
			return i
		}
	}
	return -1
}

// render lists items with subtasks under their parents. openOnly keeps
// open items and the parents of open subtasks; finished ones would make the
// model redo work.
func render(items []Item, openOnly bool) string {
	var lines func(parent string, depth int) []string
	lines = func(parent string, depth int) []string {
		var out []string
		for _, it := range items {
			if it.Parent != parent {
				continue
			}
			kids := lines(it.ID, depth+1)
			open := it.Status == "pending" || it.Status == "in_progress"
			if openOnly && !open && len(kids) == 0 {
				continue
			}
			out = append(out, fmt.Sprintf("%s- %s %s. %s", strings.Repeat("  ", depth), marks[it.Status], it.ID, it.Content))
			out = append(out, kids...)
		}
		return out
	}
	return strings.Join(lines("", 0), "\n")
}

// isLoop reports whether following parents from item i comes back to it.
func isLoop(items []Item, i int) bool {
	for p, steps := items[i].Parent, 0; p != "" && steps <= len(items); steps++ {
		if p == items[i].ID {
			return true
		}
		j := index(items, p)
		if j < 0 {
			return false
		}
		p = items[j].Parent
	}
	return false
}
