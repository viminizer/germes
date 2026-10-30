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
	// ContextWindow is the model's context size in tokens; history is
	// compacted at half of it. 0 = 200k.
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
	// (memory and skills). Empty: no review.
	ReviewToolsets []string
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
	steps       map[string]int             // session id -> tool calls since memory/skills were last saved
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
		frozen: map[string]string{}, seenFolders: map[string]map[string]bool{}, steps: map[string]int{}}
}

// Name returns the engine name.
func (l *Loop) Name() string { return l.opts.Name }

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
	reviewDue := false
	defer func() {
		if reviewDue {
			l.review(t.SessionID, t.Dir) // after the turn, in the background
		}
	}()
	for step := 0; step <= spec.maxSteps; step++ {
		if tokens(system, msgs) > l.opts.ContextWindow/2 {
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
		var texts []string
		for ch := range chunks {
			switch ch.Kind {
			case llm.KindTextDelta:
				stream.Send(ctx, out, engine.Event{Kind: engine.KindTextDelta, Text: ch.Text})
			case llm.KindText:
				texts = append(texts, ch.Text)
				stream.Send(ctx, out, engine.Event{Kind: engine.KindText, Text: ch.Text})
			case llm.KindToolCall:
				calls = append(calls, *ch.ToolCall)
			case llm.KindRaw:
				raws = append(raws, llm.Message{Role: llm.RoleRaw, Raw: ch.Raw})
			case llm.KindUsage:
				u := ch.Usage
				stream.Send(ctx, out, engine.Event{Kind: engine.KindUsage, Usage: &engine.Usage{Engine: l.opts.Name, Window: u.Window, UsedPercent: u.UsedPercent, ResetAt: u.ResetAt}})
			case llm.KindLimitHit:
				stream.Send(ctx, out, engine.Event{Kind: engine.KindLimitHit, ResetAt: ch.ResetAt, Err: ch.Err})
				return
			case llm.KindError:
				l.fail(ctx, out, ch.Err)
				return
			}
		}
		if ctx.Err() != nil {
			return // stopped (Esc)
		}
		// Reasoning first, then the assistant's text and calls, then results.
		msgs = append(msgs, raws...)
		for _, text := range texts {
			msgs = append(msgs, llm.Message{Role: llm.RoleAssistant, Text: text})
		}
		if len(calls) == 0 {
			if nudge := g.finishNudge(); nudge != "" && step < spec.maxSteps {
				msgs = append(msgs, llm.Message{Role: llm.RoleUser, Text: nudge})
				continue
			}
			stream.Send(ctx, out, engine.Event{Kind: engine.KindDone, Engine: l.opts.Name})
			return
		}
		if spec.review && l.countSteps(t.SessionID, calls) {
			reviewDue = true
		}
		msgs = append(msgs, llm.Message{Role: llm.RoleAssistant, ToolCalls: calls})
		asked := "" // the questions for the user, when the model called clarify
		for _, call := range calls {
			// The session saves the call before it runs.
			stream.Send(ctx, out, engine.Event{Kind: engine.KindToolCall, Tool: &engine.ToolCall{ID: call.ID, Name: call.Name, Args: call.Args}})
			res := tool.Result{IsError: true, Output: g.refuse(call)}
			switch {
			case res.Output != "":
			case spec.delegate && call.Name == delegateTool:
				res = l.delegate(ctx, t, system, call.Args)
			case spec.ask && call.Name == clarifyTool:
				text, ok := clarify(call.Args)
				res = tool.Result{IsError: !ok, Output: text}
				if ok {
					asked = text
					res.Output = "asked the user; the answer comes in their next message"
				}
			default:
				res = l.tools.Run(ctx, call.Name, tool.Call{SessionID: t.SessionID, Dir: t.Dir, Args: call.Args})
				res.Output += g.note(call, res)
				if !res.IsError {
					res.Output += l.folderRules(t.SessionID, t.Dir, call)
				}
			}
			if ctx.Err() != nil {
				return
			}
			stream.Send(ctx, out, engine.Event{Kind: engine.KindToolResult, Tool: &engine.ToolCall{ID: call.ID, Output: res.Output, IsError: res.IsError}})
			msgs = append(msgs, llm.Message{Role: llm.RoleTool, ToolCallID: call.ID, Text: res.Output})
		}
		if asked != "" {
			// The questions end the turn: the user's next message answers.
			stream.Send(ctx, out, engine.Event{Kind: engine.KindText, Text: asked})
			stream.Send(ctx, out, engine.Event{Kind: engine.KindDone, Engine: l.opts.Name})
			return
		}
	}
	stream.Send(ctx, out, engine.Event{Kind: engine.KindDone, Engine: l.opts.Name})
}

func (l *Loop) fail(ctx context.Context, out chan<- engine.Event, err error) {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return
	}
	stream.Send(ctx, out, engine.Event{Kind: engine.KindError, Err: err})
}
