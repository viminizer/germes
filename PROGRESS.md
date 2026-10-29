# germes-quality-loop

## Fixed scope (round 1 preparation)

- Base commit: `967d5f35f5c66bb0ba64d0e3f1a3b2474f9876de` (`origin/main` after fetch and rebase).
- Repository: `https://github.com/viminizer/germes.git`, Go module `github.com/viminizer/germes`.
- Isolated worktree: `/Users/mac/.germes/worktrees/germes-1aca4ad9/cbphan`.
- Job branch: `germes/cbphan`; shared checkout `/Users/mac/dev/github/germes/germes` is not an edit target.
- Initial working tree was clean. `git fetch origin && git rebase origin/main` succeeded without conflicts or discarded work.

Exact selected paths:

1. `internal/tool/blocked.go` — parse and match configured block-list entries; normalize candidate file paths consistently before matching.
2. `internal/tool/registry_test.go` — public `Registry.Run` regression tests proving refused calls never reach handlers and ordinary calls still run.
3. `docs/PLAN.md` — only the Permissions and Tools failure-handling contracts relevant to this matcher.
4. `PROGRESS.md` — durable scope, evidence, checks and review/recovery log for this job.

Responsibility: correctness of configured tool denial matching, specifically lexical file-path candidates and wildcard semantics. Keep the selection fixed through implementation, independent Codex review, consistency audit, verification, merge and cleanup.

## Selection evidence and boundaries

The small `internal/tool` leaf package owns permission checks shared by native tools, API tool calls and reference expansion. Its matcher has two concrete risks visible in the current code:

- `candidates` computes a joined absolute path but only appends its relative form. An absolute deny pattern for a project file does not match a relative request for that same file. Candidate normalization must preserve raw/base/relative compatibility while also checking the actual lexical absolute target (and the file tools' supported `~/` syntax where applicable).
- `match` documents `*` as matching any characters, but translates it to Go regexp `.*` without dot-all semantics. Newlines therefore violate the stated wildcard contract, potentially allowing denied multiline tool arguments.

Use fake handlers and synthetic paths/arguments in tests; do not execute denied commands or access user secrets. Do not claim this block list is a complete sandbox. Shell parsing, indirect file reads through other tools, symlink/TOCTOU confinement, toolset authorization, registry redesign, scheduler and workflow code/configuration are outside this selection. No performance claim without a measurement; no dependency or new abstraction is currently justified.

Relevant peers (read-only unless scope is explicitly reconsidered by the user):

- `internal/tool/registry.go`: constructor parses rules; `Run` checks denial before invoking a handler.
- `internal/tool/tool.go`: `Tool.MatchArg`, `Call.Dir`, typed JSON decoding and public contracts.
- `internal/tools/files/files.go`: `resolve` expands `~/`, preserves absolute paths and joins relative paths to `Call.Dir`; Read/Write/Edit use `file_path`.
- `internal/tools/shell/shell.go`, `internal/env/env.go`: shell execution downstream; shell parser changes excluded.
- `internal/refs/refs.go`: reference expansion uses registry checks.
- `internal/config/config.go`: configured block-list strings.
- `internal/engine/claude`: separate CLI permission enforcement, not changed by this job.

## Contracts and gates

Read `AGENTS.md`, `docs/RULEBOOK.md`, and relevant `docs/PLAN.md` module map, Permissions, Safety and Tools failure-handling contracts. Important rules: R1/R2 (leaf boundaries), R6–R8 (small, shared solution), R12–R16 (root-cause validation, refusal and secrets), R19/R20 (checks and latest base), R23 (behavior docs), R35/R37–R41 (important safety tests through public APIs), R45 (consistency audit before merge).

Required next steps: demonstrate defects with regression tests, implement only scoped fixes, run project checks, obtain independent Codex review and consistency audit, fix actionable findings and repeat both until clean. Do not merge early or alter review gates. Fetch/rebase again before merge; preserve unrelated work and diagnose/retry recoverable failures.

## Preparation verification

- `go test ./internal/tool ./internal/refs ./internal/archtest` — passed on the unmodified base.
- No production behavior changed during preparation.
- No preparation errors or review findings yet; implementation and both review gates remain pending.
