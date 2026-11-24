-- Defer automatic cloud uploads while a tracked game is actively running and
-- optionally coalesce frequent saves behind a per-game minimum upload interval.
ALTER TABLE settings ADD COLUMN cloud_upload_on_exit_only INTEGER NOT NULL DEFAULT 1;
ALTER TABLE settings ADD COLUMN cloud_upload_min_interval INTEGER NOT NULL DEFAULT 0;
