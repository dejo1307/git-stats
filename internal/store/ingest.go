package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dejo1307/git-stats/internal/github"
)

// Ingest derives database rows from one archived snapshot. It is the single
// path into the database — `collect` calls it after writing the archive and
// `rebuild` calls it while replaying the archive, so a rebuild reproduces
// exactly what collection produced.
//
// Ingest is idempotent: re-ingesting the same snapshot replaces its rows
// rather than duplicating them.
//
// namer decides how release asset filenames are split into platform columns;
// a zero namer records every asset without them.
func Ingest(db *DB, snap Snapshot, namer github.AssetNamer) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	snapshotID, err := upsertSnapshot(tx, snap)
	if err != nil {
		return err
	}

	// Per-snapshot tables are rewritten wholesale for idempotency.
	for _, table := range []string{"asset_count", "traffic_top", "traffic_window", "repo_stat"} {
		if _, err := tx.Exec("DELETE FROM "+table+" WHERE snapshot_id = ?", snapshotID); err != nil {
			return fmt.Errorf("clearing %s: %w", table, err)
		}
	}

	if err := ingestReleases(tx, snap, snapshotID, namer); err != nil {
		return err
	}
	if err := ingestRepo(tx, snap, snapshotID); err != nil {
		return err
	}
	if err := ingestTraffic(tx, snap, snapshotID); err != nil {
		return err
	}
	if err := ingestTop(tx, snap, snapshotID); err != nil {
		return err
	}
	if err := ingestStargazers(tx, snap); err != nil {
		return err
	}

	return tx.Commit()
}

func upsertSnapshot(tx *sql.Tx, snap Snapshot) (int64, error) {
	var id int64
	err := tx.QueryRow(`
		INSERT INTO snapshot (taken_at, raw_dir) VALUES (?, ?)
		ON CONFLICT(taken_at) DO UPDATE SET raw_dir = excluded.raw_dir
		RETURNING id`,
		snap.TakenAt.UTC().Format(time.RFC3339), snap.Dir,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("recording snapshot: %w", err)
	}
	return id, nil
}

func ingestReleases(tx *sql.Tx, snap Snapshot, snapshotID int64, namer github.AssetNamer) error {
	raw, found, err := snap.Read(FileReleases)
	if err != nil || !found {
		return err
	}
	var releases []github.Release
	if err := json.Unmarshal(raw, &releases); err != nil {
		return fmt.Errorf("decoding %s: %w", FileReleases, err)
	}

	stmt, err := tx.Prepare(`
		INSERT INTO asset_count
			(snapshot_id, asset_id, release_tag, version, asset_name,
			 os, arch, kind, download_count, size, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, rel := range releases {
		for _, a := range rel.Assets {
			// Platform columns are parsed at insert time so reporting SQL
			// never has to pick apart filenames.
			var osCol, archCol, kindCol any
			parsed, ok := namer.Parse(a.Name)
			if ok {
				osCol, archCol, kindCol = parsed.OS, parsed.Arch, parsed.Kind
			}
			if _, err := stmt.Exec(
				snapshotID, a.ID, rel.TagName, github.Version(rel.TagName), a.Name,
				osCol, archCol, kindCol,
				a.DownloadCount, a.Size, a.CreatedAt.UTC().Format(time.RFC3339),
			); err != nil {
				return fmt.Errorf("inserting asset %s: %w", a.Name, err)
			}
		}
	}
	return nil
}

func ingestRepo(tx *sql.Tx, snap Snapshot, snapshotID int64) error {
	raw, found, err := snap.Read(FileRepo)
	if err != nil || !found {
		return err
	}
	var r github.Repo
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("decoding %s: %w", FileRepo, err)
	}
	_, err = tx.Exec(`
		INSERT INTO repo_stat (snapshot_id, stars, forks, watchers, open_issues)
		VALUES (?, ?, ?, ?, ?)`,
		snapshotID, r.Stars, r.Forks, r.Subscribers, r.OpenIssues)
	return err
}

func ingestTraffic(tx *sql.Tx, snap Snapshot, snapshotID int64) error {
	for _, src := range []struct {
		metric string
		file   string
	}{
		{"views", FileViews},
		{"clones", FileClones},
	} {
		raw, found, err := snap.Read(src.file)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		var series github.TrafficSeries
		if err := json.Unmarshal(raw, &series); err != nil {
			return fmt.Errorf("decoding %s: %w", src.file, err)
		}

		// The window totals are recorded as the API reported them. Uniques are
		// de-duplicated across the whole window and cannot be recovered by
		// adding up the daily buckets below.
		if _, err := tx.Exec(`
			INSERT INTO traffic_window (snapshot_id, metric, count, uniques)
			VALUES (?, ?, ?, ?)`,
			snapshotID, src.metric, series.Count, series.Uniques,
		); err != nil {
			return fmt.Errorf("inserting %s window: %w", src.metric, err)
		}

		for _, p := range series.Points() {
			// MAX, not overwrite: overlapping 14-day windows re-report days
			// that are already final, and the current day's bucket is still
			// growing. Both are handled by keeping the larger value.
			if _, err := tx.Exec(`
				INSERT INTO traffic_day (metric, day, count, uniques) VALUES (?, ?, ?, ?)
				ON CONFLICT(metric, day) DO UPDATE SET
					count   = MAX(count, excluded.count),
					uniques = MAX(uniques, excluded.uniques)`,
				src.metric, p.Timestamp.UTC().Format("2006-01-02"), p.Count, p.Uniques,
			); err != nil {
				return fmt.Errorf("upserting %s day: %w", src.metric, err)
			}
		}
	}
	return nil
}

func ingestTop(tx *sql.Tx, snap Snapshot, snapshotID int64) error {
	for _, src := range []struct {
		kind string
		file string
	}{
		{"path", FilePaths},
		{"referrer", FileReferrers},
	} {
		raw, found, err := snap.Read(src.file)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		var items []github.TopItem
		if err := json.Unmarshal(raw, &items); err != nil {
			return fmt.Errorf("decoding %s: %w", src.file, err)
		}
		for _, it := range items {
			if _, err := tx.Exec(`
				INSERT INTO traffic_top (snapshot_id, kind, name, title, count, uniques)
				VALUES (?, ?, ?, ?, ?, ?)
				ON CONFLICT(snapshot_id, kind, name) DO UPDATE SET
					count = excluded.count, uniques = excluded.uniques`,
				snapshotID, src.kind, it.Name(), it.Title, it.Count, it.Uniques,
			); err != nil {
				return fmt.Errorf("inserting top %s: %w", src.kind, err)
			}
		}
	}
	return nil
}

func ingestStargazers(tx *sql.Tx, snap Snapshot) error {
	raw, found, err := snap.Read(FileStargazers)
	if err != nil || !found {
		return err
	}
	var stars []github.Stargazer
	if err := json.Unmarshal(raw, &stars); err != nil {
		return fmt.Errorf("decoding %s: %w", FileStargazers, err)
	}
	for _, s := range stars {
		if s.User.Login == "" {
			continue
		}
		if _, err := tx.Exec(`
			INSERT INTO stargazer (login, starred_at) VALUES (?, ?)
			ON CONFLICT(login) DO UPDATE SET starred_at = excluded.starred_at`,
			s.User.Login, s.StarredAt.UTC().Format(time.RFC3339),
		); err != nil {
			return fmt.Errorf("upserting stargazer: %w", err)
		}
	}
	return nil
}
