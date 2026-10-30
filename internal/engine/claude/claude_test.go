package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/viminizer/germes/internal/engine"
)

// fakeClaude puts a claude script on PATH. It logs each run's arguments and
// prints turn (JSON lines); a review (--fork-session) or a run given the
// grant folder prints a plain success. It returns the log file.
func fakeClaude(t *testing.T, grant string, turn ...string) string {
	dir := t.TempDir()
	logFile, turnFile := filepath.Join(dir, "log"), filepath.Join(dir, "turn")
	os.WriteFile(turnFile, []byte(strings.Join(turn, "\n")+"\n"), 0o600)
	script := `#!/bin/sh
echo "$*" >> "` + logFile + `"
case "$*" in
*--fork-session*|*"--add-dir ` + grant + `"*) echo '{"type":"result","subtype":"success","session_id":"s1","result":"ok"}' ;;
*) cat "` + turnFile + `" ;;
esac
`
	os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o700)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return logFile
}

// runTurn runs one turn and returns its last event and claude's runs.
func runTurn(t *testing.T, c *Claude, work, logFile string) (engine.Event, []string) {
	ch, err := c.Run(context.Background(), engine.Turn{SessionID: "a", Dir: work, Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	var last engine.Event
	for ev := range ch {
		last = ev
	}
	if c.opts.Reviews != nil {
		c.opts.Reviews.Wait()
	}
	b, _ := os.ReadFile(logFile)
	return last, strings.Split(strings.TrimSpace(string(b)), "\n")
}

func toolUse(name, args string) string {
	return `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t","name":"` + name + `","input":` + args + `}]}}`
}

func TestRunResumesWithRefusedFolders(t *testing.T) {
	home, work, other := t.TempDir(), t.TempDir(), t.TempDir()
	denials, _ := json.Marshal([]map[string]any{
		{"tool_input": map[string]string{"file_path": filepath.Join(other, "a.txt")}}, // another folder: granted
		{"tool_input": map[string]string{"file_path": filepath.Join(work, ".env")}},   // the session's folder: refused by a rule
		{"tool_input": map[string]string{"file_path": filepath.Join(home, "x.yaml")}}, // Germes' home: already allowed
		{"tool_input": map[string]string{"file_path": "relative.txt"}},                // relative: in the session's folder
	})
	logFile := fakeClaude(t, other, `{"type":"result","subtype":"success","session_id":"s1","result":"ok","permission_denials":`+string(denials)+`}`)
	last, runs := runTurn(t, New(Options{GermesHome: home}), work, logFile)
	if last.Kind != engine.KindDone || len(runs) != 2 {
		t.Fatalf("last %+v, runs %q", last, runs)
	}
	if !strings.Contains(runs[1], "--resume s1") || strings.Count(runs[1], "--add-dir") != 2 { // home and other
		t.Fatalf("second run %q", runs[1])
	}
}

func TestRunReviewsAfterTenToolCalls(t *testing.T) {
	home := t.TempDir()
	turn := []string{}
	for range 10 {
		turn = append(turn, toolUse("Bash", `{"command":"ls"}`))
	}
	logFile := fakeClaude(t, "/no-grant", append(turn, `{"type":"result","subtype":"success","session_id":"s1","result":"ok"}`)...)
	_, runs := runTurn(t, New(Options{GermesHome: home, Reviews: engine.NewReviews(context.Background(), "", nil)}), t.TempDir(), logFile)
	if len(runs) != 2 || !strings.Contains(runs[1], "--resume s1 --fork-session") {
		t.Fatalf("runs %q", runs)
	}
}

func TestRunSkipsReviewAfterSavingMemory(t *testing.T) {
	home := t.TempDir()
	turn := []string{}
	for range 10 {
		turn = append(turn, toolUse("Bash", `{"command":"ls"}`))
	}
	turn = append(turn, toolUse("Edit", `{"file_path":"`+filepath.Join(home, "memories", "MEMORY.md")+`"}`))
	logFile := fakeClaude(t, "/no-grant", append(turn, `{"type":"result","subtype":"success","session_id":"s1","result":"ok"}`)...)
	_, runs := runTurn(t, New(Options{GermesHome: home, Reviews: engine.NewReviews(context.Background(), "", nil)}), t.TempDir(), logFile)
	if len(runs) != 1 {
		t.Fatalf("runs %q", runs)
	}
}

// Every tool claude runs goes through `germes hook check` for this session
// (issue #42), with a timeout a run: hook fits in.
func TestArgsSendToolsThroughGermesCheck(t *testing.T) {
	c := New(Options{Germes: "/opt/my germes", GermesHome: "/home/k/.germes"})
	args := c.args(engine.Turn{SessionID: "s1"}, "", nil)
	i := slices.Index(args, "--settings")
	if i < 0 {
		t.Fatalf("no --settings in %q", args)
	}
	var s struct {
		Hooks struct {
			PreToolUse []struct {
				Matcher string
				Hooks   []struct {
					Type, Command string
					Timeout       int
				}
			}
		}
	}
	if err := json.Unmarshal([]byte(args[i+1]), &s); err != nil || len(s.Hooks.PreToolUse) != 1 {
		t.Fatalf("settings %s: %v", args[i+1], err)
	}
	h := s.Hooks.PreToolUse[0]
	want := `GERMES_HOME='/home/k/.germes' '/opt/my germes' hook check --session 's1'`
	if h.Matcher != "*" || h.Hooks[0].Type != "command" || h.Hooks[0].Command != want || h.Hooks[0].Timeout < 600 {
		t.Fatalf("hook %+v\nwant command %s", h, want)
	}
}
