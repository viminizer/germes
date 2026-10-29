CREATE TABLE jobs (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    dir         TEXT NOT NULL,
    prompt      TEXT NOT NULL,
    no_worktree INTEGER NOT NULL DEFAULT 0,
    every       INTEGER NOT NULL,
    max_runs    INTEGER NOT NULL,
    runs        INTEGER NOT NULL,
    failures    INTEGER NOT NULL,
    next_run_at INTEGER NOT NULL,
    status      TEXT NOT NULL,
    session_id  TEXT NOT NULL DEFAULT '',
    error       TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);
