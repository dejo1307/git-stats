// Package store owns the two storage layers: an immutable archive of raw API
// responses, and a SQLite database derived from it. The archive is the source
// of truth — the database can always be thrown away and rebuilt, so a bug in
// the derive logic can never destroy collected data.
package store

import (
	"database/sql"
	"fmt"
	"path/filepath"

	_ "modernc.org/sqlite" // pure-Go driver, no cgo
)

// DB wraps the derived SQLite database.
type DB struct {
	*sql.DB
	Path string
}

// Open opens (creating if needed) the database at path and applies the schema.
func Open(path string) (*DB, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := ensureDir(dir); err != nil {
			return nil, err
		}
	}
	sqlDB, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	if _, err := sqlDB.Exec(schema); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("applying schema: %w", err)
	}
	return &DB{DB: sqlDB, Path: path}, nil
}

// schema is idempotent: every statement is CREATE ... IF NOT EXISTS, so it is
// safe to run on every open.
const schema = `
-- One row per collection run.
CREATE TABLE IF NOT EXISTS snapshot (
  id       INTEGER PRIMARY KEY,
  taken_at TEXT NOT NULL UNIQUE,   -- RFC3339 UTC
  raw_dir  TEXT NOT NULL
);

-- Cumulative per-asset download counters as of one snapshot. Deltas between
-- consecutive snapshots (keyed on the stable GitHub asset_id) are what turn
-- these counters into a time series.
CREATE TABLE IF NOT EXISTS asset_count (
  snapshot_id    INTEGER NOT NULL REFERENCES snapshot(id) ON DELETE CASCADE,
  asset_id       INTEGER NOT NULL,
  release_tag    TEXT    NOT NULL,
  version        TEXT    NOT NULL,
  asset_name     TEXT    NOT NULL,
  os             TEXT,                -- darwin | linux | windows, NULL if unparsed
  arch           TEXT,                -- amd64 | arm64
  kind           TEXT,                -- tar.gz | sha256
  download_count INTEGER NOT NULL,
  size           INTEGER NOT NULL,
  created_at     TEXT    NOT NULL,
  PRIMARY KEY (snapshot_id, asset_id)
);
CREATE INDEX IF NOT EXISTS asset_count_by_asset ON asset_count(asset_id, snapshot_id);

-- Daily traffic buckets, deduplicated across overlapping 14-day windows.
-- Upserts take MAX(count): the counter is monotone within a day and final once
-- the day has passed, so MAX is correct both for re-reported days and for a
-- day still in progress.
CREATE TABLE IF NOT EXISTS traffic_day (
  metric  TEXT    NOT NULL,           -- views | clones
  day     TEXT    NOT NULL,           -- YYYY-MM-DD (UTC)
  count   INTEGER NOT NULL,
  uniques INTEGER NOT NULL,
  PRIMARY KEY (metric, day)
);

-- The API's own window totals for one snapshot. Kept separately from the daily
-- buckets because uniques do NOT sum: a visitor active on three days appears in
-- three daily buckets but counts once here. This is the only place the true
-- de-duplicated figure exists.
CREATE TABLE IF NOT EXISTS traffic_window (
  snapshot_id INTEGER NOT NULL REFERENCES snapshot(id) ON DELETE CASCADE,
  metric      TEXT    NOT NULL,       -- views | clones
  count       INTEGER NOT NULL,
  uniques     INTEGER NOT NULL,
  PRIMARY KEY (snapshot_id, metric)
);

-- Top-10 lists are point-in-time rankings, not daily series, so they are kept
-- per snapshot rather than merged.
CREATE TABLE IF NOT EXISTS traffic_top (
  snapshot_id INTEGER NOT NULL REFERENCES snapshot(id) ON DELETE CASCADE,
  kind        TEXT    NOT NULL,       -- path | referrer
  name        TEXT    NOT NULL,
  title       TEXT,
  count       INTEGER NOT NULL,
  uniques     INTEGER NOT NULL,
  PRIMARY KEY (snapshot_id, kind, name)
);

CREATE TABLE IF NOT EXISTS repo_stat (
  snapshot_id INTEGER PRIMARY KEY REFERENCES snapshot(id) ON DELETE CASCADE,
  stars       INTEGER NOT NULL,
  forks       INTEGER NOT NULL,
  watchers    INTEGER NOT NULL,
  open_issues INTEGER NOT NULL
);

-- Full star history; each row carries its own timestamp so this table is
-- complete after a single backfill rather than sampled over time.
CREATE TABLE IF NOT EXISTS stargazer (
  login      TEXT PRIMARY KEY,
  starred_at TEXT NOT NULL
);
`
