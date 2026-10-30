// Package memory has the memory tool: add, replace or remove entries in
// MEMORY.md (agent notes) and USER.md (about the user).
package memory

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/viminizer/germes/internal/memory"
	"github.com/viminizer/germes/internal/tool"
)

// New returns the memory tool.
func New(store *memory.Store) []tool.Tool {
	return []tool.Tool{{
		Name: "memory", Toolset: "memory",
		Description: "Save durable facts for future sessions. target=agent: your own notes (MEMORY.md, about the environment, tools, conventions). target=user: about the user (USER.md, preferences, how they work). " +
			"action=add saves text; replace swaps the entry containing old_text for text; remove deletes the entry containing old_text. Keep entries short; the files have small limits.",
		Schema: json.RawMessage(`{"type":"object","properties":{` +
			`"action":{"type":"string","enum":["add","replace","remove"]},` +
			`"target":{"type":"string","enum":["agent","user"]},` +
			`"text":{"type":"string"},"old_text":{"type":"string"}},` +
			`"required":["action","target"],"additionalProperties":false}`),
		Run: tool.NewRun(func(ctx context.Context, c tool.Call, a struct {
			Action  string `json:"action"`
			Target  string `json:"target"`
			Text    string `json:"text"`
			OldText string `json:"old_text"`
		}) (string, error) {
			t := memory.Target(a.Target)
			var err error
			switch a.Action {
			case "add":
				err = store.CreateEntry(ctx, t, a.Text)
			case "replace":
				err = store.UpdateEntry(ctx, t, a.OldText, a.Text)
			case "remove":
				err = store.DeleteEntry(ctx, t, a.OldText)
			default:
				return "", fmt.Errorf("unknown action %q", a.Action)
			}
			if err != nil {
				return "", err
			}
			snap, err := store.Load(ctx)
			if err != nil {
				return "", err
			}
			return "saved (it shows in the prompt from the next session); " + snap.GetUsage(t), nil
		}),
	}}
}
