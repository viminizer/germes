// Package loop is Germes' own agent loop: call the model, run the tools it
// asks for, repeat until it answers without tools or the budget runs out.
package loop

import (
	"context"
	"errors"
	"fmt"
	"github.com/viminizer/germes/internal/stream"
	"log/slog"
	"runtime/debug"
	"slices"
	"strings"
	"sync"

	"github.com/viminizer/germes/internal/api"
	"github.com/viminizer/germes/internal/engine"
	"github.com/viminizer/germes/internal/llm"
	"github.com/viminizer/germes/internal/prompt"
	"github.com/viminizer/germes/internal/tool"
)

// History reads a session's stored events.
type History interface {
	ListEvents(ctx context.Context, f api.EventFilter) (api.Page[api.Event], error)
}

// Options configure the loop.
type Options struct {
	Name     string   // engine name, e.g. "codex-api"
	Toolsets []string // tools the model may use
	// OverseerToolsets are the overseer's tools (it also controls Germes).
	OverseerToolsets []string
	MaxSteps         int // model calls per turn before the wrap-up call; 0 = 50
	// ContextWindow is the model's context size in tokens when the client
	// does not know it (WindowClient); history is compacted at half of it.
	// 0 = 200k.
	ContextWindow int
	// Todos gives the model its open task items again after a compaction;
	// nil = none.
	Todos TaskLists
	// PlanToolsets are the tools of a plan turn: reading only.
	PlanToolsets []string
	// SubagentToolsets are the tools of the subagents delegate_task starts.
	// Empty: no delegate_task.
	SubagentToolsets []string
	// ReviewToolsets are the tools of the background self-learning review
	// (memory and skills), run by Reviews. Empty or no Reviews: no review.
	ReviewToolsets []string
	Reviews        *engine.Reviews
}

// TaskLists reads a session's open task items (the todo_list tool).
type TaskLists interface {
	GetActive(sessionID string) string
}

// Loop is the engine.
type Loop struct {
	client  llm.Client
	tools   *tool.Registry
	prompts *prompt.Builder
	history History
	opts    Options

	mu          sync.Mutex
	frozen      map[string]string          // session id -> system prompt, frozen for the cache
	seenFolders map[string]map[string]bool // session id -> subfolders whose instructions were given
}

var _ engine.Engine = (*Loop)(nil)

// New creates the loop engine.
func New(client llm.Client, tools *tool.Registry, prompts *prompt.Builder, history History, opts Options) *Loop {
	if opts.MaxSteps <= 0 {
		opts.MaxSteps = 50
	}
	if opts.ContextWindow <= 0 {
		opts.ContextWindow = 200_000
	}
	return &Loop{client: client, tools: tools, prompts: prompts, history: history, opts: opts,
		frozen: map[string]string{}, seenFolders: map[string]map[string]bool{}}
}

// Name returns the engine name.
func (l *Loop) Name() string { return l.opts.Name }

// WindowClient is a client that knows its model's context window, like
// Codex from its model list (Hermes: agent/model_metadata.py).
type WindowClient interface {
	GetContextWindow(ctx context.Context) (int, error)
}

// window is the model's context window in tokens: the client's when it
// knows it, else ContextWindow.
func (l *Loop) window(ctx context.Context) int {
	if c, ok := l.client.(WindowClient); ok {
		if n, err := c.GetContextWindow(ctx); err == nil && n > 0 {
			return n
		}
	}
	return l.opts.ContextWindow
}

// Run runs one turn. The user's message is already in the session history.
func (l *Loop) Run(ctx context.Context, t engine.Turn) (<-chan engine.Event, error) {
	events, err := l.loadEvents(ctx, t.SessionID)
	if err != nil {
		return nil, err
	}
	system, err := l.system(ctx, t)
	if err != nil {
		return nil, err
	}
	out := make(chan engine.Event, 64)
	go func() {
		defer close(out)
		defer func() {
			if p := recover(); p != nil {
				slog.Error("loop panicked", "session", t.SessionID, "panic", p, "stack", string(debug.Stack()))
				stream.Send(ctx, out, engine.Event{Kind: engine.KindError, Err: fmt.Errorf("internal error: %v", p)})
			}
		}()
		summary, keepFrom, ok, err := l.compact(ctx, system, events)
		if err != nil {
			// Not fatal: the turn still runs; the model may reject a too-long input.
			slog.Warn("compaction failed", "session", t.SessionID, "err", err)
		}
		if ok {
			// Saved by the session before anything else of this turn, so the
			// next turn loads the summary plus the kept events.
			stream.Send(ctx, out, engine.Event{Kind: engine.KindCompacted, Text: summary, KeepFrom: keepFrom})
			events = append(events, api.Event{Kind: api.KindCompacted, Text: summary, KeepFrom: keepFrom})
		}
		msgs := messages(events)
		if summary, _ := effective(events); summary != "" && l.opts.Todos != nil {
			// The summary may blur the plan: the open items go right after it.
			if plan := l.opts.Todos.GetActive(t.SessionID); plan != "" {
				msgs[0].Text += "\n\n" + plan
			}
		}
		spec := turnSpec{toolsets: l.opts.Toolsets, maxSteps: l.opts.MaxSteps,
			review: true, delegate: len(l.opts.SubagentToolsets) > 0, ask: true}
		switch {
		case t.Plan:
			// Read only, and no subagents: they could change files.
			spec.toolsets, spec.delegate = l.opts.PlanToolsets, false
			msgs = append(msgs, llm.Message{Role: llm.RoleUser, Text: strings.TrimSpace(prompt.PlanMode)})
		case t.Overseer:
			spec.toolsets = l.opts.OverseerToolsets
		}
		l.turn(ctx, t, system, msgs, out, spec)
	}()
	return out, nil
}

// system returns the session's prompt, built once and then frozen.
func (l *Loop) system(ctx context.Context, t engine.Turn) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// Frozen per session and folder: leaving a worktree builds a new prompt.
	key := t.SessionID + "\x00" + t.Dir
	if s, ok := l.frozen[key]; ok {
		return s, nil
	}
	p, err := l.prompts.Create(ctx, prompt.Where{Dir: t.Dir, Project: t.Project, Branch: t.Branch}, prompt.NewRole(t.Overseer))
	if err != nil {
		return "", err
	}
	l.frozen[key] = p.System
	return p.System, nil
}

// turnSpec says what a turn may do: a session's turn, or a subagent's.
type turnSpec struct {
	toolsets []string
	maxSteps int
	review   bool // count tool calls toward the background review
	delegate bool // may start subagents
	ask      bool // may ask the user (clarify); a subagent has nobody to ask
}

func (l *Loop) turn(ctx context.Context, t engine.Turn, system string, msgs []llm.Message, out chan<- engine.Event, spec turnSpec) {
	window := l.window(ctx)
	var specs []llm.ToolSpec
	for _, tl := range l.tools.List(spec.toolsets...) {
		specs = append(specs, llm.ToolSpec{Name: tl.Name, Description: tl.Description, Schema: tl.Schema})
	}
	if spec.delegate {
		specs = append(specs, delegateSpec)
	}
	if spec.ask {
		specs = append(specs, clarifySpec)
	}
	if t.Extra != "" {
		// A handoff goes in as a message: the system prompt stays frozen, so
		// the provider's prompt cache keeps working.
		msgs = append(msgs, llm.Message{Role: llm.RoleUser, Text: "[Germes] " + t.Extra})
	}
	g := newGuard(t.Dir)
	g.readRequest(t.Prompt)
	askedWhy := false        // the model sent a final answer with tool calls once
	var used *engine.Context // how full the context was for the last response
	done := func() {
		stream.Send(ctx, out, engine.Event{Kind: engine.KindDone, Engine: l.opts.Name, Context: used})
	}
	defer func() {
		if spec.review && l.reviews() && l.opts.Reviews.IsDue(t.SessionID) {
			l.opts.Reviews.Run(t.SessionID, func(ctx context.Context) { l.review(ctx, t) })
		}
	}()
	for step := 0; step <= spec.maxSteps; step++ {
		if tokens(system, msgs) > window/2 {
			prune(msgs, 10) // a long turn: drop old tool outputs, keep the newest
		}
		req := llm.Request{System: system, Messages: msgs, Tools: specs, CacheKey: t.SessionID}
		if step == spec.maxSteps {
			// Grace call: no tools, just wrap up.
			req.Tools = nil
			req.Messages = append(append([]llm.Message(nil), msgs...), llm.Message{Role: llm.RoleUser,
				Text: "You used your step budget for this turn. Stop calling tools. Say what you did and what is left."})
		}
		chunks, err := l.client.Send(ctx, req)
		if err != nil {
			l.fail(ctx, out, err)
			return
		}
		var raws []llm.Message
		var calls []llm.ToolCall
		var texts []llm.Message
		for ch := range chunks {
			switch ch.Kind {
			case llm.KindTextDelta:
				stream.Send(ctx, out, engine.Event{Kind: engine.KindTextDelta, Text: ch.Text})
			case llm.KindText:
				texts = append(texts, llm.Message{Role: llm.RoleAssistant, Text: ch.Text, Phase: ch.Phase})
				stream.Send(ctx, out, engine.Event{Kind: engine.KindText, Text: ch.Text})
			case llm.KindToolCall:
				calls = append(calls, *ch.ToolCall)
			case llm.KindRaw:
				raws = append(raws, llm.Message{Role: llm.RoleRaw, Raw: ch.Raw})
			case llm.KindUsage:
				u := ch.Usage
				stream.Send(ctx, out, engine.Event{Kind: engine.KindUsage, Usage: &engine.Usage{Engine: l.opts.Name, Window: u.Window, UsedPercent: u.UsedPercent, ResetAt: u.ResetAt}})
			case llm.KindLimitHit:
				stream.Send(ctx, out, engine.Event{Kind: engine.KindLimitHit, ResetAt: ch.ResetAt, Err: ch.Err, Outage: ch.Outage})
				return
			case llm.KindError:
				l.fail(ctx, out, ch.Err)
				return
			case llm.KindStatus:
				stream.Send(ctx, out, engine.Event{Kind: engine.KindStatus, Text: ch.Text})
			case llm.KindReasoning:
				stream.Send(ctx, out, engine.Event{Kind: engine.KindReasoning, Text: ch.Text})
			case llm.KindContext:
				used = &engine.Context{Model: ch.Context.Model, Used: ch.Context.Tokens, Window: window}
			}
		}
		if ctx.Err() != nil {
			return // stopped (Esc)
		}
		// Reasoning first, then the assistant's text and calls, then results.
		msgs = append(msgs, raws...)
		msgs = append(msgs, texts...)
		if len(calls) > 0 && isFinal(texts) {
			// A final answer ends the turn: calls sent with it do not run.
			// Once a turn the model may say why it is not done and go on.
			if !askedWhy && step < spec.maxSteps {
				askedWhy = true
				msgs = append(msgs, llm.Message{Role: llm.RoleUser, Text: finalWithCalls})
				continue
			}
			slog.Warn("final answer came with tool calls; not running them", "session", t.SessionID, "calls", len(calls))
			calls = nil
		}
		if len(calls) == 0 {
			if nudge := g.finishNudge(); nudge != "" && step < spec.maxSteps {
				msgs = append(msgs, llm.Message{Role: llm.RoleUser, Text: nudge})
				continue
			}
			done()
			return
		}
		if spec.review && l.reviews() {
			for _, c := range calls {
				l.opts.Reviews.Update(t.SessionID, c.Name == "memory" || c.Name == "skill_save" || c.Name == "skill_patch")
			}
		}
		msgs = append(msgs, llm.Message{Role: llm.RoleAssistant, ToolCalls: calls})
		asked := "" // the questions for the user, when the model called clarify
		// Every call of the step is saved before any of them runs (issue #25,
		// like Hermes's agent/turn_tool_round.py): a tool never changes
		// anything without a record of it.
		for _, call := range calls {
			if !sendSaved(ctx, out, t, engine.Event{Kind: engine.KindToolCall, Tool: &engine.ToolCall{ID: call.ID, Name: call.Name, Args: call.Args}}) {
				return
			}
		}
		// Reads in a row run at once (issue #38); any other call runs alone,
		// in order, and the calls after it wait for it.
		for i := 0; i < len(calls); {
			n := countReads(calls[i:])
			var results []tool.Result
			if n > 1 {
				results = l.runReads(ctx, t, g, calls[i:i+n])
			} else {
				n = 1
				res, question := l.runCall(ctx, t, system, spec, g, calls[i])
				results = []tool.Result{res}
				if question != "" {
					asked = question
				}
			}
			if ctx.Err() != nil {
				return
			}
			for k, res := range results {
				call := calls[i+k]
				// Saved, in the order of the calls, before the next tool or
				// model call; a tool that ran is never run again for it.
				if !sendSaved(ctx, out, t, engine.Event{Kind: engine.KindToolResult, Tool: &engine.ToolCall{ID: call.ID, Output: res.Output, IsError: res.IsError}}) {
					return
				}
				msgs = append(msgs, llm.Message{Role: llm.RoleTool, ToolCallID: call.ID, Text: res.Output})
			}
			i += n
		}
		if asked != "" {
			// The questions end the turn: the user's next message answers.
			stream.Send(ctx, out, engine.Event{Kind: engine.KindText, Text: asked})
			done()
			return
		}
	}
	done()
}

func (l *Loop) fail(ctx context.Context, out chan<- engine.Event, err error) {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return
	}
	stream.Send(ctx, out, engine.Event{Kind: engine.KindError, Err: err})
}

// finalWithCalls goes back to a model that gave its final answer and asked
// for tools in the same step.
const finalWithCalls = "[Germes] You gave your final answer and also asked for tools, which did not run. " +
	"If the task is done, stop now. If it is not, say in one sentence why more work is needed, then call the tools again."

// isFinal reports whether one of a step's texts is the model's final answer.
func isFinal(texts []llm.Message) bool {
	return slices.ContainsFunc(texts, func(m llm.Message) bool { return m.Phase == llm.PhaseFinalAnswer })
}

// sendSaved sends a tool event. When the session saves them (Turn.Saves),
// it waits for the save: false means the save failed or the turn ended, and
// the turn must stop.
// readTools only read, and one never depends on another: a row of them in
// one step runs at once, at most maxParallel at a time.
var readTools = map[string]bool{"Read": true, "Glob": true, "Grep": true, "WebSearch": true, "WebFetch": true, "session_search": true}

const maxParallel = 4

// countReads counts the read tool calls at the start of calls.
func countReads(calls []llm.ToolCall) int {
	n := 0
	for n < len(calls) && readTools[calls[n].Name] {
		n++
	}
	return n
}

// runCall runs one call. A clarify call returns the questions it asked.
func (l *Loop) runCall(ctx context.Context, t engine.Turn, system string, spec turnSpec, g *guard, call llm.ToolCall) (tool.Result, string) {
	if why := g.refuse(call); why != "" {
		return tool.Result{IsError: true, Output: why}, ""
	}
	switch {
	case spec.delegate && call.Name == delegateTool:
		return l.delegate(ctx, t, system, call.Args), ""
	case spec.ask && call.Name == clarifyTool:
		text, ok := clarify(call.Args)
		if !ok {
			return tool.Result{IsError: true, Output: text}, ""
		}
		return tool.Result{Output: "asked the user; the answer comes in their next message"}, text
	}
	res := l.tools.Run(ctx, call.Name, tool.Call{SessionID: t.SessionID, Dir: t.Dir, Args: call.Args})
	return l.finish(t, g, call, res), ""
}

// runReads runs read calls at once and returns their results in order.
// Only the tools run concurrently: the guard and the folder rules see the
// calls one by one, in order, so their notes do not depend on timing.
func (l *Loop) runReads(ctx context.Context, t engine.Turn, g *guard, calls []llm.ToolCall) []tool.Result {
	results := make([]tool.Result, len(calls))
	refused := make([]bool, len(calls))
	slots := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup
	for i, call := range calls {
		if why := g.refuse(call); why != "" {
			results[i], refused[i] = tool.Result{IsError: true, Output: why}, true
			continue
		}
		slots <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-slots; wg.Done() }()
			// Registry.Run never panics; a stop ends every call here.
			results[i] = l.tools.Run(ctx, call.Name, tool.Call{SessionID: t.SessionID, Dir: t.Dir, Args: call.Args})
		}()
	}
	wg.Wait()
	for i, call := range calls {
		if !refused[i] {
			results[i] = l.finish(t, g, call, results[i])
		}
	}
	return results
}

// finish adds the guard's note and, after success, the rules of folders the
// call touched for the first time.
func (l *Loop) finish(t engine.Turn, g *guard, call llm.ToolCall, res tool.Result) tool.Result {
	res.Output += g.note(call, res)
	if !res.IsError {
		res.Output += l.folderRules(t.SessionID, t.Dir, call)
	}
	return res
}

func sendSaved(ctx context.Context, out chan<- engine.Event, t engine.Turn, ev engine.Event) bool {
	if !t.Saves {
		return stream.Send(ctx, out, ev)
	}
	saved := make(chan error, 1)
	ev.Saved = saved
	if !stream.Send(ctx, out, ev) {
		return false
	}
	select {
	case err := <-saved:
		return err == nil
	case <-ctx.Done():
		return false
	}
}
