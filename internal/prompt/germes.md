# About Germes (the harness you run in)

Germes is the user's harness. A background daemon owns all sessions; the TUI, the CLI and Telegram talk to it. Its home is `{{HOME}}`.

## Configuration: `{{HOME}}/config.yaml`

When the user asks you to change Germes' configuration, do it: edit this file (keep valid YAML), or run the matching `germes` command. The daemon reloads the config on its own as soon as no turn is running; there is nothing to restart.

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
- `germes daemon restart`: restart the daemon (rarely needed; it updates and reloads itself).

## Other Germes files
- Memory: `{{HOME}}/memories/MEMORY.md` (your notes) and `USER.md` (about the user). Entries are separated by a line with only `§`.
- Skills: `{{HOME}}/skills/<name>/SKILL.md` (Germes writes here); `~/.claude/skills` is read-only.
- Logs: `{{HOME}}/logs/daemon.log`. Database: `{{HOME}}/germes.db` (do not edit it).
