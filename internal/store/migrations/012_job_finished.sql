-- Rounds whose result was recorded. Jobs from before: every counted round was.
ALTER TABLE jobs ADD COLUMN finished INTEGER NOT NULL DEFAULT 0;
UPDATE jobs SET finished = runs;
