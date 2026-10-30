# Rulebook

Rules this project's code must follow. Check every change against them before calling it done.
Add a rule: next free number, today's date, where it came from. Never renumber or reuse a number;
retire a rule by marking it "Retired (date): reason" and leave it in place.

Design and decisions: [PLAN.md](PLAN.md).

## Project rules

## R1. One job per module

Each module is a Go package under `internal/` with one job. It exports a small public API; everything else stays unexported.

- From: Kevin, 2026-10-29, chat ("composed of modules, each module having clear apis")
- Check: the package name says what it does; exported names are only what other modules use; no package reaches into another's internals.
- Example: none yet (no code)

## R2. Dependencies go one way

Modules form layers (see the module map in PLAN.md). A module imports only modules in lower layers. No cycles, no upward imports. A test checks the allowed imports.

- From: Kevin, 2026-10-29, chat ("no spaghetti code")
- Check: run the import-check test (to be written with the first code); look at each new import against the layer list.
- Example: none yet

## R3. No hidden wiring

No global state and no `init()` registration. A module gets what it needs through its constructor. Only `cmd/germes/main.go` connects modules.

- From: Kevin, 2026-10-29, chat
- Check: search for package-level `var` holding state and for `func init(`; constructors are created only in `main.go`.
- Example: none yet

## R4. Small interfaces and plain data

Modules talk through small interfaces and plain structs (for example `Engine`, `Turn`, `Event`). The module that needs something defines the interface it uses. No `utils`, `common` or `helpers` package.

- From: Kevin, 2026-10-29, chat
- Check: no package named utils/common/helpers/shared; interfaces have only the methods their user calls.
- Example: none yet

## R5. Clients only use the api package

Entry points (`tui`, `telegram`, `mcp`, `cli`) import only `internal/api` from Germes. They never import session, engine or other internals.

- From: Kevin, 2026-10-29, chat
- Check: the imports of `internal/tui`, `internal/telegram`, `internal/mcp`, `internal/cli` contain no other `internal/` package than `api`.
- Example: none yet

## R25. One word per action

The same action uses the same word in every module. If one module uses `Fetch` for an action, no other module uses `Get` for that same action.

- From: Kevin, 2026-10-29, chat ("keep those action words consistent")
- Check: every exported function and method starts with a verb from the list below. A new verb is added to the list first.
- Exception: a method whose name an interface requires (`Error`, `String`, `Name`, Bubble Tea's `Init`/`Update`/`View`) keeps that name.
- Booleans: a function that returns a bool starts with `Is` or `Has`, whichever reads right (`IsAvailable`, `HasQueue`). (Kevin, 2026-10-29, audit.)
- Example: none yet

| Action | Verb |
|---|---|
| make a new thing | `Create` |
| remove a thing | `Delete` |
| read one thing by id | `Get` |
| read many | `List` |
| change a thing | `Update` |
| read or write a file on disk | `Load` / `Save` |
| run a long process | `Run` |
| start or stop something that keeps running | `Start` / `Stop` |
| send a message | `Send` |
| stream changes live until stopped | `Watch` |
| block until goroutines finish | `Wait` |
| put saved state back (a checkpoint) | `Restore` |
| constructor | `New` |
| release a resource (Go's `io.Closer`) | `Close` |
| answer yes or no (returns a bool) | `Is` / `Has` |
| decide whether something may happen (returns why not) | `Check` |
| make text safe to use as one shell word | `Quote` |
| fill in references (@file, @diff…) with what they point to | `Expand` |
| put new text where old text is, inside a string | `Replace` |
| make a path absolute against a folder | `Resolve` |
| turn a value into text for people | `Format` |

## R26. Handle every failure gracefully

Every failure is handled: recover when possible, otherwise tell the user what happened and what to do. Never crash the daemon, never lose data. Each failure mode is listed in PLAN.md "Failure handling"; a new one is added there with its handling.

- From: Kevin, 2026-10-29, chat ("i want all the failures to be handled properly, and gracefully")
- Check: every goroutine recovers panics; every error path ends in a retry, a clear message, or an `error` event; the failure is in PLAN.md "Failure handling".
- Example: none yet

## R27. Context first

`context.Context` is the first parameter of every function that can block: network calls, running processes, waiting on channels or locks, calling other modules. Quick local file reads at startup (config, database open) do not need it.

- From: Kevin, 2026-10-29, chat (approved Go practices). Narrowed by Kevin 2026-10-29, audit (option a).
- Check: functions that call the network, run processes, wait, or call another module take `ctx` first.
- Example: none yet

## R28. No leaked goroutines

Every goroutine has an owner and stops when its `ctx` is cancelled.

- From: Kevin, 2026-10-29, chat (approved Go practices)
- Check: each `go` statement has a clear stop path through `ctx` or a closed channel.
- Example: none yet

## R29. The sender closes a channel

Only the goroutine that sends on a channel closes it.

- From: Kevin, 2026-10-29, chat (approved Go practices)
- Check: no `close(ch)` on the receiving side.
- Example: none yet

## R30. Check errors by type

Check errors with `errors.Is` or `errors.As`, never by comparing strings.

- From: Kevin, 2026-10-29, chat (approved Go practices)
- Check: no `err.Error() ==` or `strings.Contains(err.Error(), ...)`.
- Exception: a test may read an error's text when the text itself is the behavior under test: a message shown to the user (`germes workflow check` output), or a check that a secret is not in it. (Kevin, 2026-10-30, audit.)
- Example: `internal/skill/skill_test.go` (`errors.Is`); exceptions: `internal/job/workflow_test.go`, `internal/telegram/telegram_test.go`

## R31. Panic only for bugs

Normal errors are returned. `panic` is only for real programming bugs.

- From: Kevin, 2026-10-29, chat (approved Go practices)
- Check: search for `panic(`; each one marks an impossible state.
- Example: none yet

## R32. Structured logging

Log with `log/slog`, with the session ID when there is one.

- From: Kevin, 2026-10-29, chat (approved Go practices)
- Check: no `fmt.Print*` or `log.Print*` for logs.
- Example: none yet

## R33. Code that explains itself

Names and structure explain the code. A comment is not required; add one only when the code cannot say it (why something is done, a limit, a trap).

- From: Kevin, 2026-10-29, chat (approved Go practices). Changed by Kevin 2026-10-29, audit: "comments are not requirement, as long as the code describes itself"
- Check: a reader understands each exported name from its name and signature; comments explain why, not what.
- Example: none yet

## R34. Time and randomness as inputs

Code that tests need to control gets time and randomness as a parameter, not by calling `time.Now()` or `rand` directly.

- From: Kevin, 2026-10-29, chat (approved Go practices)
- Check: logic with time or random values takes a clock or source from its caller.
- Example: none yet

## R44. Go error strings

Error strings follow Go's convention: they start with a lowercase letter and have no ending punctuation, because they get wrapped into longer messages.

- From: Kevin, 2026-10-29, audit ("use go's convention")
- Check: `staticcheck ./...` reports no ST1005.
- Example: `internal/store/sessions.go`

## R45. Audit before merge

No milestone or feature branch merges into main before the `consistency-audit` skill has run on its diff (`git diff origin/main...HEAD`) and its findings are fixed or decided by Kevin.

- From: Kevin, 2026-10-30, chat ("you seem to be forgetting consistency-audit after implementation")
- Check: the branch's last step before merge is the audit; the summary to Kevin lists what it found.
- Example: none yet

## R43. TUI built from components

Each UI element of the TUI (banner, message, transcript, status bar, composer, picker) is its own component in its own file, with its own state and a `view(width)`. Every component renders correctly at any terminal width (responsive).

- From: Kevin, 2026-10-29, chat ("the tui should be responsive and use components too", "each ui item or element can be its own component, so we can change them later or easy to read the code")
- Check: a new UI element gets its own file and type in `internal/tui`; its view takes the width and never writes past it.
- Example: `internal/tui/statusbar.go`

## R46. Database updates keep older daemons working

During a hand-over an older daemon still runs on the database, so a normal migration only adds: new tables, and new columns that are nullable or have a `DEFAULT`. Removing or renaming goes in two steps: an additive `NNN_x.sql` adds the new form and copies the data, the new code uses it, and a later `NNN_x.contract.sql` copies what older daemons wrote meanwhile (only into rows the new code has not set) and then removes the old form. Only the daemon runs migrations: additive ones when it opens the store, contract ones once every older daemon's process is gone (`Daemon.runContract`). `germes doctor` only reads.

- From: issue #21, 2026-10-30 (Kevin: "finish the other part of the issue")
- Check: `go test ./internal/store -run TestAdditiveMigrationsKeepOlderDaemonsWorking` passes; no `DROP`, `RENAME`, or `NOT NULL` column without `DEFAULT` outside a `.contract.sql` file; only `cmd/germes/main.go` (the daemon) calls `store.New`.
- Example: `internal/store/migrate_test.go` (`TestRenameOverlapKeepsBothVersionsWrites`)

## R47. One rule set for every engine

Whether a tool call may run is decided in one place, `tool.Registry.Check` (the block list, then the hooks), for every engine. The Germes loop gets it through `Registry.Run`; an engine that runs its own tools (Claude) asks it before each call (`germes hook check`, Claude's PreToolUse hook), and its own controls (`--disallowedTools`) are only a first line, never the decision. A new engine is added the same way: route its tool calls through `Check`, never a copy of the rules. Anything that keeps `Check` from deciding (no daemon, a broken hooks file, a hook that cannot run) blocks the call.

- From: issue #42, 2026-10-30
- Check: an engine package has no rule matching of its own; `go test ./internal/tool -run 'Hook|CheckTools'` and `go test ./internal/cli -run TestCheckHook` pass.
- Example: `internal/tool/hooks.go`, `internal/cli/hook.go`

## Default rules

### Code

## R6. Smallest clear solution

Use the smallest clear solution. No abstraction that is not needed: no interface with one implementation unless it is a module boundary from the plan, no config for a value that never changes.

- From: default rulebook
- Check: each interface has a second implementation or is a planned module boundary; each config key is actually changed by someone.
- Example: none yet

## R7. Reuse before you write

Reuse a helper already in the project, then the Go standard library, then a dependency already in `go.mod`. No new dependency for what a few lines can do.

- From: default rulebook
- Check: every new line in `go.mod` has a reason in the commit message.
- Example: none yet

## R8. No duplicate logic

Kevin does not want duplicate logic. If it can be avoided, it must be avoided: the same logic lives in one place, owned by the module whose job it is, and everyone else calls it. Duplicate logic is allowed only in very rare cases, with a comment saying why it cannot be shared.

- From: default rulebook. Strengthened by Kevin 2026-10-29, audit: "i do not want duplicate logic, if it can be avoided, we must avoid it, otherwise, we can create duplicate logic in very rare cases"
- Check: `go run github.com/mibk/dupl@latest -t 40 ./internal ./cmd` for copied blocks, and search by meaning (the same job done twice with other words), e.g. channel sends, safe file writes, argument decoding, text cutting, event paging.
- Shared pieces today: `stream.Send`, `file.Save`, `file.DeleteOld`, `file.IsInside`, `text.Shorten`, `text.Replace`, `text.Transcript`, `text.QuoteShell`, `api.ListAllEvents`, `api.FormatAge`, `api.IsNotFound` / `api.HasCode`, `api.GetLoadedSkill`, `tool.NewRun`, `tool.ResolvePath`, `tool.Registry.Check`.
- Example: `internal/text/text.go`

## R9. No dead code

No unused functions, imports, files or flags, and no commented-out code.

- From: default rulebook
- Check: `go vet ./...` and `staticcheck ./...` report nothing unused.
- Example: none yet

## R10. Match the code around you

Follow the naming, structure and comment density around you. Code is formatted with `gofmt`.

- From: default rulebook
- Check: `gofmt -l .` prints nothing.
- Example: none yet

## R11. Clear names, one job per function

Names say what a thing is. Each function does one job.

- From: default rulebook
- Check: read the function name and body; one job, matching name.
- Example: none yet

### Correctness and safety

## R12. Fix the root cause

Fix the root cause in the shared place, not in one caller.

- From: default rulebook
- Check: a fix in a caller does not leave other callers of the same function broken.
- Example: none yet

## R48. Publish whole files from exclusive temporary files

Whole-file replacement uses a unique, exclusively created temporary file in the destination directory, private until its requested final permissions are applied. Check write, permission, close and rename failures; on failure remove only that save's temporary file and report cleanup errors. Never reuse a predictable `.tmp` path. Use `file.Save` where module boundaries permit; boundary-required copies document why they cannot share it. Atomic publication does not promise power-loss durability or serialize caller read-modify-write operations.

- From: atomic-save concurrency, symlink and stale-permission review findings, 2026-10-30.
- Check: inspect whole-file saving code for exclusive sibling creation and checked publication/cleanup; run `go test ./internal/file ./internal/telegram ./internal/archtest -count=1`.
- Example: `internal/file/file.go`; R5-boundary copy: `internal/telegram/telegram.go`.

## R13. Validate input where it enters

Validate input where it enters: socket requests, Telegram messages, tool arguments from the model, config files.

- From: default rulebook
- Check: `daemon`, `telegram` and tool handlers check their input before using it.
- Example: none yet

## R14. Never swallow errors

Return or handle every error. Wrap it with context (`fmt.Errorf("...: %w", err)`). Show the user a useful message and never lose data.

- From: default rulebook
- Check: no `_ = err`, no empty `if err != nil {}`.
- Example: none yet

## R15. Check permissions where the action runs

The daemon checks who is asking for every protected action: socket owner only, Telegram allowlist, blocked commands refused.

- From: default rulebook, PLAN.md (safety)
- Check: each protected action in `daemon` or `session` checks the caller.
- Example: none yet

## R16. No secrets in code, logs or commits

Tokens and keys never appear in code, logs or commits. Germes never reads the Claude login token.

- From: default rulebook, PLAN.md (engines)
- Check: search logs and diffs for tokens; no code opens Claude auth files.
- Example: none yet

### Data

## R17. Lists that can grow are paged

A list that can grow (sessions, messages, jobs, search results) is paged. No unbounded queries.

- From: default rulebook
- Check: every `SELECT` that can return many rows has `LIMIT`.
- Example: none yet

## R18. Schema changes go through migrations

Every change to `germes.db` tables goes through a numbered migration.

- From: default rulebook
- Check: no `CREATE`/`ALTER TABLE` outside the migrations in `internal/store`.
- Example: none yet

### Process

## R19. Checks pass before done

Before a task is done, `go build ./...`, `go vet ./...`, `gofmt -l .` and `go test ./...` pass.

- From: default rulebook
- Check: run the four commands.
- Example: none yet

## R20. Start from the latest code

`git fetch origin` before you plan or change anything. Branch from `origin/main`, not the local one. Fetch and rebase again before you merge.

- From: default rulebook
- Check: the branch's base is the latest `origin/main`.
- Example: none yet

## R21. Small changes in a worktree

Work in a branch or worktree. Keep each change small and focused.

- From: default rulebook
- Check: one topic per branch.
- Example: none yet

## R22. Fix review findings before merge

- From: default rulebook
- Check: no open review findings on the branch.
- Example: none yet

## R23. Update the docs

When behavior or a decision changes, update `PLAN.md` or this rulebook in the same change.

- From: default rulebook
- Check: a change in behavior comes with a doc change.
- Example: none yet

## R24. Version comes from git

A base commit is 0.1.0 and each additional commit reachable from the build's HEAD adds one patch step. Patch and minor range from 0 to 99: 0.1.99 becomes 0.2.0, and 0.99.99 becomes 1.0.0. Major has no rollover limit. No file holds the version, and no commit edits one.

- From: default rulebook; rollover after 99 requested by Kevin, 2026-10-30
- Check: no version string in source files; the build reads it from git.
- Example: none yet

### Tests

## R35. Test only important logic

Optimize for fast iteration. Write tests only for important logic, where a bug would lose data, break safety or silently do the wrong thing: store and migrations, the block list, router picking and limits, handoff, session sequence numbers and `Watch`, workflow step order, the import check (R2). Everything else is tried by running the app.

- From: default rulebook (test rules), approved by Kevin 2026-10-29. Changed by Kevin 2026-10-29: "i do not want you to write tests for every single thing ... optimize for fast iteration and development"
- Check: a change to one of the important areas above comes with a test; other changes do not need one.
- Example: none yet

## R36. Bug fixes come with a test (retired)

A bug fix comes with a test that fails before the fix and passes after.

- From: default rulebook (test rules), approved by Kevin 2026-10-29
- Retired (2026-10-29): Kevin wants fast iteration and tests only for important stuff. A bug fix in an important area (R35) still gets a test.
- Example: none yet

## R37. Test the public interface

Test behavior through the public API, not private details.

- From: default rulebook (test rules), approved by Kevin 2026-10-29
- Check: read the change and its tests.
- Example: none yet

## R38. Fast, isolated tests

No real network, clock or random values without a fake, and no order dependence between tests.

- From: default rulebook (test rules), approved by Kevin 2026-10-29
- Check: read the change and its tests.
- Example: none yet

## R39. One test, one thing

One test checks one thing, and its name says what.

- From: default rulebook (test rules), approved by Kevin 2026-10-29
- Check: read the change and its tests.
- Example: none yet

## R40. Small setup

Build the data a test needs inside the test. Keep fixtures small.

- From: default rulebook (test rules), approved by Kevin 2026-10-29
- Check: read the change and its tests.
- Example: none yet

## R41. Standard test tools

Use Go's `testing` package and the project's own helpers. No new test library without a reason.

- From: default rulebook (test rules), approved by Kevin 2026-10-29
- Check: read the change and its tests.
- Example: none yet

## R42. Tests pass before done

`go test ./...` passes before a task is done.

- From: default rulebook (test rules), approved by Kevin 2026-10-29
- Check: read the change and its tests.
- Example: none yet
