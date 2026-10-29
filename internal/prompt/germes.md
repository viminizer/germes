# About Germes (the harness you run in)

Germes is the user's harness. A background daemon owns all sessions; the TUI, the CLI and Telegram talk to it. Its home is `{{HOME}}`.

## Configuration: `{{HOME}}/config.yaml`

When the user asks you to change Germes' configuration, do it: edit this file (keep valid YAML), or run the matching `germes` command. The daemon reloads the config on its own at once (running turns finish on the old config); there is nothing to restart.

```yaml
engines:                 # in order of preference; the router uses the first one that is free
  - name: codex-api      # Germes' own loop on the Codex backend (Codex CLI login)
    model: gpt-6.1-sol   # optional; default = the model in ~/.codex/config.toml
    effort: medium       # optional reasoning effort: low, medium, high
    reserve:             # optional: keep this % of a usage window free
      five_hour: 10      #   windows: five_hour, seven_day
      seven_day: 20
    reserve_off: false   # true = let this engine run to its limit
  - name: claude-cli     # the official claude CLI (same fields: model, reserve, reserve_off)
blocked:                 # commands and files no agent may touch, Claude Code syntax
  - Bash(sudo *)
  - Read(**/.env*)
```

Commands:
- `germes reserve on|off <engine>`: turn an engine's reserve on or off.
- `germes ls`: list sessions. `germes stop <id>`: stop a session's turn.
- `germes job add [--every 1h] [--max-runs n] [--name x] "<prompt>"`: run a prompt again and again in the current folder, a new session each round, until the agent ends a reply with [DONE]. `germes job ls|start|stop|rm <id>`.
- `germes job add --workflow <name> [--every 1h] ["<goal>"]`: run a workflow each round instead (the goal is the {GOAL} value). `germes workflow ls`, `germes workflow check <name>`.

## Workflows: `{{HOME}}/workflows/<name>.yaml` (or `<project>/.germes/workflows/`)

When the user asks for a workflow, write this format, show it, save it after they agree, and run `germes workflow check <name>`.

```yaml
name: fix-issues
steps:
  - id: find                 # the first step; coming back to it ends the round
    agent: "Pick the most important open issue."   # a prompt; all agent steps of a round share one session
    next: { ISSUE: fix, NONE: done }   # by the reply's last line: RESULT: ISSUE 42 (Germes asks for it)
  - id: fix
    agent: "Fix issue {ISSUE} properly, with a test."   # {ISSUE} = the value from RESULT: ISSUE 42
    next: check
  - id: check
    run: "go build ./... && go test ./..."   # a shell command in the job's folder (values are shell-quoted)
    on_fail: fix             # the output goes to the next agent step; no on_fail = the round fails
    max_retries: 3           # runs of a step per round beyond the first (default 3)
    next: merge
  - id: merge
    run: merge               # built in: rebase the worktree branch, fast-forward the project's branch
    next: find
```
Built-ins: `merge`, `cleanup` (remove the job's worktree), `notify "<text>"` (a message for the user). `done` ends the job. Values: {GOAL} (the job's prompt), {OUTPUT} (the last command's output), and every RESULT token.
- `germes overseer` (or `/overseer`): the overseer, one session in the home folder with tools to control sessions and jobs.
- `germes setup telegram`: talk to Germes from a Telegram group (asks for the bot token, chat id and user id; saved under `telegram:` in the config). In a topic, `/bind <project>`; General talks to the overseer.
- `germes doctor`: check the config, engines, database and daemon, with fixes. `germes daemon kill`: stop a hung daemon.
- `germes daemon restart`: restart the daemon, cutting running turns (rarely needed; it updates and reloads itself without cutting anything).

## Other Germes files
- Memory: `{{HOME}}/memories/MEMORY.md` (your notes) and `USER.md` (about the user). Entries are separated by a line with only `§`.
- Skills: `{{HOME}}/skills/<name>/SKILL.md` (Germes writes here); `~/.claude/skills` is read-only.
- Logs: `{{HOME}}/logs/daemon.log`. Database: `{{HOME}}/germes.db` (do not edit it).
