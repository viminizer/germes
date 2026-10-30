package todo

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/viminizer/germes/internal/tool"
)

func call(t *testing.T, lists *Lists, args any) string {
	t.Helper()
	b, _ := json.Marshal(args)
	out, err := New(lists)[0].Run(context.Background(), tool.Call{SessionID: "s1", Args: b})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestTaskList(t *testing.T) {
	dir := t.TempDir()
	lists := NewLists(dir)
	call(t, lists, map[string]any{"todos": []Item{
		{ID: "1", Content: "Find the bug", Status: "completed"},
		{ID: "2", Content: "Fix it", Status: "in_progress"},
		{ID: "2a", Content: "Write a test", Status: "pending", Parent: "2"},
		{ID: "3", Content: "Update docs", Status: "pending"},
	}})
	// An update changes one status and keeps the rest.
	out := call(t, lists, map[string]any{"todos": []Item{{ID: "3", Status: "cancelled"}}})
	want := "- [x] 1. Find the bug\n- [>] 2. Fix it\n  - [ ] 2a. Write a test\n- [-] 3. Update docs"
	if out != want {
		t.Fatalf("list:\n%s\nwant:\n%s", out, want)
	}
	if read := call(t, lists, map[string]any{}); read != want {
		t.Fatalf("a call without items should read the list:\n%s", read)
	}
	// After a restart (a new Lists on the same folder) only open work is
	// given back, with the parent of an open subtask.
	active := NewLists(dir).GetActive("s1")
	if !strings.Contains(active, "- [>] 2. Fix it\n  - [ ] 2a. Write a test") || strings.Contains(active, "Find the bug") || strings.Contains(active, "Update docs") {
		t.Fatalf("active:\n%s", active)
	}
	if NewLists(dir).GetActive("other") != "" {
		t.Fatal("another session has items")
	}
	// replace starts a new plan.
	if out := call(t, lists, map[string]any{"todos": []Item{{ID: "n", Content: "New plan", Status: "pending"}}, "replace": true}); out != "- [ ] n. New plan" {
		t.Fatalf("replaced:\n%s", out)
	}
}

func TestParentLoopsAreBroken(t *testing.T) {
	lists := NewLists(t.TempDir())
	out := call(t, lists, map[string]any{"todos": []Item{
		{ID: "a", Content: "A", Status: "pending", Parent: "b"},
		{ID: "b", Content: "B", Status: "pending", Parent: "a"},
		{ID: "c", Content: "C", Status: "pending", Parent: "nope"},
	}})
	for _, id := range []string{"a. A", "b. B", "c. C"} {
		if !strings.Contains(out, id) {
			t.Fatalf("%q hidden:\n%s", id, out)
		}
	}
}
