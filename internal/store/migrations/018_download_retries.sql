INSERT INTO settings(name, value) VALUES ('download_retries', '3')
ON CONFLICT(name) DO NOTHING;

ALTER TABLE tracks ADD COLUMN download_attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tracks ADD COLUMN download_failed INTEGER NOT NULL DEFAULT 0 CHECK (download_failed IN (0, 1));
