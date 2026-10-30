package api

import (
	"encoding/json"
	"path/filepath"
)

// GetLoadedSkill is the skill a tool call loads, or "": skill_view (the Germes
// loop), Claude's Skill tool, or a Read of a SKILL.md. Insights count these
// and skill hooks look for them (issue #42).
func GetLoadedSkill(t *ToolCall) string {
	if t == nil {
		return ""
	}
	var a struct {
		Name     string `json:"name"`
		Skill    string `json:"skill"`
		FilePath string `json:"file_path"`
	}
	if json.Unmarshal(t.Args, &a) != nil {
		return ""
	}
	switch t.Name {
	case "skill_view", "Skill":
		return a.Name + a.Skill
	case "Read":
		if filepath.Base(a.FilePath) == "SKILL.md" {
			return filepath.Base(filepath.Dir(a.FilePath))
		}
	}
	return ""
}
