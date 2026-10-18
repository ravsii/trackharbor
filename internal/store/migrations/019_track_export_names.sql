-- Keep client filenames stable across metadata changes and library syncs.
CREATE TABLE track_export_names (
    track_id INTEGER PRIMARY KEY REFERENCES tracks(id) ON DELETE CASCADE,
    name TEXT NOT NULL UNIQUE,
    name_key TEXT NOT NULL UNIQUE
);
