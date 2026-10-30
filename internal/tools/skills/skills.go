// Package skills has the skill tools: skill_view loads a skill, skill_save
// creates or replaces one, skill_patch fixes part of one.
package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/viminizer/germes/internal/skill"
	"github.com/viminizer/germes/internal/tool"
)

// New returns the skill tools.
func New(store *skill.Store) []tool.Tool {
	return []tool.Tool{
		{
			Name: "skill_view", Toolset: "skills",
			Description: "Load a skill by name: its full instructions and the list of its extra files (read those with Read).",
			Schema:      json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"],"additionalProperties":false}`),
			Run: tool.NewRun(func(ctx context.Context, c tool.Call, a struct{ Name string }) (string, error) {
				sk, err := store.Load(ctx, a.Name)
				if err != nil {
					return "", err
				}
				out := fmt.Sprintf("# Skill %s\nFolder: %s\n\n%s", sk.Name, sk.Dir, sk.Body)
				if len(sk.Files) > 0 {
					out += "\n\nExtra files:\n- " + strings.Join(sk.Files, "\n- ")
				}
				return out, nil
			}),
		},
		{
			Name: "skill_save", Toolset: "skills",
			Description: "Create or update a skill: a reusable how-to for a kind of task, saved for future sessions. " +
				"name: lowercase-with-dashes. description: one line saying when to use it. body: clear numbered steps, with exact commands.",
			Schema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"description":{"type":"string"},"body":{"type":"string"}},` +
				`"required":["name","description","body"],"additionalProperties":false}`),
			Run: tool.NewRun(func(ctx context.Context, c tool.Call, a struct{ Name, Description, Body string }) (string, error) {
				if err := store.Save(ctx, skill.Skill{Summary: skill.Summary{Name: a.Name, Description: a.Description}, Body: a.Body}); err != nil {
					return "", err
				}
				return "saved skill " + a.Name, nil
			}),
		},
		{
			Name: "skill_patch", Toolset: "skills",
			Description: "Fix part of a skill that turned out wrong or incomplete: old_text (quoted exactly, found once in its SKILL.md) becomes new_text. " +
				"Use it instead of saving the whole skill again.",
			Schema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"old_text":{"type":"string"},"new_text":{"type":"string"}},` +
				`"required":["name","old_text","new_text"],"additionalProperties":false}`),
			Run: tool.NewRun(func(ctx context.Context, c tool.Call, a struct {
				Name    string `json:"name"`
				OldText string `json:"old_text"`
				NewText string `json:"new_text"`
			}) (string, error) {
				if err := store.Update(ctx, a.Name, a.OldText, a.NewText); err != nil {
					return "", err
				}
				return "patched skill " + a.Name, nil
			}),
		},
	}
}
