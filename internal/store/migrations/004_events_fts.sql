CREATE VIRTUAL TABLE events_fts USING fts5(text, content='events', content_rowid='rowid');

INSERT INTO events_fts (rowid, text) SELECT rowid, text FROM events WHERE text != '';

CREATE TRIGGER events_fts_insert AFTER INSERT ON events WHEN new.text != '' BEGIN
    INSERT INTO events_fts (rowid, text) VALUES (new.rowid, new.text);
END;

CREATE TRIGGER events_fts_delete AFTER DELETE ON events WHEN old.text != '' BEGIN
    INSERT INTO events_fts (events_fts, rowid, text) VALUES ('delete', old.rowid, old.text);
END;
