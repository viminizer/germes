package session

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/viminizer/germes/internal/api"
)

var nonWord = regexp.MustCompile(`[^a-z0-9]+`)

// savePlan saves a plan turn's reply as a Markdown file in the project's
// .germes/plans, and tells the user where.
func (w *worker) savePlan(request, plan string) {
	dir := filepath.Join(w.sess.Project, ".germes", "plans")
	title, _, _ := strings.Cut(strings.TrimSpace(request), "\n")
	words := strings.Fields(nonWord.ReplaceAllString(strings.ToLower(title), " "))
	name := strings.Join(words[:min(len(words), 6)], "-")
	if name == "" {
		name = "plan"
	}
	base := filepath.Join(dir, time.Now().Format("2006-01-02")+"-"+name)
	path := base + ".md"
	for i := 2; fileExists(path); i++ {
		path = fmt.Sprintf("%s-%d.md", base, i)
	}
	body := strings.TrimSpace(plan) + "\n"
	if !strings.HasPrefix(body, "# ") { // the plan has no title of its own
		body = fmt.Sprintf("# Plan: %s\n\n%s", title, body)
	}
	err := os.MkdirAll(dir, 0o755)
	if err == nil {
		err = os.WriteFile(path, []byte(body), 0o644)
	}
	if err != nil {
		w.emit(api.Event{Kind: api.KindError, Text: "could not save the plan: " + err.Error()})
		return
	}
	w.emit(api.Event{Kind: api.KindNotice, Text: "plan saved to " + path + "; answer \"go\" to carry it out"})
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
