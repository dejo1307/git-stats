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
	if err := ingestForks(tx, snap); err != nil {
		return err
	}
	if err := ingestFileChanges(tx, snap); err != nil {
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
		if err := upsertRelease(tx, rel); err != nil {
			return err
		}
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

// upsertRelease records a release as a dated event.
//
// Drafts are skipped: an unpublished release is invisible to everyone but the
// maintainer, so it cannot have moved any number and would only add a marker to
// the timeline for something nobody saw. It also has no publication date to
// place that marker at.
func upsertRelease(tx *sql.Tx, rel github.Release) error {
	if rel.Draft || rel.PublishedAt.IsZero() {
		return nil
	}
	name := rel.Name
	if name == "" {
		name = rel.TagName
	}
	_, err := tx.Exec(`
		INSERT INTO release (tag, name, published_at, prerelease, draft, body_len)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(tag) DO UPDATE SET
			name = excluded.name, published_at = excluded.published_at,
			prerelease = excluded.prerelease, draft = excluded.draft,
			body_len = excluded.body_len`,
		rel.TagName, name, rel.PublishedAt.UTC().Format(time.RFC3339),
		rel.Prerelease, rel.Draft, len(rel.Body))
	if err != nil {
		return fmt.Errorf("upserting release %s: %w", rel.TagName, err)
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
	return markBackfilled(tx, "stars", snap.TakenAt)
}

// markBackfilled records that a self-dating history was captured up to this
// snapshot, which is what tells a later reader that a stretch with no rows was
// a quiet period rather than an uncollected one.
func markBackfilled(tx *sql.Tx, kind string, at time.Time) error {
	_, err := tx.Exec(`
		INSERT INTO backfill (kind, captured_at) VALUES (?, ?)
		ON CONFLICT(kind) DO UPDATE SET
			captured_at = MAX(captured_at, excluded.captured_at)`,
		kind, at.UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("recording %s backfill: %w", kind, err)
	}
	return nil
}

func ingestForks(tx *sql.Tx, snap Snapshot) error {
	raw, found, err := snap.Read(FileForks)
	if err != nil || !found {
		return err
	}
	var forks []github.Fork
	if err := json.Unmarshal(raw, &forks); err != nil {
		return fmt.Errorf("decoding %s: %w", FileForks, err)
	}
	for _, f := range forks {
		if f.FullName == "" {
			continue
		}
		if _, err := tx.Exec(`
			INSERT INTO fork (full_name, created_at) VALUES (?, ?)
			ON CONFLICT(full_name) DO UPDATE SET created_at = excluded.created_at`,
			f.FullName, f.CreatedAt.UTC().Format(time.RFC3339),
		); err != nil {
			return fmt.Errorf("upserting fork: %w", err)
		}
	}
	return markBackfilled(tx, "forks", snap.TakenAt)
}

// ingestFileChanges records the commit history of every tracked path captured
// in this snapshot. Each file names its own path, so which paths were tracked
// is read out of the archive rather than out of the current configuration.
func ingestFileChanges(tx *sql.Tx, snap Snapshot) error {
	names, err := snap.CommitsFiles()
	if err != nil {
		return err
	}
	for _, name := range names {
		raw, found, err := snap.Read(name)
		if err != nil || !found {
			return err
		}
		var history github.PathCommits
		if err := json.Unmarshal(raw, &history); err != nil {
			return fmt.Errorf("decoding %s: %w", name, err)
		}
		if history.Path == "" {
			return fmt.Errorf("%s names no path", name)
		}
		for _, item := range history.Commits {
			var c github.Commit
			if err := json.Unmarshal(item, &c); err != nil {
				return fmt.Errorf("decoding a commit in %s: %w", name, err)
			}
			if c.SHA == "" {
				continue
			}
			adds, dels, err := commitSize(snap, history.Path, c.SHA)
			if err != nil {
				return err
			}
			// COALESCE keeps a size already recorded when this ingest has no
			// cached detail for the commit, so a pruned cache degrades to
			// "no new sizes" rather than erasing the ones already derived.
			if _, err := tx.Exec(`
				INSERT INTO file_change (path, sha, committed_at, subject, additions, deletions)
				VALUES (?, ?, ?, ?, ?, ?)
				ON CONFLICT(path, sha) DO UPDATE SET
					committed_at = excluded.committed_at,
					subject      = excluded.subject,
					additions    = COALESCE(excluded.additions, additions),
					deletions    = COALESCE(excluded.deletions, deletions)`,
				history.Path, c.SHA, c.At().UTC().Format(time.RFC3339), c.Subject(), adds, dels,
			); err != nil {
				return fmt.Errorf("upserting commit %s: %w", c.SHA, err)
			}
		}
	}
	return nil
}

// commitSize returns the tracked path's additions and deletions in one commit,
// or nil columns when the commit's detail has not been fetched.
func commitSize(snap Snapshot, path, sha string) (additions, deletions any, err error) {
	raw, found, err := snap.ReadCommitDetail(sha)
	if err != nil || !found {
		return nil, nil, err
	}
	var detail github.CommitDetail
	if err := json.Unmarshal(raw, &detail); err != nil {
		return nil, nil, fmt.Errorf("decoding cached commit %s: %w", sha, err)
	}
	file, ok := detail.File(path)
	if !ok {
		// The commit list said this commit touched the path, so a detail that
		// disagrees means a rename or a merge commit, whose file list GitHub
		// reports against its first parent. Recording zero would claim the
		// change was empty; leaving it unknown is the honest column.
		return nil, nil, nil
	}
	return file.Additions, file.Deletions, nil
}
