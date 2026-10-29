CREATE TABLE sessions (
    id         TEXT PRIMARY KEY,
    title      TEXT NOT NULL,
    project    TEXT NOT NULL,
    dir        TEXT NOT NULL,
    status     TEXT NOT NULL,
    engine     TEXT NOT NULL DEFAULT '',
    resume_ids TEXT NOT NULL DEFAULT '{}',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX sessions_project ON sessions (project, updated_at);

CREATE TABLE events (
    session_id TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    seq        INTEGER NOT NULL,
    time       INTEGER NOT NULL,
    kind       TEXT NOT NULL,
    text       TEXT NOT NULL DEFAULT '',
    tool       TEXT,
    PRIMARY KEY (session_id, seq)
);
