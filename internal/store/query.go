package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Snap identifies one collection run.
type Snap struct {
	ID      int64
	TakenAt time.Time
}

// Snapshots returns every collection run in chronological order.
func (db *DB) Snapshots() ([]Snap, error) {
	rows, err := db.Query(`SELECT id, taken_at FROM snapshot ORDER BY taken_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var snaps []Snap
	for rows.Next() {
		var s Snap
		var takenAt string
		if err := rows.Scan(&s.ID, &takenAt); err != nil {
			return nil, err
		}
		if s.TakenAt, err = time.Parse(time.RFC3339, takenAt); err != nil {
			return nil, fmt.Errorf("parsing snapshot time %q: %w", takenAt, err)
		}
		snaps = append(snaps, s)
	}
	return snaps, rows.Err()
}

// ObservedSnapshots returns only those snapshots that actually captured
// release data, oldest first.
//
// A run that failed before fetching releases still records a snapshot row, but
// with no counters attached. Such a snapshot means "not observed", never
// "zero" — charting it as zero invents a cliff in every series, and using it
// as an interval boundary misattributes the next interval's growth. Every
// download-derived query works from this list rather than from Snapshots.
func (db *DB) ObservedSnapshots() ([]Snap, error) {
	rows, err := db.Query(`
		SELECT s.id, s.taken_at FROM snapshot s
		WHERE EXISTS (SELECT 1 FROM asset_count a WHERE a.snapshot_id = s.id)
		ORDER BY s.taken_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var snaps []Snap
	for rows.Next() {
		var s Snap
		var takenAt string
		if err := rows.Scan(&s.ID, &takenAt); err != nil {
			return nil, err
		}
		if s.TakenAt, err = time.Parse(time.RFC3339, takenAt); err != nil {
			return nil, fmt.Errorf("parsing snapshot time %q: %w", takenAt, err)
		}
		snaps = append(snaps, s)
	}
	return snaps, rows.Err()
}

// assetRow is one asset's cumulative counter at one snapshot.
type assetRow struct {
	snapshotID int64
	assetID    int64
	tag        string
	version    string
	platform   string
	kind       string
	count      int64
}

func (db *DB) assetRows() ([]assetRow, error) {
	rows, err := db.Query(`
		SELECT snapshot_id, asset_id, release_tag, version,
		       COALESCE(os, '') , COALESCE(arch, ''), COALESCE(kind, ''), download_count
		FROM asset_count
		ORDER BY asset_id, snapshot_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []assetRow
	for rows.Next() {
		var r assetRow
		var os, arch string
		if err := rows.Scan(&r.snapshotID, &r.assetID, &r.tag, &r.version, &os, &arch, &r.kind, &r.count); err != nil {
			return nil, err
		}
		// An empty platform is not a missing value: any os-arch pair parses, so
		// an asset that did not is not a platform binary at all (a manifest, a
		// source tarball). Callers route it to Other rather than guessing.
		if os != "" && arch != "" {
			r.platform = os + "-" + arch
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Interval is the download activity between two consecutive snapshots.
//
// Because snapshots are taken manually they are irregularly spaced, so Days is
// always carried alongside Total: a delta of 20 means something quite
// different over one day than over three weeks. Never render Total as a daily
// rate without dividing by Days.
type Interval struct {
	From, To time.Time
	Days     float64
	Total    int64
	// Other is the part of Total that did not come from a platform binary:
	// assets whose name does not carry an os-arch, such as release manifests.
	// They are real downloads but not installs, so they get a column of their
	// own instead of masquerading as a platform.
	Other      int64
	ByPlatform map[string]int64
	ByKind     map[string]int64
	ByVersion  map[string]int64
}

// PerDay normalises the interval's total to a daily rate.
func (i Interval) PerDay() float64 {
	if i.Days <= 0 {
		return 0
	}
	return float64(i.Total) / i.Days
}

// Intervals computes download deltas between consecutive snapshots.
//
// Deltas are keyed on the GitHub asset_id, which is stable for the life of an
// upload. Three cases are handled deliberately:
//
//   - An asset present in both snapshots contributes the difference of its
//     cumulative counters.
//   - An asset seen for the first time in a later snapshot contributes its
//     whole count: the asset was created inside this interval, so everything
//     it has accrued belongs to the interval.
//   - A decrease contributes zero. Counters only grow, so a drop means the
//     asset was replaced or its release deleted, not that downloads were
//     returned.
//
// The first snapshot produces no interval at all — its counters are an
// all-time baseline that cannot be attributed to any date.
func (db *DB) Intervals() ([]Interval, error) {
	snaps, err := db.ObservedSnapshots()
	if err != nil {
		return nil, err
	}
	if len(snaps) < 2 {
		return nil, nil
	}

	rows, err := db.assetRows()
	if err != nil {
		return nil, err
	}

	// index of each snapshot id within the chronological ordering
	order := make(map[int64]int, len(snaps))
	for i, s := range snaps {
		order[s.ID] = i
	}

	intervals := make([]Interval, len(snaps)-1)
	for i := range intervals {
		intervals[i] = Interval{
			From:       snaps[i].TakenAt,
			To:         snaps[i+1].TakenAt,
			Days:       snaps[i+1].TakenAt.Sub(snaps[i].TakenAt).Hours() / 24,
			ByPlatform: map[string]int64{},
			ByKind:     map[string]int64{},
			ByVersion:  map[string]int64{},
		}
	}

	// rows are ordered by (asset_id, snapshot_id), so each asset's history is
	// a contiguous run.
	for start := 0; start < len(rows); {
		end := start
		for end < len(rows) && rows[end].assetID == rows[start].assetID {
			end++
		}
		history := rows[start:end]
		start = end

		for j, cur := range history {
			idx, ok := order[cur.snapshotID]
			if !ok || idx == 0 {
				// Baseline snapshot: nothing to attribute.
				continue
			}
			// First observation of an asset: the whole counter is new. If the
			// asset was missing from intervening snapshots, growth since it
			// was last seen lands in this interval.
			delta := cur.count
			if j > 0 {
				delta = cur.count - history[j-1].count
			}
			if delta < 0 {
				delta = 0
			}
			if delta == 0 {
				continue
			}
			iv := &intervals[idx-1]
			iv.Total += delta
			if cur.platform == "" {
				iv.Other += delta
			} else {
				iv.ByPlatform[cur.platform] += delta
			}
			iv.ByKind[cur.kind] += delta
			iv.ByVersion[cur.version] += delta
		}
	}
	return intervals, nil
}

// Totals is the cumulative all-time picture at one snapshot.
type Totals struct {
	TakenAt time.Time
	Total   int64
	// Other is the part of Total that did not come from a platform binary:
	// assets whose name does not carry an os-arch, such as release manifests.
	// They are real downloads but not installs, so they get a column of their
	// own instead of masquerading as a platform.
	Other      int64
	ByPlatform map[string]int64
	ByKind     map[string]int64
	ByRelease  []ReleaseTotal
}

// ReleaseTotal is one release's all-time downloads.
type ReleaseTotal struct {
	Tag         string
	PublishedAt time.Time
	Total       int64
	// Archives excludes checksum files, which are fetched by install scripts
	// alongside the artifact and would otherwise double-count a single install.
	Archives int64
}

// checksumKinds are asset kinds that verify another asset rather than being a
// download in their own right.
var checksumKinds = []string{"sha256", "sha512"}

// Archives is all-time downloads of release artifacts, excluding checksums
// and other non-install assets. Both are fetched without installing anything:
// a checksum verifies an artifact, and a manifest merely describes one.
func (t Totals) Archives() int64 { return t.Total - t.Checksums() - t.Other }

// Checksums is all-time downloads of checksum files. Install scripts and
// self-updaters fetch one alongside every artifact; browsers and plain curl
// normally do not, which is what makes the number informative.
func (t Totals) Checksums() int64 {
	var sum int64
	for _, kind := range checksumKinds {
		sum += t.ByKind[kind]
	}
	return sum
}

// LatestTotals returns cumulative counters as of the most recent snapshot.
func (db *DB) LatestTotals() (Totals, error) {
	snaps, err := db.ObservedSnapshots()
	if err != nil || len(snaps) == 0 {
		return Totals{}, err
	}
	latest := snaps[len(snaps)-1]

	t := Totals{
		TakenAt:    latest.TakenAt,
		ByPlatform: map[string]int64{},
		ByKind:     map[string]int64{},
	}

	rows, err := db.Query(`
		SELECT COALESCE(os, ''), COALESCE(arch, ''), COALESCE(kind, ''), SUM(download_count)
		FROM asset_count WHERE snapshot_id = ?
		GROUP BY os, arch, kind`, latest.ID)
	if err != nil {
		return Totals{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var os, arch, kind string
		var sum int64
		if err := rows.Scan(&os, &arch, &kind, &sum); err != nil {
			return Totals{}, err
		}
		t.Total += sum
		if os != "" && arch != "" {
			t.ByPlatform[os+"-"+arch] += sum
		} else {
			// No os-arch in the name: a real download, not an install.
			t.Other += sum
		}
		t.ByKind[kind] += sum
	}
	if err := rows.Err(); err != nil {
		return Totals{}, err
	}

	relRows, err := db.Query(`
		SELECT release_tag, MIN(created_at), SUM(download_count),
		       SUM(CASE WHEN COALESCE(kind, '') IN ('sha256', 'sha512')
		                THEN 0 ELSE download_count END)
		FROM asset_count WHERE snapshot_id = ?
		GROUP BY release_tag`, latest.ID)
	if err != nil {
		return Totals{}, err
	}
	defer relRows.Close()
	for relRows.Next() {
		var r ReleaseTotal
		var created string
		if err := relRows.Scan(&r.Tag, &created, &r.Total, &r.Archives); err != nil {
			return Totals{}, err
		}
		r.PublishedAt, _ = time.Parse(time.RFC3339, created)
		t.ByRelease = append(t.ByRelease, r)
	}
	if err := relRows.Err(); err != nil {
		return Totals{}, err
	}
	sort.Slice(t.ByRelease, func(i, j int) bool {
		return t.ByRelease[i].PublishedAt.After(t.ByRelease[j].PublishedAt)
	})
	return t, nil
}

// CumulativePoint is the all-time download total observed at one snapshot.
// Unlike interval deltas this is exact at every point, which makes it the
// honest series to chart when snapshots are irregularly spaced.
type CumulativePoint struct {
	TakenAt time.Time
	Total   int64
}

// Cumulative returns the all-time download total at each snapshot.
func (db *DB) Cumulative() ([]CumulativePoint, error) {
	rows, err := db.Query(`
		SELECT s.taken_at, SUM(a.download_count)
		FROM snapshot s JOIN asset_count a ON a.snapshot_id = s.id
		GROUP BY s.id ORDER BY s.taken_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CumulativePoint
	for rows.Next() {
		var p CumulativePoint
		var takenAt string
		if err := rows.Scan(&takenAt, &p.Total); err != nil {
			return nil, err
		}
		p.TakenAt, _ = time.Parse(time.RFC3339, takenAt)
		out = append(out, p)
	}
	return out, rows.Err()
}

// TrafficPoint is one merged daily traffic bucket.
type TrafficPoint struct {
	Day     string
	Count   int64
	Uniques int64
}

// Traffic returns the merged daily series for "views" or "clones", oldest
// first. Days never captured within GitHub's 14-day retention are simply
// absent — they cannot be recovered.
func (db *DB) Traffic(metric string) ([]TrafficPoint, error) {
	rows, err := db.Query(
		`SELECT day, count, uniques FROM traffic_day WHERE metric = ? ORDER BY day`, metric)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TrafficPoint
	for rows.Next() {
		var p TrafficPoint
		if err := rows.Scan(&p.Day, &p.Count, &p.Uniques); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// TrafficWindow is the API's own totals for its trailing window.
type TrafficWindow struct {
	TakenAt time.Time
	Count   int64
	// Uniques is de-duplicated across the whole window. It is always <= the
	// sum of the daily buckets' uniques and is the only correct answer to
	// "how many distinct visitors" — adding up daily uniques double-counts
	// anyone who came back on another day.
	Uniques int64
	Found   bool
}

// LatestTrafficWindow returns the most recently captured window totals for
// "views" or "clones".
func (db *DB) LatestTrafficWindow(metric string) (TrafficWindow, error) {
	var w TrafficWindow
	var takenAt string
	err := db.QueryRow(`
		SELECT s.taken_at, w.count, w.uniques
		FROM traffic_window w JOIN snapshot s ON s.id = w.snapshot_id
		WHERE w.metric = ?
		ORDER BY s.taken_at DESC LIMIT 1`, metric).Scan(&takenAt, &w.Count, &w.Uniques)
	if errors.Is(err, sql.ErrNoRows) {
		return TrafficWindow{}, nil
	}
	if err != nil {
		return TrafficWindow{}, err
	}
	w.TakenAt, _ = time.Parse(time.RFC3339, takenAt)
	w.Found = true
	return w, nil
}

// TopEntry is one popular path or referrer as of the latest snapshot.
type TopEntry struct {
	Name    string
	Title   string
	Count   int64
	Uniques int64
}

// LatestTop returns the most recent top-10 list of kind "path" or "referrer".
func (db *DB) LatestTop(kind string) ([]TopEntry, error) {
	rows, err := db.Query(`
		SELECT name, COALESCE(title, ''), count, uniques FROM traffic_top
		WHERE kind = ? AND snapshot_id = (
			SELECT MAX(snapshot_id) FROM traffic_top WHERE kind = ?)
		ORDER BY count DESC`, kind, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TopEntry
	for rows.Next() {
		var e TopEntry
		if err := rows.Scan(&e.Name, &e.Title, &e.Count, &e.Uniques); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// RepoPoint is the repository's counters at one snapshot.
type RepoPoint struct {
	TakenAt  time.Time
	Stars    int64
	Forks    int64
	Watchers int64
}

// RepoHistory returns repository counters over time.
func (db *DB) RepoHistory() ([]RepoPoint, error) {
	rows, err := db.Query(`
		SELECT s.taken_at, r.stars, r.forks, r.watchers
		FROM repo_stat r JOIN snapshot s ON s.id = r.snapshot_id
		ORDER BY s.taken_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RepoPoint
	for rows.Next() {
		var p RepoPoint
		var takenAt string
		if err := rows.Scan(&takenAt, &p.Stars, &p.Forks, &p.Watchers); err != nil {
			return nil, err
		}
		p.TakenAt, _ = time.Parse(time.RFC3339, takenAt)
		out = append(out, p)
	}
	return out, rows.Err()
}

// StarsByDay returns cumulative star counts per day from the backfilled star
// history. Empty until `backfill-stars` has run.
func (db *DB) StarsByDay() ([]TrafficPoint, error) {
	rows, err := db.Query(`
		SELECT substr(starred_at, 1, 10) AS day, COUNT(*)
		FROM stargazer GROUP BY day ORDER BY day`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TrafficPoint
	var running int64
	for rows.Next() {
		var p TrafficPoint
		if err := rows.Scan(&p.Day, &p.Count); err != nil {
			return nil, err
		}
		running += p.Count
		p.Uniques = running // cumulative total alongside the daily delta
		out = append(out, p)
	}
	return out, rows.Err()
}
