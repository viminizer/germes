CREATE TABLE engine_limits (
    engine TEXT PRIMARY KEY,
    until  INTEGER NOT NULL
);

CREATE TABLE engine_usage (
    engine       TEXT NOT NULL,
    window       TEXT NOT NULL,
    used_percent REAL NOT NULL,
    reset_at     INTEGER NOT NULL,
    PRIMARY KEY (engine, window)
);
