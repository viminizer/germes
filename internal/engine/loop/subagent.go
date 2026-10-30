package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/viminizer/germes/internal/engine"
	"github.com/viminizer/germes/internal/llm"
	"github.com/viminizer/germes/internal/tool"
)

const (
	delegateTool  = "delegate_task"
	maxSubagents  = 4                // tasks per delegate_task call; they run at the same time
	subagentSteps = 40               // model calls a subagent may make
	subagentTime  = 15 * time.Minute // a subagent's time limit
)

// delegateSpec is the delegate_task tool. It lives in the loop, not the
// registry: a subagent is a loop turn of its own.
var delegateSpec = llm.ToolSpec{
	Name: delegateTool,
	Description: fmt.Sprintf("Run up to %d subagents at the same time, each on one task, in this folder, with fresh context and the file and shell tools. "+
		"Use it for independent research or work that can run in parallel (look into several areas of the code, try several fixes). "+
		"Each subagent knows nothing about this conversation: give it a specific goal and, in context, every fact it needs (paths, errors, constraints). "+
		"Subagents share the folder: give subagents that change files different files. They cannot start subagents. "+
		"Returns each subagent's report; check what matters before you rely on it.", maxSubagents),
	Schema: json.RawMessage(`{"type":"object","properties":{"tasks":{"type":"array","items":{"type":"object","properties":{` +
		`"goal":{"type":"string","description":"what this subagent must do or find; specific and self-contained"},` +
		`"context":{"type":"string","description":"background it needs: paths, errors, constraints"}},` +
		`"required":["goal"]}}},"required":["tasks"],"additionalProperties":false}`),
}

type task struct {
	Goal    string `json:"goal"`
	Context string `json:"context"`
}

// delegate runs the tasks as subagents at the same time and returns their
// reports, in order. Stopping the turn stops them.
func (l *Loop) delegate(ctx context.Context, t engine.Turn, system string, args json.RawMessage) tool.Result {
	var a struct {
		Tasks []task `json:"tasks"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return tool.Result{IsError: true, Output: "bad arguments: " + err.Error()}
	}
	switch {
	case len(a.Tasks) == 0:
		return tool.Result{IsError: true, Output: "give at least one task"}
	case len(a.Tasks) > maxSubagents:
		return tool.Result{IsError: true, Output: fmt.Sprintf("at most %d tasks at once; split them over several calls", maxSubagents)}
	}
	for i, tk := range a.Tasks {
		if strings.TrimSpace(tk.Goal) == "" {
			return tool.Result{IsError: true, Output: fmt.Sprintf("task %d has no goal", i+1)}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, subagentTime)
	defer cancel()
	reports := make([]string, len(a.Tasks))
	var wg sync.WaitGroup
	for i, tk := range a.Tasks {
		wg.Go(func() { reports[i] = l.runSubagent(ctx, t, system, i, tk) })
	}
	wg.Wait()
	var b strings.Builder
	for i, tk := range a.Tasks {
		fmt.Fprintf(&b, "## Subagent %d: %s\n\n%s\n\n", i+1, tk.Goal, reports[i])
	}
	return tool.Result{Output: strings.TrimSpace(b.String())}
}

// runSubagent runs one task as a fresh turn and returns its final report.
// Its own steps are not shown in the session: only the report comes back.
func (l *Loop) runSubagent(ctx context.Context, parent engine.Turn, system string, i int, tk task) (report string) {
	defer func() {
		if p := recover(); p != nil {
			slog.Error("subagent panicked", "session", parent.SessionID, "panic", p, "stack", string(debug.Stack()))
			report = fmt.Sprintf("(failed: internal error: %v)", p)
		}
	}()
	t := engine.Turn{SessionID: fmt.Sprintf("%s.sub%d", parent.SessionID, i+1), Dir: parent.Dir, Project: parent.Project, Branch: parent.Branch}
	brief := "[Germes] You are a subagent of another agent, working on one task in this folder. You start with no history: " +
		"everything you know is here.\n\nGoal: " + tk.Goal
	if strings.TrimSpace(tk.Context) != "" {
		brief += "\n\nContext: " + tk.Context
	}
	brief += "\n\nWork it through with your tools. You cannot ask questions: make reasonable assumptions and say which. " +
		"End with one self-contained report: what you found or did, with exact file paths, names and facts. The other agent sees only that report."
	out := make(chan engine.Event, 64)
	go func() {
		defer close(out)
		l.turn(ctx, t, system, []llm.Message{{Role: llm.RoleUser, Text: brief}}, out,
			turnSpec{toolsets: l.opts.SubagentToolsets, maxSteps: subagentSteps})
	}()
	var last, failure string
	for ev := range out {
		switch ev.Kind {
		case engine.KindText:
			last = ev.Text
		case engine.KindError, engine.KindLimitHit:
			if ev.Err != nil {
				failure = ev.Err.Error()
			} else {
				failure = "usage limit hit"
			}
		}
	}
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		return fmt.Sprintf("(stopped: over the %s limit)\n%s", subagentTime, last)
	case failure != "":
		return "(failed: " + failure + ")\n" + last
	case last == "":
		return "(no report)"
	}
	return last
}
