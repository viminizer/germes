# Germes quality-loop ledger

## Active: germes-continuous-quality-loop / round 3 / preparation

### Identity, base and coordination

- Repository verified by remote, module and GitHub metadata: `https://github.com/viminizer/germes.git`, `github.com/viminizer/germes`, `viminizer/germes`; default branch `main`, configured tracking target `origin/main`.
- Job `pn9q9i`, session `yjzp4y`, isolated worktree `/Users/mac/.germes/worktrees/germes-1aca4ad9/yjzp4y`, branch `germes/yjzp4y`. Shell tools still default to shared checkout despite worktree creation: explicitly cd to the isolated path for EVERY command and use absolute isolated paths for file tools. Shared checkout `/Users/mac/dev/github/germes/germes` is not an edit target.
- Initial isolated tree clean. `git fetch origin`, `git merge --ff-only origin/main`, and subsequent `git fetch origin && git rebase origin/main` succeeded, already current. Base/HEAD `d963a6af90711e82af9b2456414136952de47042`. Git log confirms round 2 checkpoint repair merged through PR #20; earlier active labels below are historical and do not require reopening that completed responsibility.
- Other worktree: `.claude/worktrees/codex-error`, branch `codex-error`, open PR #19 (Codex stream error messages). No file ownership overlap with this selection; do not modify it. Shared checkout clean on main at discovery. Job list shows no other active Germes coding job. Recheck ownership/status before integration or expansion.

### Selected responsibility and exact paths

ONE responsibility: safe and complete removal of a session's Git worktree and owned branch, refusing to lose actual checkout commits or uncommitted work, while allowing fully merged cleanup independently of stale upstream configuration. Inspect the ENTIRE selected implementation, including pre-existing defects, not only eventual diff.

1. `internal/worktree/worktree.go` — full leaf module; focus HasWork/Delete and the Git operations they use, with Create/IsRepo/RunGit as lifecycle context.
2. `internal/worktree/worktree_test.go` — public behavior safety regressions and existing lifecycle coverage.
3. `docs/PLAN.md` — only Worktrees, related failure handling and implementation contracts if behavior needs clarification.
4. `PROGRESS.md` — durable scope, findings, authorization and verification ledger.

Reason: this module promises never to delete work. HasWork currently checks the saved branch, not the checkout's actual HEAD (which shell users/agents can detach or switch). Delete removes the directory before `git branch -d`, whose upstream-based merge decision can reject a branch already merged into the project; a partial cleanup leaves session metadata pointing at a removed folder. These are code-evidenced review candidates, NOT reproduced findings or a clean review. Reproduce safely using temporary local repositories before production fixes. Inspect both refusal and successful cleanup paths, metadata/checkout identity and partial failures within this responsibility. No production/test repair in preparation.

Initial paths are not an authorization boundary: minimally add directly necessary owner files/contracts after checking compatibility/ownership and record why. Do not redesign session scheduling, workflow merge, snapshot retention, Git execution globally, or unrelated Codex authentication/errors. No repository-wide rewrite, new dependency or generalized abstraction is justified by preparation.

### Applicable instructions, contracts and peers

- Read root `AGENTS.md` (points to docs), full `docs/RULEBOOK.md`, `docs/PLAN.md` module map, Worktrees (1177–1190), Failure handling (1192–1260), Implementation and Answer quality context; `Makefile`, `go.mod`, `.github/workflows/ci.yml`. The whole PLAN read was truncated; the relevant Worktrees/failure contracts were subsequently read explicitly. No nested AGENTS/CLAUDE, CODEOWNERS, contribution guide, standalone verification policy or postponed-feature contract found. No scope prerequisite or owner conflict found.
- Worktree is a leaf with no internal imports. Consumers own their interfaces; avoid upward dependencies. Relevant read-only callers/peers: `internal/session/manager.go` UpdateSession/deleteWorktree/DeleteSession (errors keep metadata until Delete succeeds); `internal/job/steps.go` cleanup/merge (cleanup delegates through UpdateSession); `internal/file/file.go` DeleteOld (conservative removal); `internal/checkpoint/checkpoint.go` (retention/locking safety peer, use for comparison in audit). Read complete affected callers if a fix reaches them. No identical Git cleanup peer exists.
- Loaded skills: `verify-germes-worktree-isolation`, `consistency-audit`. Actual rulebook is `docs/RULEBOOK.md`, not the skill's default root path. Separate audit stays read-only; explicit user/workflow authorization permits subsequent fixes without routine approval. Load recovery/branch-cleanup skills if their concrete recovery circumstances arise.
- Important rules: R1–R4 boundaries; R6–R16 simplicity, reuse, typed errors, no lost work/secrets; R19/R20 checks/latest base; R23/R26 behavior docs and graceful failures; R30 typed error checks; R35/R37–R41 important safety tests via public APIs, standard testing, isolated temporary repos; R45 independent consistency audit before merge.

### Exact verification and delivery gates

- Tests ARE allowed and required for this data-loss/silent-wrong-action risk (R35); standard Go tests, local temporary repositories, no external network/credentials/user work. No new test library or real-clock dependence.
- Required: `make check` = `go build ./...`, `go vet ./...`, `test -z "$(gofmt -l .)"`, `go test -count=1 ./...`. Focused: `go test ./internal/worktree ./internal/session ./internal/archtest -count=1`. Diff checks: `git diff --check` and `git diff origin/main...HEAD --check`. No separate typecheck command. CI runs `make check` using go.mod's Go version. Rulebook audit also names `staticcheck ./...` and `go run github.com/mibk/dupl@latest -t 40 ./internal ./cmd`; do not claim either ran unless recorded.
- Workflow `/Users/mac/.germes/workflows/germes-review-loop.yaml` read, unchanged: checks -> independent Codex review of ENTIRE scope and every job change -> separate consistency-audit, repeat BOTH after fixes or integration changes until clean. Failed/truncated/incomplete review output never clears gates. Use permitted tooling recovery, never weaken read-only permissions/protections or substitute the parent audit for Codex.
- Full delivery authorized: commit on `germes/yjzp4y` only after checks and both reviews; draft-first PR with visible reviewed commit/check summary, read published state back, mark ready and merge after required checks/protections; verify remote merge, safely reconcile local main, built-in merge/cleanup only for verified merged job branches/worktrees. Fetch/rebase before integration; rerun checks and BOTH reviews if base/code changes. No force push, hard reset, protection bypass, edits to others' worktrees or removal of unmerged work. Do not prematurely commit preparation before review gates.
- This job repeats until explicitly stopped. Round completion is not continuous-goal completion; report genuine blockers/no useful work and back off. Recover SCOPE here if missing/truncated. No workflow gates changed.

### Round 3 preparation status

- Only this ledger changed; no implementation fix, regression reproduction, independent review, consistency verdict, commit or delivery claimed. `make check` PASSED (build, vet, formatting, uncached full tests), `git diff --check` PASSED and `git diff origin/main...HEAD --check` PASSED at base `d963a6af90711e82af9b2456414136952de47042`. Only this result-recording sentence changed afterward. Next workflow checks and BOTH full-scope reviews remain mandatory.

### Round 3 check gate

- Recovered current scope and candidate issues above. No prior round-3 fix or completed review exists; these are investigation candidates, not unresolved findings from an unchanged failed repair, so NEEDS_FIX's no-progress condition does not apply.
- Verified isolated cwd, branch `germes/yjzp4y`, HEAD `d963a6af90711e82af9b2456414136952de47042`; only `PROGRESS.md` modified. Ran `make check`: PASS (`go build ./...`, `go vet ./...`, `test -z "$(gofmt -l .)"`, `go test -count=1 ./...`). Ran `git diff --check`: PASS. Ran `git diff origin/main...HEAD --check`: PASS. No blocked command or missing prerequisite observed. Only this result record changed afterward. This clears checks only; independent full-scope Codex review and separate consistency audit remain required.

### Round 3 verified review repairs

- Independent Codex review completed at `d963a6af90711e82af9b2456414136952de47042`, verdict NEEDS_FIX, artifacts `/tmp/germes-worktree-review.3DAkpf/` (full initial/resume transcripts, exit 0 after same-session read-only resume, unchanged HEAD/status/diff). Separate consistency audit independently confirmed both findings: unchecked detached HEAD commits and stale-upstream refusal after directory removal. Neither finding rejected; prior checks did not clear either gate.
- Before production changes, public `Delete` regressions failed for all three scenarios: detached unmerged commits were accepted and their checkout removed; stale upstream caused branch deletion failure after checkout removal; retry after completed directory removal failed at status in the missing directory. All fixtures were isolated temporary local repositories; no user work or unrelated worktree touched.
- Shared worktree inspection now verifies the registered root and common Git directory, rejects other checked-out branches/ownership mismatches, and checks both actual checkout HEAD and the exact owned local branch against project HEAD. Nonexistence is accepted only when both directory and registration are absent; invalid or uncertain state remains an error. HasWork uses the same inspection as Delete. Existing uncommitted-work refusal remains intact.
- Cleanup follows the proven stale-upstream recovery pattern: only after ancestry verification/removal, unset the owned branch's upstream when present, then ordinary `git branch -d` rechecks against project HEAD and guards branches checked out elsewhere. No force deletion or direct ref-file surgery. Retry finishes branch cleanup after directory removal and also accepts already-completed cleanup, so existing session persistence error handling can recover without a migration or caller API change. No scope additions were needed.
- Added public regressions for detached unmerged and merged HEADs, stale upstreams, retry after directory/branch removal, switched branches, unregistered or missing-registered directories, wrong repositories, branches checked out elsewhere and unmerged branches after partial removal. Focused `go test ./internal/worktree ./internal/session ./internal/archtest -count=1` passed after the initial repair; final verification below supersedes it.
- PLAN Worktrees/failure handling now describes the conservative inspection, retry protocol and normal Git guards; arbitrary external concurrent checkout mutation is explicitly not transactional. Existing R12/R14/R23/R26/R35 already require these shared safety and recovery patterns, so no redundant rule, abstraction, dependency, instruction pointer or new global Git policy was added. Loaded `repair-merged-branch-cleanup` for the existing unset-upstream recovery precedent. Both reviews must repeat after repairs; no commit or delivery yet.

- Final repair verification at unchanged HEAD `d963a6af90711e82af9b2456414136952de47042`: `make check` PASS (build, vet, gofmt cleanliness, uncached full tests), `git diff --check` PASS, `git diff origin/main...HEAD --check` PASS. Only this observed-result ledger sentence changed afterward. Staticcheck/dupl were not rerun in this fix step. These are substantive repairs, not another build of unchanged failed code; workflow must obtain BOTH fresh full-scope reviews before commit/delivery.

### Round 3 post-repair check gate

- Confirmed substantive production/test/contract repairs for both open findings above; NEEDS_FIX's unchanged-failed-repair condition does not apply. HEAD remains `d963a6af90711e82af9b2456414136952de47042`, branch `germes/yjzp4y`; exactly the four selected files are modified.
- Ran `make check`: PASS (`go build ./...`, `go vet ./...`, `test -z "$(gofmt -l .)"`, `go test -count=1 ./...`). Ran `go test ./internal/worktree ./internal/session ./internal/archtest -count=1`: PASS. Ran `git diff --check`: PASS. Ran `git diff origin/main...HEAD --check`: PASS. All commands exited 0; no blocked commands, missing prerequisites or incomplete verification. Only this result record changed afterward. Fresh independent Codex full-scope review and separate consistency audit remain mandatory before commit/delivery.

## Historical round 2 (merged through PR #20)

## Active: germes-continuous-quality-loop / round 2 / preparation

### Current scope and recovered work

- Continue ONE responsibility: safe retention/deletion of rollback snapshot repositories, including the synchronization with Create/List/whole Restore needed to prevent deletion of active history. Round 1 left substantive uncommitted repairs; do not abandon them or start an unrelated scope. Exact selected paths: `internal/checkpoint/checkpoint.go`, `internal/checkpoint/checkpoint_test.go`, checkpoint-related contracts only in `docs/PLAN.md`, and this `PROGRESS.md` ledger. Review the entire responsibility, including pre-existing defects, not merely the diff. Other owner files may be added minimally when demonstrably necessary after ownership/contract checks; this is not a repo-wide audit.
- Selection reason: irreversible loss of rollback history from ambiguous filesystem errors, weak activity evidence and concurrent pruning. The prior round's repaired findings and regression evidence below remain useful history, NOT clean review gates. The current implementation still needs both full-scope reviews; preparation makes no additional production repair or clean-review claim.
- Verified repository: `https://github.com/viminizer/germes.git`, Go module `github.com/viminizer/germes`, GitHub `viminizer/germes`, default `main`. Verified isolated directory `/Users/mac/.germes/worktrees/germes-1aca4ad9/0g1r9j`, branch `germes/0g1r9j`; explicitly cd there for every command. Session `hc8vg7` is round 2 of repeat-enabled job `pn9q9i`. Do not edit shared checkout `/Users/mac/dev/github/germes/germes`.
- Initial HEAD was `f97e07892bd86bf3199f808dbfb3a7cd7d265a60`; four modified files were present, all selected paths. `git fetch origin` succeeded. Preserved the four-file repair in a named temporary stash, rebased onto configured `origin/main`, and reapplied it without conflicts; Git dropped only that successfully applied temporary stash. Current base/HEAD: `2b869bad05a2511a086625457fbda0ea857e0b80`. No job commit yet. Previous checks/reviews cannot approve this updated base.
- Coordination: shared checkout clean on `main` tracking `origin/main`; additional worktrees `fix/search-respects-read-blocks` (directory `grep-blocked`) and `web-fixes` both at the current base. No open PRs and no competing Germes code job shown by `germes job ls`; recheck their ownership/activity before expanding scope or integration. Do not modify their worktrees. Upstream changes touched PLAN and unrelated file/search/web owners, not checkpoint implementation; stash application merged PLAN cleanly.

### Instructions, peers, verification and delivery for round 2

- Applicable sources rediscovered/read: root `AGENTS.md`, full `docs/RULEBOOK.md`, `docs/PLAN.md` module map, Worktrees, snapshot Failure handling, rollback Answer quality and Implementation contracts; `Makefile`, `go.mod`, `.github/workflows/ci.yml`. No nested AGENTS/CLAUDE, CODEOWNERS, contribution guide, separate verification policy or postponed-feature contract was found. No scope prerequisite or owner conflict discovered. Checkpoint imports worktree only; no upward imports or new dependency needed.
- Peers/callers inspected: `internal/file/file.go` (`DeleteOld`), `internal/tool/registry.go` (`PruneOutputs`), `internal/daemon/daemon.go` (stable flock precedent), `internal/worktree/worktree.go` (safe Git operations), `internal/session/checkpoints.go` (Create/List/Restore consumers), `cmd/germes/main.go` (cleanup counts and warnings). These are read-only context unless directly necessary for this same responsibility.
- Loaded skills: `verify-germes-worktree-isolation`, `audit-filesystem-retention-safety`, `checkpoint-retention-locking`, `consistency-audit`, `recover-read-only-codex-review`. The actual rulebook is `docs/RULEBOOK.md`, not root RULEBOOK.md. Important rules: R1–R4/R6–R16 boundaries, reuse, root-cause fixes, typed errors and safety; R19/R20 verification/latest base; R23/R26 contracts and graceful failures; R34–R41 deterministic important data-safety tests; R45 consistency audit before merge.
- Tests are allowed and required for this data-safety responsibility under R35: standard Go testing, temporary files, deterministic cutoff input, no real network/secrets or permission workaround. Exact required check: `make check` = `go build ./...`, `go vet ./...`, `test -z "$(gofmt -l .)"`, `go test -count=1 ./...`. Focused: `go test ./internal/checkpoint ./internal/archtest -count=1`; retention-only: `go test ./internal/checkpoint -run TestPrune -count=1`; diff gates: `git diff --check` and `git diff origin/main...HEAD --check`. No separate typecheck command. Rulebook audit also names `staticcheck ./...` and `go run github.com/mibk/dupl@latest -t 40 ./internal ./cmd`; report honestly which actually run.
- Workflow source read, unchanged: `/Users/mac/.germes/workflows/germes-review-loop.yaml`. Gates remain checks -> independent Codex full-scope review -> separate consistency audit; repair and repeat BOTH until clean on current code. Missing/truncated reviews or command errors do not clear gates. Keep review artifacts outside the repo and use the recovery skill without weakening read-only permissions. The job list retains an older review-runner timeout/approval blocker; this does not authorize replacing workflow gates. Current preparation needs no replacement; if it recurs, diagnose using permitted longer tool deadlines/resume and report genuine remaining blockers rather than inventing approval.
- Autonomous full delivery explicitly authorized: commit only after gates; publish draft PR and visible review/check summary, read back state, mark ready and merge after required gates/protections, verify remote merge, reconcile clean local target safely, then remove only verified merged job branches/worktrees. No force-push, hard reset or protection bypass. Fetch/rebase before integration and rerun checks plus BOTH reviews after base/code changes. Do not commit preparation prematurely because the workflow's commit gate requires clean reviews.
- Continuous job is repeat-enabled until explicitly stopped; successful delivery finishes only this round. Recover scope from this active section if SCOPE is empty/truncated. No new useful responsibility should replace unfinished selected work merely because a new round started.

### Round 2 preparation retry / current base

- Repeated prepare prompt recovered this same scope; no unrelated selection or review-gate bypass. Fetch found newer `origin/main`; preserved all four modified files in a temporary named stash, rebased successfully and reapplied without conflicts. Current base/HEAD is `d0d86058beaf532e1f0b8405885f94d3e2e3fce5`, superseding the earlier base below. Upstream touched file-search authorization and its PLAN contract, not checkpoint; no instruction, rulebook, Makefile or CI changes. Remaining peer worktree is `web-fixes`; shared checkout is on main at this base. Both reviews still required on current code. Prior check results below apply only to their stated older base.

### Round 2 check gate

- At HEAD/base `d0d86058beaf532e1f0b8405885f94d3e2e3fce5`, recovered the prior findings and substantive retention/locking/activity/error-isolation repairs above. No recorded actionable finding remains without a substantive attempted repair; independent reassessment is pending, not a reason to skip checks as NEEDS_FIX.
- Ran `make check`: PASS (`go build ./...`, `go vet ./...`, `test -z "$(gofmt -l .)"`, `go test -count=1 ./...`, including checkpoint and archtest). Ran `git diff --check`: PASS. Ran `git diff origin/main...HEAD --check`: PASS. No command was blocked and no missing prerequisite was observed. Only this verification record changed afterward; no production/test changes in the check step. Checks clear this gate only, not either review or delivery gate.

### Round 2 preparation verification

- After rebase and the preparation ledger edit, `make check` PASSED (build, vet, gofmt cleanliness, uncached full tests including checkpoint/archtest), `git diff --check` PASSED, and `git diff origin/main...HEAD --check` PASSED at base `2b869bad05a2511a086625457fbda0ea857e0b80`. Only this result-recording line changed afterward. No independent review or consistency-audit verdict was obtained in preparation; both remain required on the current code. Prior round results are historical, not approval of the current base.

## Prior round 1 scope and evidence

This section records the unfinished prior round of the same responsibility. The round 2 section above supersedes its base, coordination snapshot and gate status. All sections below `Historical ledger` belong to the completed older `germes-quality-loop` job and do not constrain this round or clear its gates.

### Identity, base and coordination

- Repository: `https://github.com/viminizer/germes.git`; module `github.com/viminizer/germes`; GitHub identity/default confirmed as `viminizer/germes`, `main`.
- Isolated worktree: `/Users/mac/.germes/worktrees/germes-1aca4ad9/0g1r9j`; branch `germes/0g1r9j`. All shell commands must explicitly enter this directory. Shared checkout `/Users/mac/dev/github/germes/germes` is not an editing target.
- Base: `f97e07892bd86bf3199f808dbfb3a7cd7d265a60`. Initial tree clean; `git fetch origin && git rebase origin/main` succeeded, already up to date. `main` tracks `origin/main`; remote URL and default branch were inspected, not assumed.
- Worktree list contained only this job and the shared checkout. Shared checkout was clean on `main`; no open GitHub PRs. `germes job ls` confirmed job `pn9q9i`, session `0g1r9j`, workflow `germes-review-loop:prepare`, repeat-enabled. No competing Germes code job was found. Recheck before integration.

### Selected responsibility and exact paths

Responsibility: safe retention/deletion of rollback snapshot repositories, owned by `checkpoint.Prune`. Inspect its complete behavior, including pre-existing defects, rather than only eventual diffs. Keep this responsibility focused for this round.

1. `internal/checkpoint/checkpoint.go` — retention eligibility, filesystem-error classification and deletion in `Prune`; read the entire file to understand snapshot metadata and lifecycle.
2. `internal/checkpoint/checkpoint_test.go` — important data-safety regression tests through public `Prune`, retaining existing snapshot/restore tests.
3. `docs/PLAN.md` — only checkpoint retention and relevant failure-handling contracts.
4. `PROGRESS.md` — durable scope, evidence, authorization, checks and reviews.

Selection reason: cleanup irreversibly removes rollback history. Currently any failed project-folder stat is treated as a vanished project, and any failed index stat allows deletion even for an existing project. Permission/I/O failures are not proof of absence or age. A cutoff equality also differs from the documented “before” wording and the `file.DeleteOld` peer. These are code-evidenced review candidates; controlled regression demonstrations and independent assessment remain pending. No production behavior or tests changed in preparation. Check directory/symlink and metadata handling within this same retention responsibility rather than assuming entries are safe.

Related peers/callers read: `internal/file/file.go` (`DeleteOld`), `internal/tool/registry.go` (`PruneOutputs`), `cmd/germes/main.go` (cleanup callback, count/error logging), `internal/session/checkpoints.go` (snapshot/list/restore consumers), `internal/worktree/worktree.go` (Git runner and safe deletion precedent). No identical snapshot-retention helper exists. Checkpoint may import worktree only; avoid upward imports, new dependencies or generalized filesystem abstractions without evidence.

Snapshot creation/restoration redesign, session scheduling/locking, matcher permissions and other cleanup owners are not selected. This initial path list is not a permission boundary: minimally add directly necessary owner files/contracts after inspecting ownership and compatibility, and record why. Preserve genuine project restrictions and unrelated work.

### Instructions, tests, verification and delivery

- Applicable instructions: root `AGENTS.md`; `docs/RULEBOOK.md` (read in full); `docs/PLAN.md` (module map, Worktrees, Failure handling, Config, Answer quality/rollback contracts); `Makefile`; `go.mod`; `.github/workflows/ci.yml`. No nested AGENTS.md/CLAUDE.md, CODEOWNERS, separate verification policy, contribution guide or postponed-feature contract found in this checkout. No owner conflict or prerequisite blocks this scope.
- Rules: R1–R4 boundaries; R6–R14 smallest shared solution, errors and root causes; R16 no secrets; R19/R20 checks/latest base; R23 behavior documentation; R26 graceful failures; R30 typed errors; R34 controlled time; R35/R37–R41 tests for important data-safety behavior; R45 consistency audit before merge.
- Test policy: targeted Go standard-library tests are allowed and required for this data-loss risk. Use temporary directories and deterministic cutoff inputs, no real network, user files or permission bypass. Reproduce the defect before fixing where feasible. Do not broaden into unrelated tests.
- Exact required command: `make check`, expanding to `go build ./...`, `go vet ./...`, `test -z "$(gofmt -l .)"`, `go test -count=1 ./...`. There is no separate type-check script; Go build/vet are the defined static gates. Focused command: `go test ./internal/checkpoint ./internal/archtest`; final diff checks: `git diff --check` and `git diff origin/main...HEAD --check`. Rulebook additionally names `staticcheck ./...` and `go run github.com/mibk/dupl@latest -t 40 ./internal ./cmd` for relevant audit checks; do not claim those ran unless recorded.
- Skills loaded: `verify-germes-worktree-isolation`, `consistency-audit` (read-only audit, actual rulebook is docs/RULEBOOK.md), `recover-read-only-codex-review`. Use the last skill if the independent Codex command fails or its coverage/output is incomplete; do not weaken sandbox restrictions or substitute a parent audit.
- Workflow source: `/Users/mac/.germes/workflows/germes-review-loop.yaml`, discovered after the job-name-derived filename did not exist. It was read, not modified. Do not change gates. Recover scope here if SCOPE is missing/truncated.
- Required delivery: checks plus independent Codex review of the ENTIRE selected responsibility and separate consistency audit of scope and diff, both clean on current code; fix actionable findings and repeat both after repairs/rebases. No incomplete/truncated review clears a gate. Do not edit after clean reviews without repeating gates.
- Full autonomous delivery is authorized: commit on this branch; publish visible review summary and exact verification, read back published state, mark PR ready and merge after required gates/protections pass. Repository docs do not mandate PRs, but the active workflow requires draft-first PR publication and remote delivery. Fetch/rebase before merge; changes from integration require fresh checks and both reviews. Reconcile clean local target safely and clean up only verified merged job branches/worktrees. No force-push, hard reset, protection bypass or deletion of others' work.
- This job repeats until explicitly stopped. A completed round is not completion of the continuous user goal; report/back off for genuine external blockers or no useful work.

### Preparation evidence and remaining gates

- `go test ./internal/checkpoint ./internal/archtest` passed on the unchanged base.
- Preparation changed only this ledger; no implementation, regression reproduction, independent review, consistency audit, commit or delivery is claimed yet.
- After the scope ledger edit, `make check` passed (build, vet, formatting, uncached full tests); `git diff --check` and `git diff origin/main...HEAD --check` also passed. This result-only ledger update changes no executable code. Later check/review steps must still verify the current revision.

### Round 1 substantive retention repair

- The check step routed to NEEDS_FIX without rebuilding unchanged code. No independent review verdict exists for this round yet.
- Before changing production code, `go test ./internal/checkpoint -run TestPrunePreservesUncertainSnapshots -count=1` failed in all eight initial cases. Folder stat failure (self-referential symlink), missing index, index stat failure, empty/relative folder records, cutoff equality and a valid whitespace-suffixed project path deleted history; unreadable folder metadata silently returned success. Fixtures use only temporary paths and deterministic timestamps; no privileged permission workaround or real user files.
- `Prune` now treats only `errors.Is(err, os.ErrNotExist)` as a gone project, returns contextual errors for other inspection failures, and preserves uncertain history. Existing projects require a regular index with modification time strictly before the cutoff. A missing/non-regular index is an error, not evidence of age. Counts still report completed deletions before an error.
- Directory entries only are considered (not symlink aliases). The folder record removes exactly the newline written by `open`, preserving path whitespace, and must be absolute and map to this snapshot repo's existing name/hash. Empty, relative or mismatched records are errors; missing records still leave unrelated/uninitialized entries alone. This validates existing metadata rather than introducing a new storage format or product policy.
- Added public `Prune` safety regressions, expanded them with non-regular index and mismatched record cases, and made existing stale/fresh/orphan retention checks use a fixed cutoff. Error wrapping preserves `os.ErrNotExist` for a missing index. `go test ./internal/checkpoint ./internal/archtest -count=1` passed after the production fix; final full-check results are recorded in the step transcript.
- PLAN.md now documents eligibility, preserved metadata errors and daemon logging/retry. Existing R12/R14/R26/R30/R34/R35 already mandate this reusable safety pattern, so no redundant rulebook rule, new abstraction, dependency or additional scope path was needed. No performance claim is made.
- Post-repair `make check` PASSED (build, vet, gofmt cleanliness, uncached full tests); `git diff --check` and `git diff origin/main...HEAD --check` PASSED. The only subsequent edit records these results. Both independent Codex full-scope review and separate consistency audit remain REQUIRED on the current repaired code; the next workflow gates must obtain them before commit/delivery. No merge or clean-review claim is made here.

### Round 1 recovered review repairs

- Recovered independent read-only Codex review completed with exit 0 in `/tmp/codex-review.tsgGWr/` after an outer timeout/resume, with complete bounded reads and no execution-error/truncation markers. Its three findings are retained: unsynchronized prune versus snapshot/rollback, index-only activity evidence, and one damaged entry starving later cleanup. The earlier no-findings review does not supersede this verdict.
- Confirmed concurrent cleanup/session ownership and immediate-return starvation in source. A simple local Git experiment did rewrite the index on an unchanged snapshot, so that experiment did not reproduce the review's exact index optimization claim. Nonetheless index-only age is no longer sufficient: public retention regressions explicitly isolate an old index with a recent unchanged commit, for both loose and packed refs. This avoids relying on a Git optimization or adding a new activity format/clock API.
- Added stable per-repo `.lock` sidecars using the daemon's existing `syscall.Flock` precedent. Create/List/whole Restore serialize with each other and cleanup across cooperating processes. Prune never waits on active repos; operation waits poll with context cancellation. Locks live outside deleted repos and are never unlinked, avoiding split-inode races. Existing metadata/refs remain compatible, with no new dependency, migration or additional file path. Already-running old binaries do not participate; the PLAN states that upgrade limitation.
- Prune now preserves per-entry failures, continues independent repositories and returns `errors.Join` plus completed deletion count. Eligibility also checks HEAD, loose refs and optional packed refs, all strictly older than cutoff. Missing/invalid required metadata preserves history with errors. Added standard-library public regressions for lock contention/release/cancellation, recent loose/packed commit activity with an old index, and partial cleanup past an invalid record. Existing retention fixtures now age all activity metadata deterministically.
- `git fetch origin` initially failed with a connection reset, then succeeded on retry before edits. `go test ./internal/checkpoint ./internal/archtest -count=1` passed after repairs. Using a temporary Go overlay of the exact prior reviewed implementation, `go test -overlay <temporary-overlay.json> ./internal/checkpoint -run 'TestPrune(SkipsLockedRepo|RetainsRecentCommitWithOldIndex|ContinuesAfterInvalidRecord)' -count=1` failed all four new scenarios before repair; no repository file was reverted. Current `make check`, `git diff --check`, `git diff origin/main...HEAD --check` and `go run honnef.co/go/tools/cmd/staticcheck@latest ./...` all passed (exit 0); evidence `/tmp/checkpoint-fix.ohPr5c/`. Both reviews must follow on this revision. R12/R14/R26/R30/R34/R35 already mandate conservative shared deletion and data-safety tests; no redundant rule or new abstraction rule was warranted. PLAN documents the protocol, retention evidence, independent-error handling and upgrade limitation.

## Historical ledger (completed older job; not active scope)

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

## Round 1 normalized-basename review repair

- Recovered independent Codex review at `6cdd467` identified a basename-denial bypass; the separate consistency audit independently reproduced it. `sub/private.txt/.` resolved to the same target as `sub/private.txt` but its raw basename was `.` and no normalized basename candidate was checked. Read/Write/Edit were all affected; neither review gate cleared.
- Before repair, `git fetch origin && git rebase origin/main` succeeded without conflicts onto `6a3afee0344e883cb4a920a0e9bdc4ceee0e0b85`. Rebase changed the reviewed revision, so previous reviews cannot approve this branch.
- Extended the existing public `Registry.Run` path table with nested trailing-dot, parent-segment, absolute and home-expanded basename denials across Read/Write/Edit, plus an allowed different-file control. `go test ./internal/tool -run TestBlockListResolvedFilePaths -count=1` failed in all twelve new denial cases before the production fix; fake handlers ran when they should have been refused. No protected file was accessed.
- The shared matcher now appends `filepath.Base(abs)` alongside the already-cleaned absolute target, preserving all previous candidates. PLAN.md specifies normalized-basename matching. Existing R12/R15/R35/R37 cover the pattern; no additional rule, abstraction, import, dependency or scope expansion is needed.
- After repair, `go test ./internal/tool -run TestBlockList -count=1`, `make check` (build, vet, formatting and uncached full tests), `git diff --check` and `git diff origin/main...HEAD --check` passed. Both independent Codex review and the separate consistency audit must repeat on this repaired revision before merge; no merge or cleanup performed.

## Round 1 protected-argument review repair

- Recovered independent Codex review at `32c6cd4` completed in the read-only sandbox, with full transcript and verdict saved under `/tmp/germes-cbphan-review/`. Parent-run `make check` supplied verification without relaxing sandbox permissions. Unlike the older attached no-findings transcript, this review identified a pre-existing protected-key decoding bypass. The separate consistency audit independently confirmed it (R12/R13/R15); neither gate cleared.
- The guard's case-sensitive map lookup disagreed with typed handlers' case-insensitive JSON decoding. Duplicate keys ending in null could also erase the map value while leaving a prior typed string intact. New public Registry.Run regressions with NewRun typed fake handlers failed before repair for uppercase/mixed-case keys, conflicting aliases and duplicate/null keys. No denied commands or protected files were accessed.
- Fixed in blocked.go only: for configured pattern denials, validate the argument object and inspect top-level keys in input order using the standard JSON decoder. Reject case aliases and duplicate protected keys, including identical-spelling duplicates, rather than relying on map order or guessing handler precedence. Malformed objects and invalid protected value types fail closed with the rule and a useful reason. Canonical allowed calls and unrelated nested fields still reach handlers; missing/null empty-value behavior is unchanged.
- PLAN.md Permissions documents this boundary. Existing R12/R13/R15 and public safety-test rules already mandate the shared validation pattern; no new rule, AGENTS pointer, registry redesign or extra dependency is warranted. Only the four selected files changed; no performance claim is made.
- `git fetch origin` succeeded before repair. After repair, block-list tests, `make check` (build, vet, formatting and all tests), and `git diff --check` passed. Both independent Codex review and the separate consistency audit must run again on this repair before merge; no merge or cleanup performed.
