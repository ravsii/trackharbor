---
name: settings
description: Use when adding, changing, or reviewing TrackHarbor application settings, defaults, persistence, or configuration.
---

# TrackHarbor Settings

Application settings always belong in the database `settings` table, using
`name` and `value`. Do not add settings to `config.yaml`, `internal/config.Config`,
environment variables, or a separate settings table.

- Add defaults through a new database migration; preserve existing values with
  `ON CONFLICT(name) DO NOTHING`.
- Use `Store.Setting` and `Store.SetSetting` in `internal/store/settings.go`.
  Add typed accessors there when parsing or validation is needed.
- Reject invalid values with a clear error; do not interpret zero as unlimited.
- Keep per-track state in `tracks`, not in the application settings table.
- Existing startup/bootstrap configuration is not a precedent for adding new
  application settings to config files.
