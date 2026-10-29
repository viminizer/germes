# Germes plan

Status: planning and learning. No code yet.

Germes is a Go harness for building software apps from a prompt. It takes good ideas from Hermes Agent (NousResearch/hermes-agent, cloned in `hermes/`).

## Goals

- Start it in any folder, like Claude Code. That folder (its git root) is the project.
- Talk to it from a Hermes-like TUI. This is the main way to work.
- One daemon owns all sessions on the machine.
- An optional overseer can control any session. Used mainly from remote (Telegram).
- Loops that run until I or the daemon stop them.
- No API key needed at the start: use the Codex login and the Claude Code CLI.
- Switch between providers when usage limits hit, without losing context.
- All setup is optional except having at least one engine.

## Hermes ideas to copy

### Agent loop
- Iteration budget with a refund and one last grace call.
- Save the assistant message before running its tools.
- Unknown tool name becomes a helpful error that lists valid names.
- Compress at 50% of context: first drop old tool outputs (free), then summarize the middle with a cheap model. Keep first 3 and last 20 messages.
- System prompt is frozen for the session, so the prompt cache stays valid.

### Tools and toolsets
- Each tool registers itself: name, JSON schema, handler, availability check.
- A toolset is a list of tools and can include other toolsets.
- Command execution goes through one interface: `Run(ctx, cmd, cwd, timeout)`. Local first, Docker as an option.

### Memory and skills
- `MEMORY.md` (agent notes) and `USER.md` (about me). Injected into the prompt as a snapshot frozen at session start.
- Skills are folders with a `SKILL.md` (name + short description). The prompt lists them and says "if one matches, you MUST load it".
- Self-learning: after about 10 steps without saving, a background copy of the agent reviews the chat and saves memory or skills. Runs after the reply is sent.
- Session search: SQLite FTS5 over old chats. No LLM.

### Gateway
- Adapter interface: start, send, receive events.
- One normalized event type for all platforms.
- Messages wait in a queue while a session is busy. Control commands like `/stop` skip the queue.

### Subagents
- Child gets a fresh history, fewer tools, and its own budget.
- Child cannot use memory, delegate, cron, or send_message.
- Child returns only a summary. Max 3 in parallel, max depth 2.

### System prompt
- Built in three layers, stable first: identity (`SOUL.md`) + tool guidance, then the project context file (only one type, for example `AGENTS.md`), then skills list, memory and time.

## Module map

Every module, and the only Germes modules it may import (R2). The import test checks this table. Anything not listed is not allowed.

```
cmd/germes            wiring: imports everything

entry points          tui   cli   mcp   telegram          → api only (R5)

server                daemon                               → api, session, job, router

orchestration         session                              → api, engine
                      job                                  → api
                      router                               → api, engine, config

engines               engine/loop                          → engine, llm, tool, prompt, api
                      engine/claude                        → engine, prompt

tools                 tools/files, tools/shell             → tool, env
                      tools/memory                         → tool, memory
                      tools/skills                         → tool, skill
                      tools/search                         → tool, api
                      tools/sessions, tools/jobs,          → tool, api
                      tools/worktree

services              prompt                               → memory, skill
                      store                                → api, engine
                      llm                                  → (none)

leaves                api, config, engine, tool, env,      → (none)
                      memory, skill
```

Notes:
- Shared plain types live low: `api` (Session, Event, Job, Page, filters), `engine` (Turn, Event, Usage). Higher modules and `store` use them, so nothing imports upward.
- `session` does not import `router`, and `job` does not import `session`: each defines the small interface it needs (R4), and `main.go` passes the real thing in.
- `tools/sessions`, `tools/jobs` (and `mcp`) reach the daemon through `api.NewClient(socket)`. No cycle.
- `telegram` gets its settings as its own `telegram.Options`, filled by `main.go` from `cfg.Telegram`. So entry points still import only `api`.

## Projects and sessions

- Run `germes` in any folder. Germes walks up to the git root and uses it as the project key. No git: the folder itself.
- No workspace table. Each chat saves its project path. The list of projects is a query over chats.
- A chat is saved only when the first message is sent. Opening and closing Germes leaves nothing behind.
- Cleanup: hide projects whose folder is gone. Delete chats older than N days (for example 30), unless the project is pinned (`germes pin`).

## Daemon and overseer

- `germesd` (a subcommand of the same binary) runs in the background and owns all sessions.
- `germes` in a folder is a client. It always starts a new, empty chat. Resuming is explicit (see "Using Germes from the terminal").
- The overseer is a normal agent session with tools for the whole `Service` API, plus shell and files anywhere. Details in "Overseer".
- Notifications (done, failed) go to Telegram directly, not through the overseer.
- Telegram talks to the overseer by default.
- The `api.Service` interface is the single source of control. CLI commands, overseer tools and Telegram are thin wrappers over it.
- Shell is enough for now. Computer use maybe later.

### Safety
- Telegram allowlist: only my group chat ID, and only my user ID inside it.
- No approval prompts (yolo). Blocked commands are refused, see "Permissions".
- Socket is readable only by my user.

## API module

`internal/api` is the contract between the daemon and every client. It imports nothing from Germes.

### One interface, both sides

- `api.Service` is everything you can ask Germes to do.
- The daemon implements `Service` with plain Go methods. It never touches HTTP or JSON.
- `api.NewHandler(svc Service) http.Handler` is the server side. The daemon serves it on the socket.
- `api.NewClient(socketPath) Service` is the client side. Used by the TUI, CLI commands and `germes mcp`.
- Telegram, the overseer and jobs run inside the daemon and call `Service` directly.
- Tests can drive a client with a fake `Service`.

```go
type Service interface {
    // daemon
    GetDaemon(ctx) (Daemon, error)          // version, pid, started at (handshake)
    StopDaemon(ctx) error

    // sessions
    ListSessions(ctx, SessionFilter) (Page[Session], error)   // filter by project path
    GetSession(ctx, id string) (Session, error)
    UpdateSession(ctx, SessionUpdate) error   // rename
    DeleteSession(ctx, id string) error
    SendMessage(ctx, MessageRequest) (MessageResult, error)
    StopSession(ctx, id string) error        // stop the current step (Esc)

    // events
    ListEvents(ctx, EventFilter) (Page[Event], error)          // history, paged
    Watch(ctx, WatchFilter) (<-chan Event, error)  // live; filter: SessionID ("" = all), Kinds, AfterSeq

    // jobs (loops + cron)
    CreateJob(ctx, JobRequest) (Job, error)
    ListJobs(ctx, JobFilter) (Page[Job], error)
    StartJob(ctx, id string) error
    StopJob(ctx, id string) error
    DeleteJob(ctx, id string) error

    // engines
    ListEngines(ctx) ([]Engine, error)     // for example: codex-api limited until 14:00

    // tools (for germes mcp)
    ListTools(ctx, sessionID string) ([]Tool, error)
    RunTool(ctx, sessionID, name string, args json.RawMessage) (ToolResult, error)
}
```

### Design choices

- No "attach" call. `germes` always opens an empty chat. The first `SendMessage` carries the project path and no session id; the daemon creates the session then. This keeps "save nothing until the first message". Resuming: the TUI calls `ListSessions(project=<git root>)` to find the session, then `ListEvents` + `Watch`.
- Each session is an event log. Every event has a sequence number. Kinds: user_message, text, tool_call, tool_result, turn_done, engine_switched, limit_hit, error.
- History: `ListEvents` (paged). Live: `Watch(afterSeq = last seen)`, so nothing is missed. The TUI, Telegram and a second terminal all read the same log.
- `Watch` is used by: the open TUI, a TUI opened while a loop runs, two terminals on one session, the Telegram adapter, the overseer.
- Every error has one code: `not_found`, `invalid`, `unauthorized`, `version_mismatch`, `unavailable` (all engines limited).

### Types

```go
type Session struct {
    ID, Name, Project string
    Status    Status      // idle | working | paused
    Engine   string      // which engine ran last
    CreatedAt, UpdatedAt time.Time
}

type Event struct {
    SessionID string
    Seq       int64
    Time      time.Time
    Kind      EventKind
    Text      string
    Tool      *ToolCall   // set for tool_call and tool_result
}

type Job struct {
    ID, SessionID, Prompt string
    Every    time.Duration
    MaxRuns, Runs int
    Status   Status
}

type Page[T any] struct { Items []T; Next string }   // Next = "" at the end
```

### Transport

- HTTP over the unix socket `~/.germes/germes.sock`. `Watch` streams newline-delimited JSON on one long-lived connection.
- Only between separate processes on this machine: TUI, CLI commands and `germes mcp` to the daemon. No network port.
- Go standard library only (`net/http`). Debug with `curl --unix-socket ~/.germes/germes.sock localhost/sessions`.
- Outgoing HTTPS (Codex API, Telegram servers) belongs to other modules, not to `api`.
- Speed: one request over the socket takes about 0.05-0.2 ms. The model takes 10-30 ms per token. Not noticeable.
- The `Watch` handler must flush after every event, or text arrives in chunks.

### Compared with Hermes

Hermes runs one Python backend per TUI and talks newline-delimited JSON-RPC over stdio (Desktop uses WebSocket). See `hermes/tui_gateway/AGENTS.md`. Germes keeps the same ideas (event stream, replay from a sequence number) but uses one shared daemon, so a socket fits better. Both sides are Go, so the types are shared and no code generator is needed.

### Files

```
internal/api/
  service.go    the Service interface
  types.go      Session, Event, Job, Engine, Page, errors
  handler.go    NewHandler: HTTP routes -> Service
  client.go     NewClient: Service -> HTTP calls
```

## Session module

`internal/session` runs conversations: takes messages in, runs turns one at a time, records every event, streams events to watchers.

Not its job: picking a engine, limits, handoff (`router`); prompt, memory, skills (`prompt`, used by the engine); SQLite (`store`); HTTP (`api`).

```
daemon ──► session.Manager ──► router (runs turns)
                │
                └──► store (saves sessions + events)
```

The daemon builds `api.Service` mostly by forwarding to `session.Manager`. Jobs use the Manager to send their prompts.

### Public API

```go
func NewManager(store Store, runner Runner) *Manager

func (m *Manager) SendMessage(ctx, api.MessageRequest) (api.MessageResult, error)
func (m *Manager) StopSession(ctx, id string) error
func (m *Manager) Watch(ctx, api.WatchFilter) (<-chan api.Event, error)
func (m *Manager) ListSessions(ctx, api.SessionFilter) (api.Page[api.Session], error)
func (m *Manager) GetSession(ctx, id string) (api.Session, error)
func (m *Manager) UpdateSession(ctx, api.SessionUpdate) error
func (m *Manager) DeleteSession(ctx, id string) error
func (m *Manager) ListEvents(ctx, api.EventFilter) (api.Page[api.Event], error)
func (m *Manager) Stop(ctx) error   // daemon shutdown: stop all turns cleanly
```

It uses the `api` types directly. They are plain data and `api` is a leaf.

### What it needs (defined in `session`, R4)

```go
type Store interface {
    CreateSession(ctx, api.Session) error
    GetSession(ctx, id string) (api.Session, error)
    UpdateSession(ctx, api.Session) error
    DeleteSession(ctx, id string) error
    ListSessions(ctx, api.SessionFilter) (api.Page[api.Session], error)
    CreateEvent(ctx, api.Event) error
    ListEvents(ctx, api.EventFilter) (api.Page[api.Event], error)
}

type Runner interface {   // the router satisfies this
    Run(ctx, engine.Turn) (<-chan engine.Event, error)
}
```

### Names

- ID: random, 6 characters (`k3f9a2`), never changes.
- Title: from the first message, cut to about 40 characters (`add dark mode`). Later, maybe a model-written title after the first turn.
- The project is shown next to the title, not in it: `todo-app · add dark mode`.
- `germes attach <id>` takes an ID or a unique start of one. `germes --resume` shows project, title and time to pick from.
- Rename with `/rename <text>` in the TUI (`UpdateSession`).
- Jobs get their name when created: `germes job add --name nightly-tests ...`.

### Workers

One goroutine (worker) per active session, with an inbox:

```
SendMessage ─► inbox ─► worker goroutine
                          1. take next message
                          2. status = working
                          3. runner.Run(turn) → engine events
                          4. for each: convert → api.Event, seq++, store.CreateEvent, publish to watchers
                          5. turn_done → status = idle → next message
```

- Messages sent while busy wait in the inbox.
- Only the worker touches the session's state: no locks.
- Only the worker assigns sequence numbers: always in order, no gaps.
- Save first, then publish. A crash never loses an event a client already saw.
- Stop (Esc) cancels the current turn's `ctx`. The session goes back to idle.
- A worker leaves memory after 5 minutes idle. The next message loads the session from the store again.
- Daemon restart in the middle of a turn: the session saves an `error: interrupted` event and goes idle. No automatic retry (it could run a tool twice). Jobs start their next round themselves.

### Watch

```
Watch(id, afterSeq=120)
  1. subscribe to live events first
  2. read stored events with seq > 120, send them
  3. then send live events, skipping any seq already sent
```

- Subscribing before reading history means no event falls into the gap.
- Each watcher has a buffered channel. A watcher that cannot keep up gets its channel closed; the agent never slows down. The client reconnects with `Watch(afterSeq=last seen)` and catches up from the store.

### Permissions

No approval prompts (yolo). Agents run any command except blocked ones, which are refused (never asked). The blocked list lives in config and starts from my Claude Code deny list:

```yaml
blocked:
  - Bash(sudo *)
  - Bash(rm -rf /*)
  - Read(.env)
  - Read(**/.env*)
```

- `engine/loop`: the shell and file tools check the list before running. Blocked = error result to the model.
- `engine/claude`: allow all tools, pass the list as `--disallowedTools`, and `--permission-prompts none` so anything that would ask is refused. Never `--dangerously-skip-permissions` (my global rule).
- No approval events, no `waiting` status, no approval code anywhere.

### Resume ids

The Claude CLI returns its own session id (`Done{ResumeID}`), needed for `--resume`. The session stores these in its record as an opaque map (engine name → id) and passes them back in the next `Turn`. It never looks inside them.

### Files

```
internal/session/
  manager.go   Manager: active sessions, create on first message, route calls
  worker.go    one session's goroutine: inbox, turns, stop
  watch.go     watchers: subscribe, replay, drop slow ones
```

## Loops and jobs

- Loop and cron are one feature: a job. Details in "Job module".
- Jobs are saved in `germes.db` and restart after a reboot.
- Each round starts with fresh context and reads `PROGRESS.md` in the project. The agent updates it every round.
- Stops when: I stop it, the overseer stops it, the agent replies `[DONE]`, a limit is hit (runs), or rounds keep failing.

## Using Germes from the terminal

- `germes` opens a full TUI: streaming output, tool calls shown live, multiline input, history, slash-command autocomplete.
- Esc stops the current step (interrupt and redirect). `/sessions` opens a session picker. Ctrl+C or `/exit` quits; the session keeps running. Keys: see "TUI module".
- The overseer is one more session in the picker. Optional.
- `germes` always starts a new session. Resuming is explicit: `germes -c` (last session in this project), `germes --resume` (pick from a list), `germes attach <name>` (any session). `germes ls` lists sessions.
- Direct commands without an LLM: `germes stop <s>`, `germes job add <s> --every 1h "..."`, `germes job ls`, `germes job stop <s>`.
- Slash commands inside the TUI: `/sessions`, `/switch <s>`, `/overseer`.
- Built with Bubble Tea.

## Engines and providers

```go
type Engine interface {
    Name() string
    Run(ctx context.Context, t Turn) (<-chan Event, error)
}
// Event: Text, ToolCall, Done{ResumeID}, LimitHit{ResetAt}
```

Two kinds:

1. API engine: Germes runs its own agent loop and tools. Auth by token. For now: the Codex browser login token (Hermes does this, see `hermes/plugins/model-providers/openai-codex/`). Later: normal API keys.
2. CLI engine: the CLI runs its own loop. For now: Claude Code (`claude -p --output-format stream-json --resume <id> --append-system-prompt ... --mcp-config ...`). Germes tools reach it through `germes mcp`.

- Anthropic does not allow other harnesses to use Claude login tokens. Claude only ever runs through the official `claude` CLI. Germes never reads the Claude token.
- Both engines emit the same events, so the TUI, transcript, and loops do not care which one runs.

### Switching on usage limits

Default order and reserves:

```yaml
engines:
  - name: codex-api
    reserve: { 5h: 10%, weekly: 20% }   # stop using it when this much is left
  - name: claude-cli
    reserve: { five_hour: 10%, seven_day: 20% }
```

1. Every turn goes to the first engine that is not limited and not reserved.
2. Limited: the engine hit a limit (5-hour or weekly). Blocked until its reset time. No reset time given: blocked 30 minutes, then try again.
3. Reserved: what is left in a window is at or below its reserve. Blocked until that window resets. `reserve: 0` or no reserve = may hit the limit.
4. Let it hit the limit sometimes: `germes reserve off codex-api`. `germes reserve on codex-api` turns it back on.
5. A reserved engine is never used automatically, even when all others are limited.
6. All blocked: the session pauses and notifies me. It resumes at the earliest reset.
7. Later: switch by work type.

Usage data: Codex has a usage endpoint with "% used" per window (Hermes calls it, see `hermes/hermes_cli/auth_codex.py:722`). Called after each turn. Claude CLI: its stream-json output has a `rate_limit_event` with `five_hour` and `seven_day` utilization and reset times (checked in M1), so Claude gets a reserve too.

### Keeping context when switching

- Germes owns the context, not the provider.
- Germes keeps its own transcript of every turn (from the events), `PROGRESS.md`, and the git diff.
- On a switch, the new engine gets a handoff: goal, progress, last steps, uncommitted changes.
- Loops already start each round from `PROGRESS.md`, so switching between rounds is free.

## Router module

`internal/router` runs one turn on the best available engine and switches on limits while keeping context. The session does not know engines exist.

```
session worker ──► router.Run(turn) ──► codex-api  or  claude-cli
```

### Public API

```go
func New(engines []engine.Engine, store Store) *Router   // order = config order

func (r *Router) Run(ctx, engine.Turn) (<-chan engine.Event, error)  // satisfies session.Runner
func (r *Router) ListEngines(ctx) ([]api.Engine, error)              // header: "codex limited until 14:00"
```

### How Run works

1. Pick the first engine that is not limited and not reserved.
2. Handoff check: if that engine did not run this session's previous turn, add a handoff (goal, last events, `PROGRESS.md`, `git diff --stat`). One rule covers a switch in the middle of a turn and a switch between turns.
3. Run it and forward every event to the session.
4. On `LimitHit{ResetAt}`: mark it limited, emit `engine_switched`, go back to step 1 with the same turn.
5. On `Usage{Window, UsedPercent, ResetAt}`: save it; it decides "reserved" for the next pick.
6. All blocked: return `ErrUnavailable{ResetAt: earliest}`.

When all are blocked, the session worker (not the router) sets status `paused`, saves an event ("resuming at 14:00"), waits (cancelable), then retries the same message. Waiting belongs to the session because it owns the turn.

### State

- Limits and usage are saved in the store, so a restart does not retry a blocked engine.
- Shared by all sessions; one mutex. Touched only at the start of a turn and on limit or usage events.

```go
type Store interface {          // defined in router (R4)
    ListLimits(ctx) (map[string]time.Time, error)
    UpdateLimit(ctx, engine string, until time.Time) error
    ListUsage(ctx) ([]Usage, error)
    UpdateUsage(ctx, Usage) error
    ListEvents(ctx, api.EventFilter) (api.Page[api.Event], error)   // for the handoff
}
```

### Turn

```go
type Turn struct {
    SessionID   string
    Dir         string            // project folder
    Prompt      string
    ResumeIDs   map[string]string // engine → its own session id
    LastEngine string            // who ran the previous turn (handoff check)
    Extra       string            // handoff text, set by the router
}
```

### Built in M3

- Switch notice in the TUI: `⇄ now on claude-cli: codex-api reserved (17% of seven_day left, keeping 20%)`. The status bar follows the engine (events carry `Engine`, saved since migration 003).
- An engine that cannot start (not installed, logged out) is treated like a limit: the turn moves on.
- `engine/loop` gets the handoff as a message, not in the system prompt, so the prompt cache stays valid.
- Every engine blocked: `Run` returns `engine.UnavailableError`; the session shows "paused until ...", waits (Esc Esc stops waiting) and runs the message again.

### Files

```
internal/router/
  router.go    pick, run, switch on limit
  limits.go    limits, usage, reserves + store
  handoff.go   builds the handoff text
```

## Engine module

`internal/engine` (not `runtime`: that name is taken by the Go standard library). It holds the interface and event types. Two implementations:

```
internal/engine/          interface + event types
internal/engine/loop/     Germes' own agent loop (Codex now, API keys later)
internal/engine/claude/   wraps the claude CLI
```

### Interface

```go
type Engine interface {
    Name() string
    Run(ctx context.Context, t Turn) (<-chan Event, error)
}

// Event is one of:
// Text{Delta} | ToolCall{ID, Name, Args} | ToolResult{ID, Output, IsError}
// Usage{Window, UsedPercent, ResetAt} | LimitHit{ResetAt} | Compacted{Summary}
// Done{ResumeID} | Error{Err}
```

`Turn` is shown in the router section.

Rules for every engine:
- Events come in order. The channel closes after `Done` or `Error`.
- `ctx` cancelled (Esc): stop the work and close the channel.
- No approvals. Blocked commands are refused (see "Permissions").

### engine/loop

Needs: `llm` (calls the model with the Codex token), `tool` (registry), `prompt` (system prompt), the event history.

1. Load history: the session's events since the last `Compacted` event, turned into chat messages.
2. Build the system prompt with `prompt`. Frozen for the session.
3. Call the model; stream text out as `Text` events.
4. Tool call: emit `ToolCall` first (the session saves it: save before running). Blocked command: error result. Unknown tool: error result listing valid names. Else run it, emit `ToolResult`.
5. Loop until there are no tool calls or the budget runs out (up to 50 steps per turn, then one grace call to wrap up).
6. At 50% of the context window: drop old tool outputs first (free), then summarize the middle with a cheap model and emit `Compacted{Summary}`. The next history load starts from it, so compaction lives in the event log.
7. Emit `Done`.

### engine/claude

```
claude -p "<prompt>" --output-format stream-json --verbose --include-partial-messages
       --resume <id> --append-system-prompt "<memory + skills + handoff>"
       --mcp-config <germes mcp> --permission-mode acceptEdits --allowedTools "Bash WebFetch WebSearch Skill"
       --disallowedTools <blocked list> --permission-prompts none   (prompt goes in on stdin)
```

1. Start the process with its working folder set to `turn.Dir`.
2. Read the JSON output line by line: the first line gives the CLI session id (later `Done{ResumeID}`), text becomes `Text`, tool use becomes `ToolCall`, tool result becomes `ToolResult`, the final line becomes `Done`, a usage-limit error becomes `LimitHit` (format still to check).
3. Esc kills the process.
4. Claude Code reads `CLAUDE.md`, not `AGENTS.md`. Put project context in `--append-system-prompt`, or keep a `CLAUDE.md` that points to `AGENTS.md`.

### Files

```
internal/engine/engine.go          Engine, Turn, Event types
internal/engine/loop/loop.go       the loop: turns, tools, budget
internal/engine/loop/history.go    events → chat messages
internal/engine/loop/compact.go    prune + summarize
internal/engine/claude/claude.go   start the process, stop it
internal/engine/claude/parse.go    stream-json → Events
```

## Tool module

```
internal/tool/        types + registry (imports nothing from Germes)
internal/tools/...    the real tools, one package per group
```

`engine/loop` only knows `tool`. The real tools are passed in from `main.go`.

### Types

```go
type Tool struct {
    Name        string
    Toolset     string            // "files", "shell", "memory", ...
    Description string
    Schema      json.RawMessage   // JSON Schema for the arguments
    MatchArg    string            // which argument the block list checks ("command" for Bash)
    Run         func(ctx context.Context, c Call) (string, error)
}

type Call struct {
    SessionID string
    Dir       string              // project folder
    Args      json.RawMessage
}
```

### Registry

```go
func NewRegistry(tools []Tool, toolsets map[string]Toolset, blocked []string) *Registry

func (r *Registry) List(toolsets ...string) []Tool   // tools shown to the model
func (r *Registry) Get(name string) (Tool, bool)
func (r *Registry) Run(ctx, name string, c Call) Result
```

- No `Register` method. `main.go` passes the full list to `NewRegistry`; every tool is visible in one place.
- `Run` is the one place for: unknown name (error result listing valid names), blocked (error result, tool never runs), timeout (default 2 minutes), tool errors (error result, not a crash), big output (cut to about 50k characters with a note).
- Tools run one after another. Parallel tool calls: later, if turns feel slow.

### Toolsets

```go
type Toolset struct {
    Tools    []string
    Includes []string   // other toolsets, expanded recursively
}
```

| Toolset | Tools | Used by |
|---|---|---|
| `core` | files + shell + search | everything |
| `project` | core + memory + skills | project sessions |
| `overseer` | project + sessions + jobs | the overseer |
| `child` | core | subagents (no memory, no delegate) |
| `review` | memory + skills | the background self-learning review |

### Tool names

Core tools use Claude Code's names: `Bash`, `Read`, `Write`, `Edit`, `Glob`, `Grep`. One block list works for both engines (`Bash(sudo *)` means the same in both), and models know these names.

### The real tools

```
internal/tools/files/     Read, Write, Edit, Glob, Grep
internal/tools/shell/     Bash (runs through env.Environment)
internal/tools/memory/    memory
internal/tools/skills/    skill_list, skill_view, skill_save
internal/tools/search/    session_search
internal/tools/sessions/  session_list, session_create, session_send, ... (overseer)
internal/tools/worktree/  worktree_exit (project sessions; calls UpdateSession, the daemon moves the session and removes an unchanged worktree)
internal/tools/jobs/      job_create, job_list, job_stop (overseer)
```

- Each package has one constructor, for example `files.New(env) []tool.Tool`.
- `delegate` lives in `engine/loop`: it starts a child loop, so anywhere else would be an import cycle.

### Files

```
internal/tool/tool.go       Tool, Call, Result
internal/tool/registry.go   NewRegistry, List, Get, Run
internal/tool/toolset.go    Toolset, expand includes
internal/tool/blocked.go    block list matching
```

## Prompt module

`internal/prompt` builds the system prompt for a session.

```go
func NewBuilder(memory MemoryStore, skills SkillStore) *Builder
func (b *Builder) Create(ctx, dir string, role Role) (Prompt, error)   // role: project | overseer

type Prompt struct {
    System string  // full prompt, for engine/loop
    Extra  string  // only the Germes parts, for claude --append-system-prompt
}

type MemoryStore interface { Load(ctx) (memory.Snapshot, error) }   // defined in prompt (R4)
type SkillStore  interface { List(ctx) ([]skill.Summary, error) }
```

Layers, stable first (for the cache):
1. Identity: `~/.germes/SOUL.md`, or a built-in default.
2. Tool guidance (built-in text).
3. Project: `AGENTS.md`, or `CLAUDE.md` if there is no `AGENTS.md`.
4. Skills list + "if one matches, you MUST load it".
5. Memory: `MEMORY.md` + `USER.md`.
6. Today's date.

- `Extra` has only layers 3-5. Claude has its own identity and guidance. Layer 3 is left out when the project has a `CLAUDE.md` (Claude reads it itself).
- Frozen: `engine/loop` keeps one prompt per session in memory. Rebuilt only after compaction or a daemon restart.
- Skipped: Hermes' prompt-injection scan of project files. Add if needed.

## Store module

`internal/store`: everything in `~/.germes/germes.db`.

- Driver: `modernc.org/sqlite` (pure Go, no C compiler, has FTS5).
- One `Store` type. Its methods satisfy the small interfaces defined by `session`, `router` and `job`.

```go
func New(path string) (*Store, error)   // opens the db, runs migrations
func (s *Store) Close() error

// sessions: Create/Get/Update/Delete/ListSessions
// events:   CreateEvent, ListEvents (filter.Query set = full-text search)
// jobs:     Create/Get/Update/Delete/ListJobs
// engines:  ListLimits, UpdateLimit, ListUsage, UpdateUsage
```

- Tables: `sessions`, `events` (key: session id + seq, payload as JSON), `events_fts` (FTS5 over event text), `jobs`, `limits`, `usage`.
- Migrations: numbered SQL files embedded in the binary, run in `New` (R18).
- WAL mode + busy timeout: reads do not block writes.
- Search is `ListEvents` with a `Query` filter. No new verb.
- Every list is paged (R17).

## Memory module

`internal/memory`: two small files the agent keeps about itself and about me.

```
~/.germes/memories/MEMORY.md   agent's notes (max ~2200 chars)
~/.germes/memories/USER.md     about me (max ~1375 chars)
```

Entries are separated by `§` (same as Hermes).

```go
func New(dir string) *Store
func (s *Store) Load(ctx) (Snapshot, error)                         // both files
func (s *Store) CreateEntry(ctx, target Target, text string) error  // target: Agent | User
func (s *Store) UpdateEntry(ctx, target Target, old, text string) error
func (s *Store) DeleteEntry(ctx, target Target, old string) error
```

- Over the size limit: error telling the model to merge or delete entries first.
- Safe writes: mutex + write to a temp file, then rename.
- Frozen in the prompt: `Load` once at session start. Writes during the session go to disk only.
- Memory is global. Project notes go in the project's `AGENTS.md`.

## Skill module

`internal/skill`: reads and writes skills. Each skill is a folder with a `SKILL.md` (agentskills.io format, same as Hermes and Claude Code).

```
~/.germes/skills/<name>/SKILL.md     frontmatter: name, description
                       /references/, /scripts/   optional extra files
```

Sources:
- `~/.germes/skills`: read and write. The only place Germes writes.
- `~/.claude/skills`: read-only, so my Claude Code skills work in Germes too.
- Same name in both: `~/.germes/skills` wins.

```go
func New(writeDir string, readDirs ...string) *Store
func (s *Store) List(ctx) ([]Summary, error)          // name + description, for the prompt
func (s *Store) Load(ctx, name string) (Skill, error) // full SKILL.md + list of extra files
func (s *Store) Save(ctx, Skill) error                // create or update, in writeDir only
func (s *Store) Delete(ctx, name string) error        // writeDir only
```

- No cache. `List` scans the folders at session start. Add a cache if it is ever slow.
- `Save` checks the name format, a short description and valid frontmatter, then writes safely (temp file, then rename).
- YAML: `gopkg.in/yaml.v3` (shared with `config`).

## Env module

`internal/env`: runs shell commands. On my machine now, in Docker later.

```go
type Environment interface {
    Run(ctx context.Context, cmd, dir string, timeout time.Duration) (Result, error)
}

type Result struct {
    Output   string // stdout + stderr, in order
    ExitCode int
}

func NewLocal() *Local      // now
func NewDocker(...) *Docker // later, for --sandbox
```

- Local runs `bash -c <cmd>` in the project folder.
- Timeout or Esc kills the whole process group, so child processes do not keep running.
- A non-zero exit code is a normal result the model sees. `error` only means "could not start".
- A fresh shell for every call: `cd` and `export` do not carry over (same as Claude Code).
- Skipped: long-running background processes (dev servers). Add when an app needs one.

## LLM module

`internal/llm`: talks to a model API and streams the answer. Used only by `engine/loop`.

```go
type Client interface {
    Send(ctx context.Context, r Request) (<-chan Chunk, error)
}

type Request struct {
    Model    string
    System   string
    Messages []Message   // Germes' own format: role, text, tool calls, tool results
    Tools    []ToolSpec
}

// Chunk: Text{Delta} | ToolCall{ID, Name, Args} | Usage{...} | LimitHit{ResetAt} | Done | Error
```

- `llm/codex` (now): the Codex backend with my Codex login token.
- `llm/openai` (later): any OpenAI-compatible API key.
- Each implementation converts Germes' message format to its API. `engine/loop` never sees provider details.
- Network errors and 5xx: retry up to 3 times. Usage limit: no retry, emit `LimitHit` (the router switches).
- After each turn `llm/codex` calls the usage endpoint and emits `Usage` (feeds the reserve).
- Model name from config.

### Codex token (inside `llm/codex`, no separate auth module)

Same as Hermes (`hermes/hermes_cli/auth_codex.py`):
- Germes keeps its own copy in `~/.germes/auth/codex.json`. First run: import from `~/.codex/auth.json`.
- The access token lives about 10 days and the Codex CLI refreshes it on its own. So when Germes' token is about to expire, it first takes the CLI's token if that one is fresh.
- Only when both are expiring does Germes refresh. Refresh tokens are single-use, so it then writes the new tokens back to `~/.codex/auth.json` (other fields kept), or the Codex CLI would be logged out. (Changed in M2: before, Germes never wrote that file.)
- Refresh fails for good: tell me to run `codex login`.
- Checked live in M2: tool call round trip with `gpt-6.1-sol`, and the usage endpoint (only a `seven_day` window came back).
- Later, maybe: Germes' own login (Hermes has a device-code login), so Germes and Codex CLI never share a token.

## Job module

`internal/job`: runs loops and scheduled jobs (one feature).

```go
func NewScheduler(store Store, sessions Sessions) *Scheduler
func (s *Scheduler) Start(ctx) error   // daemon start: resume running jobs
func (s *Scheduler) Stop(ctx) error    // daemon shutdown

// CreateJob, ListJobs, GetJob, StartJob, StopJob, DeleteJob

type Job struct {
    ID, Name, Dir, Prompt string
    Every     time.Duration  // 0 = next round right away
    MaxRuns   int            // 0 = no limit
    Runs      int
    NextRunAt time.Time
    Status    Status         // running | stopped | done | failed
}
```

Each round:
1. Save `Runs+1` and the next `NextRunAt` before running. A crash never runs a round twice (same as Hermes).
2. Send the prompt through `session.Manager`. Each round gets a new session: fresh context, no special engine mode. The prompt says: read and update `PROGRESS.md`, reply `[DONE]` when nothing is left.
3. `Watch` that session until `turn_done`.
4. Reply has `[DONE]`: status `done`.
5. Wait until `NextRunAt`, repeat.

- Stops when: I stop it, `[DONE]`, `MaxRuns` reached, or 3 rounds in a row fail (status `failed`).
- `germes attach <job name>` opens the TUI on the latest round's session: history, live output, and I can type to steer it. Ctrl+C or `/exit` quits; the job keeps running.
- Auto-follow: when the next round starts, the attached TUI follows it and shows `── round 15 ──`.
- Skipped: cron expressions ("every Monday 9am"). Only `Every` for now.

### Workflows

A job can follow a workflow file. Germes enforces the step order (not just a prompt the agent may ignore).

```yaml
# ~/.germes/workflows/fix-issues.yaml  (or <project>/.germes/workflows/)
name: fix-issues
steps:
  - id: find
    agent: "Pick the most important open issue. Reply ISSUE:<number>, or NONE."
    next: { NONE: done, ISSUE: fix }
  - id: fix
    agent: "Fix issue {ISSUE} properly, with a test."
    next: check
  - id: check
    run: "go build ./... && go test ./..."
    on_fail: fix          # the output goes back to the agent
    next: merge
  - id: merge
    run: merge            # built in: rebase, fast-forward, clean up the worktree
    next: find
```

- Two step kinds: `agent:` sends a prompt to the session; `run:` runs a command with no LLM, through `RunTool("Bash")` (block list applies).
- Branching: by exit code, or by a reply token (`ISSUE:`, `NONE`). Values like `{ISSUE}` carry to later steps.
- An agent step can say "use skill X", so existing skills (like `issue-fix`) plug in.
- `germes job add --workflow fix-issues`. Without a workflow, a job works as before (one prompt per round).
- Runs in the `job` module. No new imports (only `api`).

Creating a workflow:
- `germes workflow new` (or `/workflow new` in the TUI): describe it in words. An agent writes the YAML, checks it, shows it to me, and saves it after I agree.
- Or write the YAML by hand.
- `germes workflow check <name>`: validates the file (unknown step ids, missing `next`, unknown built-ins). Also runs automatically on save and on `job add`.

Decisions:
- Built-in actions (`run: <name>`):
  - `merge`: rebase on the main branch, fast-forward main, reset the worktree to the new main on a fresh branch. The job keeps its worktree for the next round.
  - `cleanup`: remove the worktree and its branch (job end).
  - `notify "<text>"`: send a message to Telegram or the TUI.
  - Anything else (`git push`, `make deploy`) is a plain shell command.
- Reply format: the agent's last line is `RESULT: <TOKEN> [value]` (`RESULT: ISSUE 42`, `RESULT: NONE`). Germes adds this instruction to the prompt, listing the allowed tokens from `next`. `{ISSUE}` then holds `42`. No token or an unknown one: ask once more, then the round fails.
- Retry limit: each step has `max_retries` (default 3). Over it, the round fails (counts toward the job's 3 failed rounds).
- Restart: the current step and its values are saved in the job record before each step runs. After a restart that step runs again. Safe: the worktree keeps the changes, and `merge` is safe to repeat.
- CLI: `germes workflow new`, `germes workflow check <name>`, `germes workflow ls`.

## Daemon module

`internal/daemon`: the background process. Implements `api.Service` and serves it on the socket. Thin: the real work is in `session`, `job` and `router`.

```go
func New(sessions *session.Manager, jobs *job.Scheduler, router *router.Router, version string) *Daemon
func (d *Daemon) Run(ctx) error   // blocks until stopped
// + all api.Service methods, mostly one-line forwards
```

`main.go` builds the parts and passes them in (R3). Telegram also gets `d` as its `Service`.

Run:
1. Take the lock `~/.germes/daemon.lock` (flock). Already held: exit.
2. Remove a leftover socket file, listen on `~/.germes/germes.sock` (owner only).
3. Start the job scheduler (running jobs resume).
4. Serve `api.NewHandler(d)`.
5. SIGTERM or `StopDaemon`: stop accepting requests, stop sessions (interrupted turns are saved), stop jobs, exit.

- Auto-start (client side): `germes` runs `germes daemon` detached in its own process group. Logs: `~/.germes/logs/daemon.log`.
- Config changes apply on their own: the daemon watches `config.yaml` like its binary and reloads when no turn is running.
- Self-update: every 3 s the daemon checks its own binary. When it was replaced (`make install`) and no session is working, it stops cleanly and execs the new binary in its place (same process id, about 0.25 s). Open TUIs reconnect on their own. A running turn is never cut: the update waits until it ends. `germes daemon restart` still works.

Files: `daemon.go` (lock, socket, run, shutdown), `service.go` (`Service` methods).

## TUI module

`internal/tui`: the terminal UI. Imports only `api` (R5). Built with Bubble Tea v2 + `bubbles` (input, scroll view) + `lipgloss` (styles).

```go
func New(svc api.Service, dir string, opts Options) *TUI   // opts: new, resume, attach id
func (t *TUI) Run(ctx) error
```

- Look: like the Hermes TUI for now (`hermes/ui-tui`). Copy its layout and style in Bubble Tea.
- Components: each UI element (banner, message, transcript, status bar, composer) is its own component in its own file, with its own state and a `view(width)` that fits any width (R43).
- Inline mode: finished messages are printed into the terminal scrollback; only the live part (running tools, streaming text, status bar, input) is redrawn.
- `text_delta` events are live only (Seq 0, never saved); the finished text block is saved as one `text` event.
- `queue` events are live only too. `GetSession` returns the current queue (`Queued`) for clients that attach later.
- A rule line under the input closes the app at the bottom.
- UI work is checked with screenshots: run the TUI in tmux, capture with colors, render with `freeze`, check 50, 80 and 100+ columns.
- Screen: header (project, session title, engine status), chat (text + one compact line per tool call), input box, footer with keys.
- Events: `Watch` channel → a Bubble Tea command reads one event → `Update` adds it → `View` draws it. Watch drops: reconnect with `afterSeq`.

Keys (I use Ctrl for other things, so almost none):

| Key | Action |
|---|---|
| Enter | send |
| Shift+Enter | new line (Alt+Enter as backup in terminals that cannot tell Shift+Enter apart) |
| Esc Esc | stop the current step (`StopSession`); one Esc alone does nothing |
| Ctrl+C | quit (the session keeps running) |

Slash commands, with autocomplete: `/sessions` (picker), `/switch`, `/rename`, `/overseer`, `/reserve`, `/new`, `/help`, `/exit` (quit, same as Ctrl+C). Handled in the TUI, they call `Service`.

- Skipped: markdown rendering of model text (`glamour`). Plain text first.

Files: `tui.go` (New, Run), `model.go` (state + Update), `view.go`, `picker.go`, `commands.go`.

## MCP module

`internal/mcp`: `germes mcp` is a stdio MCP server. The Claude CLI starts it for each turn, so Claude can use Germes tools (memory, skills, session_search, and session/job tools for the overseer).

Imports only `api` (R5). It does not run tools; it forwards to the daemon:

```
claude ──stdio MCP──► germes mcp ──socket──► daemon ──► tool.Registry.Run
```

- Uses `Service.ListTools` and `Service.RunTool`. Both engines use the same tool code, block list, timeouts and output limits.
- `engine/claude` writes the MCP config with `germes mcp --session <id>`, so each call knows its session and toolset (project or overseer).
- Library: the official Go MCP SDK (`github.com/modelcontextprotocol/go-sdk`).

Files: `mcp.go` (start the server, list tools, forward calls).

## Overseer

Not its own module. The overseer is a normal session with:
1. A fixed name `overseer`. Created on first use, never cleaned up.
2. Folder `~` and the `overseer` toolset (project tools + sessions + jobs).
3. An extra role text in its prompt (`prompt.Create` with `role = overseer`).

- It can do anything I can do in a shell as my user (yolo, block list applies), plus control Germes.
- `tools/sessions` and `tools/jobs` cover the whole `Service` API one to one: sessions list, create, send, read, stop, rename, delete; jobs create, list, start, stop, delete.
- These tools call the daemon through `api.NewClient(socket)`, like `mcp`. No import cycle (the registry is built before the daemon).
- Talk to it: `germes overseer` or `/overseer` in the TUI, and from Telegram (default target). Same session everywhere.
- Notifications do not wake it. The Telegram adapter watches sessions and sends short messages itself (`todo-app: done - dark mode added`).
- Long-lived: normal compaction keeps its context small.
- No GUI control (computer use maybe later).

## Telegram module

`internal/telegram`: talk to Germes from a Telegram group with topics. Runs inside the daemon, uses `Service` directly, imports only `api` (R5).

- Bot API with long polling (`getUpdates`). No webhook, no public URL. Plain `net/http`, no library.
- Setup (optional): `germes setup telegram` asks for the bot token (from @BotFather) and my group chat ID. Saved in `config.yaml` (owner only). The token never goes in logs (R16).
- The bot needs privacy mode off (BotFather `/setprivacy`) or admin rights, or it only sees commands.
- Allowed: only my chat ID, and only my user ID in it. Everyone else is ignored.

### Topics bound to projects

- `/bind <name>` inside a topic binds that topic to a project. The topic ID comes with the message (`message_thread_id`); I never type it. (Telegram commands cannot contain `-`.)
- Finding the project by name: projects I already used (from sessions in the db) + git repos under my code folders (config, for example `~/dev`, 2-3 levels deep).
  - One match: bind it. Several: buttons (inline keyboard) to pick. `/bind` alone: recent projects as buttons. A full path also works.
- A bound topic has its own session for that project: folder = project, project toolset, loads `AGENTS.md`.
- `/new` in a topic: fresh session for the same project. `/unbind`: remove the binding.
- Rule: project topic → that project's session. General or unbound topic → the overseer (any subject, any project).
- Bindings are saved in `~/.germes/telegram.json` (owned by this module, no new `Service` methods).

### Messages

- In: `SendMessage` to the topic's session. `/sessions` lists sessions, `/stop` stops the current step.
- Out: the final answer on `turn_done` (not every token; Telegram rate limits). "typing..." while it works. Long answers split at 4096 characters.
- Notifications (job done or failed, session paused, errors): `Watch` all sessions with a kind filter. Sent to the project's bound topic; otherwise to General.
- Skipped: voice, images, files. Text only.

## Config module

`internal/config`: reads `~/.germes/config.yaml` and returns one typed object. Imports nothing from Germes.

```go
func Load(path string) (Config, error)  // missing file = defaults; checks values (R13)
func Save(path string, c Config) error  // first run, setup telegram, reserve on/off

type Config struct {
    Engines     []Engine
    Blocked     []string
    CodeDirs    []string
    CleanupDays int
    Telegram    Telegram   // empty = off
}
```

```yaml
engines:
  - name: codex-api
    model: <from codex>
    reserve: { 5h: 10%, weekly: 20% }
    reserve_off: false        # germes reserve off codex
  - name: claude-cli
blocked: [Bash(sudo *), Bash(rm -rf /*), Read(.env), Read(**/.env*)]
code_dirs: [~/dev]
cleanup_days: 30
telegram: { token: ..., chat_id: ..., user_id: ... }
```

- `main.go` calls `Load` once and hands each module its own part:

```go
cfg, err := config.Load(path)
reg := tool.NewRegistry(tools, toolsets, cfg.Blocked)
rt  := router.New(engines, store, cfg.Engines)
tg  := telegram.New(svc, telegram.Options{Token: cfg.Telegram.Token, ...})
```

- Modules that take a section import `config` for its type (`config` is a leaf). Exception: entry points (`telegram`) get their own `Options` struct, so they import only `api` (R5).
- A config change needs `germes daemon restart`. No live reload.

## CLI

Go's standard `flag` package with a switch on the subcommand. No Cobra.

`cmd/germes/main.go` dispatches:

| Command | Runs |
|---|---|
| `germes`, `-c`, `--resume`, `attach <id>`, `overseer`, `--no-worktree` | TUI |
| `germes daemon` | wires everything, `daemon.Run` |
| `germes mcp --session <id>` | MCP server |
| `ls`, `stop`, `job add/ls/start/stop/rm`, `reserve on/off`, `daemon restart`, `pin` | `internal/cli` |
| `workflow check/ls` | `internal/cli` |
| `workflow new` | TUI (a session that writes the workflow with me) |
| `setup telegram` | `main.go` itself (writes the config; `internal/cli` may only import `api`) |

- `internal/cli` imports only `api` (R5). Each command is a small function: call `Service`, print the result.
- Daemon auto-start (connect, or start it and wait for the socket) lives in `main.go`, before the client is created. The TUI, CLI commands and MCP all need it.
- Skipped: `service install` (start at login). Later.

## Setup

- Only requirement: at least one engine (Codex login or `claude` installed).
- The daemon starts itself. `germes service install` (start at login) is optional.
- Optional, off by default: Telegram (`germes setup telegram`), Docker sandbox (`--sandbox`), start at login.

## Data

Global, in `~/.germes/`:

```
config.yaml      engines, defaults
germes.db        sessions, messages, search index, jobs
memories/        MEMORY.md, USER.md
skills/          learned skills
logs/
```

In the project folder:

```
AGENTS.md        project notes and rules
PROGRESS.md      loop progress
```

## Startup (until the TUI shows)

Rule: startup only reads local files. Slow work happens after the TUI is visible or on the first message.

### Fresh install
1. `germes` starts.
2. No `~/.germes/`: create it, default `config.yaml`, `logs/`.
3. Find engines: Codex login token file, `claude` on PATH (path lookup only, do not run it). None found: print how to log in and exit.
4. Save the engine order to config.
5. No socket: start `germes daemon` in the background. Check the socket every ~10 ms until it answers.
6. Daemon: take a lock file (one daemon only), create `germes.db`, open the socket (owner only), load jobs, skip Telegram if not configured.
7. Version handshake.
8. The TUI finds the git root. Nothing is saved yet.
9. TUI shows an empty chat with the project path and engine.

### Not-so-fresh install
1. Load config.
2. Connect to the socket. Daemon running: connect. Socket file but no answer: delete it, start a daemon. No daemon: start one. A new daemon migrates the database, restarts running loops, cleans old chats.
3. Older daemon after an upgrade: nothing to do. It updates itself as soon as no session is working.
4. Always an empty chat. With `-c`, `--resume` or `attach`: show that session's recent history and status (live if a loop runs).
5. Header shows engine status, for example `codex limited until 14:00 -> using claude`.

### First message
1. Save the session.
2. Build the system prompt (identity, `AGENTS.md`, skills list, memory).
3. Refresh the Codex token if needed.
4. Run the turn on the first available engine.

### Expected time to TUI
- Daemon running: about 50 ms.
- Daemon must start: about 100-200 ms.
- Avoid at startup: running `claude`/`codex` to detect them, network calls, building the prompt, scanning skills, fixed sleeps.

## Worktrees

Every session in a git project works in its own git worktree, unless I say not to or imply it. Two sessions on one project never clash.

- Created on the session's first message: `~/.germes/worktrees/<project>/<session id>`, branch `germes/<session id>`, from the project's current `HEAD`.
- The session's `Dir` is the worktree. Tools, engines and `AGENTS.md` all work there.
- Opt out:
  - Explicit: `germes --no-worktree`, Telegram `/new here`.
  - Implied ("work directly on main", "no worktree"): the prompt tells the agent it is in a worktree, and it has a tool `worktree_exit` that moves the session to the project folder and removes the worktree if it has no changes.
- Jobs: one worktree for the whole job (all rounds share it, so progress carries over).
- Telegram: a bound topic's session gets its own worktree like any session.
- Folder is not a git repo: no worktree. If another session is already active in that folder, warn me.
- Finish: when I ask (`/merge` or in words), the agent rebases on the project's main branch, fast-forwards it, removes the worktree and deletes the branch.
- Cleanup: a worktree with no changes is removed when its session is deleted or cleaned up. A worktree with changes is never removed automatically; `germes ls` shows it.

## Failure handling

Rule: every failure is handled gracefully (R26). Recover when possible. Otherwise tell me what happened and what to do. Never crash the daemon, never lose data. All errors go to `~/.germes/logs/daemon.log` with the session ID.

### Startup and daemon
- No engine found: print how to log in, exit.
- Daemon does not answer within a few seconds: show the path to `daemon.log`, exit.
- Leftover socket file: delete it, start a new daemon.
- Socket path too long (macOS limit about 104 bytes, deep `GERMES_HOME`): use `/tmp/germes-<uid>-<hash>.sock`.
- Two daemons start at once: the lock lets one win; the other exits; the client connects to the winner.
- Old daemon after an upgrade: it replaces itself with the new binary once idle. If the exec fails, it exits; the next `germes` starts the new version.
- Daemon crash in the middle of a turn: on restart the turn is saved as `interrupted`. Jobs continue; advance-before-run means no double round.
- A goroutine panics (worker, job, telegram): recover it, save an `error` event, the session goes idle. The daemon keeps running.
- Migration fails or database broken: refuse to start, do not touch the file. Never delete data automatically.
- Database busy: wait (busy timeout).
- Disk full: saving fails, an `error` event is recorded if possible, the session goes idle.

### TUI and clients
- `Watch` stream drops: reconnect with `afterSeq`; nothing is missed.
- Client too slow: its stream is closed; it reconnects and catches up.
- Daemon dies while the TUI is open: show "daemon stopped", start it again, reconnect.
- Message while busy: waits in the session's queue (max 100), shown above the status bar (`↳ text`). When the turn ends, everything queued runs as one turn (messages joined with a blank line). One Esc never interrupts: it only shows "Esc again to stop". Esc Esc (within 800 ms) stops the turn, and queued messages then run right away (`StopSession` with `SendQueue`). `germes stop <id>` stops and drops the queue.
- Daemon stops with messages still queued: the queue lives in memory, so they are lost. Later: save the queue, if this happens in practice.

### Engines and models
- Network error or 5xx: retry 3 times with backoff, then an `error` event; the turn ends.
- Usage limit: `LimitHit`, switch engines with a handoff. No reset time given: blocked 30 minutes.
- All engines blocked: the session pauses, continues at the earliest reset.
- Codex token expired: refresh. Refresh rejected: import again from `~/.codex/auth.json`. Still failing: engine marked unavailable (router switches to Claude), tell me to run `codex login`.
- `claude` missing or not logged in: error event, engine marked unavailable for a while, router switches.
- Claude process hangs: no output for 10 minutes kills it (Hermes uses 600 s too).
- Unknown line in Claude's output: skip and log it.
- Model never stops: 50-step budget, then one grace call.
- Context too long: compaction. Still too long: error event.
- Unknown tool or bad arguments: error result to the model; the turn continues.
- Esc: cancel the turn; events already made stay saved.

### Tools
- Blocked command: error result, never runs.
- Timeout: kill the whole process group, error result.
- Huge output: cut to about 50k characters with a note.
- Non-zero exit code: normal result.
- Memory file full: error asking the model to merge entries first.
- Two sessions on one project: separate worktrees (see "Worktrees").
- Worktree cannot be created (dirty state, branch exists, disk): error event, ask me whether to work in the project folder instead.

### Router and handoff
- No `PROGRESS.md` or not a git repo: leave that part out of the handoff.

### Jobs
- A round fails: count it. Three in a row: status `failed`, notify me.
- Daemon was off at the planned time: run once at the next start. Missed rounds are not caught up.
- Job folder is gone: status `failed` with an error.

### Telegram
- Network down: polling retries with backoff.
- Rate limit (429): wait `retry_after`, retry.
- Message from anyone else: ignored.
- Bound folder is gone: reply with an error and suggest `/bind`.
- Long answer: split at 4096 characters.

### MCP
- Daemon cannot be reached: the tool call returns an error to Claude, Claude continues.

### Accepted risks
- Yolo: the agent (or the overseer from Telegram) can run destructive commands not on the block list. My choice.

## Implementation

Fast iteration: one worktree and branch per milestone, commit often, merge and push when the milestone runs. Checks before merge: `make check` (build, vet, gofmt, tests for important logic). Rulebook audit once per milestone.

0. Skeleton: `go.mod`, `cmd/germes`, import test (R2), CI. Done.
1. First working chat: TUI -> daemon -> session -> `engine/claude`, with `api`, `store`, minimal `config`, daemon auto-start, `ls`, `stop`, `-c`, `attach`. Done.
2. Own loop: `llm/codex`, `engine/loop`, `tool`, `env`, file and shell tools, `prompt`. Done. Until the router, the daemon uses the first engine in config order that can run (Codex login found, or `claude` on PATH).
3. Router: limits, reserves, handoff, Codex <-> Claude switching. Done. Shown live both ways: Codex -> Claude (Codex reserved at 83% weekly) and Claude -> Codex, with context kept.
4. Memory and skills: `memory`, `skill`, search, compaction.
5. Worktrees.
6. Jobs, then workflows.
7. MCP and the overseer.
8. Telegram.

Notes from M1:
- `api.Service` has only the methods M1 needs; later milestones add theirs.
- `GERMES_HOME` overrides `~/.germes` (useful for trying things).
- Version comes from git through `make build` (R24).

## Open items

- Check OpenAI's terms for using a ChatGPT login token in other tools.
- Check the exact usage-limit messages from the Codex API and the Claude CLI.
- How to read usage before the limit (maybe the same source as `tokscale`).
- Test that the TUI streams tokens smoothly through `Watch` (flush after every event).
- All modules designed and the module map written. Next: write the import test with the first code.
