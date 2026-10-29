// Package loop is Germes' own agent loop: call the model, run the tools it
// asks for, repeat until it answers without tools or the budget runs out.
package loop

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
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
	MaxSteps int      // model calls per turn before the wrap-up call; 0 = 50
}

// Loop is the engine.
type Loop struct {
	client  llm.Client
	tools   *tool.Registry
	prompts *prompt.Builder
	history History
	opts    Options

	mu     sync.Mutex
	frozen map[string]string // session id -> system prompt, frozen for the cache
}

var _ engine.Engine = (*Loop)(nil)

// New creates the loop engine.
func New(client llm.Client, tools *tool.Registry, prompts *prompt.Builder, history History, opts Options) *Loop {
	if opts.MaxSteps <= 0 {
		opts.MaxSteps = 50
	}
	return &Loop{client: client, tools: tools, prompts: prompts, history: history, opts: opts, frozen: map[string]string{}}
}

// Name returns the engine name.
func (l *Loop) Name() string { return l.opts.Name }

// Run runs one turn. The user's message is already in the session history.
func (l *Loop) Run(ctx context.Context, t engine.Turn) (<-chan engine.Event, error) {
	msgs, err := l.load(ctx, t.SessionID)
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
				l.send(ctx, out, engine.Event{Kind: engine.KindError, Err: fmt.Errorf("internal error: %v", p)})
			}
		}()
		l.turn(ctx, t, system, msgs, out)
	}()
	return out, nil
}

// system returns the session's prompt, built once and then frozen.
func (l *Loop) system(ctx context.Context, t engine.Turn) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if s, ok := l.frozen[t.SessionID]; ok {
		return s, nil
	}
	p, err := l.prompts.Create(ctx, t.Dir, prompt.RoleProject)
	if err != nil {
		return "", err
	}
	s := p.System
	if t.Extra != "" {
		s += "\n\n" + t.Extra
	}
	l.frozen[t.SessionID] = s
	return s, nil
}

func (l *Loop) turn(ctx context.Context, t engine.Turn, system string, msgs []llm.Message, out chan<- engine.Event) {
	var specs []llm.ToolSpec
	for _, tl := range l.tools.List(l.opts.Toolsets...) {
		specs = append(specs, llm.ToolSpec{Name: tl.Name, Description: tl.Description, Schema: tl.Schema})
	}
	for step := 0; step <= l.opts.MaxSteps; step++ {
		req := llm.Request{System: system, Messages: msgs, Tools: specs, CacheKey: t.SessionID}
		if step == l.opts.MaxSteps {
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
				l.send(ctx, out, engine.Event{Kind: engine.KindTextDelta, Text: ch.Text})
			case llm.KindText:
				texts = append(texts, ch.Text)
				l.send(ctx, out, engine.Event{Kind: engine.KindText, Text: ch.Text})
			case llm.KindToolCall:
				calls = append(calls, *ch.ToolCall)
			case llm.KindRaw:
				raws = append(raws, llm.Message{Role: llm.RoleRaw, Raw: ch.Raw})
			case llm.KindUsage:
				u := ch.Usage
				l.send(ctx, out, engine.Event{Kind: engine.KindUsage, Usage: &engine.Usage{Engine: l.opts.Name, Window: u.Window, UsedPercent: u.UsedPercent, ResetAt: u.ResetAt}})
			case llm.KindLimitHit:
				l.send(ctx, out, engine.Event{Kind: engine.KindLimitHit, ResetAt: ch.ResetAt, Err: ch.Err})
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
			l.send(ctx, out, engine.Event{Kind: engine.KindDone, Engine: l.opts.Name})
			return
		}
		msgs = append(msgs, llm.Message{Role: llm.RoleAssistant, ToolCalls: calls})
		for _, call := range calls {
			// The session saves the call before it runs.
			l.send(ctx, out, engine.Event{Kind: engine.KindToolCall, Tool: &engine.ToolCall{ID: call.ID, Name: call.Name, Args: call.Args}})
			res := l.tools.Run(ctx, call.Name, tool.Call{SessionID: t.SessionID, Dir: t.Dir, Args: call.Args})
			if ctx.Err() != nil {
				return
			}
			l.send(ctx, out, engine.Event{Kind: engine.KindToolResult, Tool: &engine.ToolCall{ID: call.ID, Output: res.Output, IsError: res.IsError}})
			msgs = append(msgs, llm.Message{Role: llm.RoleTool, ToolCallID: call.ID, Text: res.Output})
		}
	}
	l.send(ctx, out, engine.Event{Kind: engine.KindDone, Engine: l.opts.Name})
}

func (l *Loop) fail(ctx context.Context, out chan<- engine.Event, err error) {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return
	}
	l.send(ctx, out, engine.Event{Kind: engine.KindError, Err: err})
}

func (l *Loop) send(ctx context.Context, out chan<- engine.Event, ev engine.Event) {
	select {
	case out <- ev:
	case <-ctx.Done():
	}
}
