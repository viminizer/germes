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
- At preparation there were no errors or review findings; implementation and both review gates were pending.

## Round 1 review and fixes

- Independent Codex review at `5b0928d`: correctly reported no regressions introduced by the documentation-only commit, but did not clear existing scope defects. Its supplied transcript was truncated and its sandbox prevented test execution; it is not a clean gate.
- Separate consistency audit at `5b0928d`: verified missing absolute/home file candidates and wildcard newline mismatch, plus missing safety regressions and permission contract documentation. `make check` passed on that revision. No changes were made during the audit.
- Added public `Registry.Run` tests for relative, absolute, dot-segment, home, basename and project-relative matching across Read/Write/Edit, and newline wildcard tests for file and synthetic shell arguments. Each case checks whether the handler was invoked, including allowed controls. The tests use fake handlers, synthetic paths and a controlled HOME; no denied command executes or secret file is accessed.
- Before fixing production code, `go test ./internal/tool -run 'TestBlockList(ResolvedFilePaths|WildcardMatchesNewlines)' -count=1` failed for relative/dot-segment/absolute/home cases and all three denied newline cases, demonstrating both defects.
- Fixed the shared matcher to retain raw/basename candidates, expand home syntax only for `file_path`, preserve absolute paths, and append the cleaned absolute target and its project-relative form. Removed the misleading join of absolute input onto the project path. Wildcards use dot-all regex mode with whole-string anchors unchanged.
- The small home-resolution sequence mirrors the file tool because the leaf permission module cannot import its callers; the code comment records that boundary exception to R8. No new public API, dependency or generalized path abstraction was warranted. Existing R12/R15/R35/R37 already cover the reusable safety pattern, so no extra rulebook rule or AGENTS pointer was added. PLAN.md Permissions now provides the concrete matcher contract and test command, plus explicit lexical/shell-parser limitations.
- After fixes: `go test ./internal/tool -run TestBlockList -count=1`, `make check` (build, vet, formatting, all tests), and `git diff --check` passed.
- No performance improvement is claimed; no scheduler/workflow or out-of-scope production files were modified.
- Both independent Codex review and consistency audit MUST run again on the fixed revision. Their prior results do not approve these changes; merge and cleanup remain gated.

## Round 1 protected-argument review repair

- Recovered independent Codex review at `32c6cd4` completed in the read-only sandbox, with full transcript and verdict saved under `/tmp/germes-cbphan-review/`. Parent-run `make check` supplied verification without relaxing sandbox permissions. Unlike the older attached no-findings transcript, this review identified a pre-existing protected-key decoding bypass. The separate consistency audit independently confirmed it (R12/R13/R15); neither gate cleared.
- The guard's case-sensitive map lookup disagreed with typed handlers' case-insensitive JSON decoding. Duplicate keys ending in null could also erase the map value while leaving a prior typed string intact. New public Registry.Run regressions with NewRun typed fake handlers failed before repair for uppercase/mixed-case keys, conflicting aliases and duplicate/null keys. No denied commands or protected files were accessed.
- Fixed in blocked.go only: for configured pattern denials, validate the argument object and inspect top-level keys in input order using the standard JSON decoder. Reject case aliases and duplicate protected keys, including identical-spelling duplicates, rather than relying on map order or guessing handler precedence. Malformed objects and invalid protected value types fail closed with the rule and a useful reason. Canonical allowed calls and unrelated nested fields still reach handlers; missing/null empty-value behavior is unchanged.
- PLAN.md Permissions documents this boundary. Existing R12/R13/R15 and public safety-test rules already mandate the shared validation pattern; no new rule, AGENTS pointer, registry redesign or extra dependency is warranted. Only the four selected files changed; no performance claim is made.
- `git fetch origin` succeeded before repair. After repair, block-list tests, `make check` (build, vet, formatting and all tests), and `git diff --check` passed. Both independent Codex review and the separate consistency audit must run again on this repair before merge; no merge or cleanup performed.
