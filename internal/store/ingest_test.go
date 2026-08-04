package store

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/dejo1307/git-stats/internal/github"
)

// testNamer matches the "widget-…" asset names used throughout these tests.
var testNamer = github.NewAssetNamer("widget")

// snapshotAt archives one set of files at a given time and ingests it.
func snapshotAt(t *testing.T, dataDir string, at time.Time, files map[string]string) {
	t.Helper()

	archive := NewArchive(ArchiveDir(dataDir))
	snap, err := archive.Create(at)
	if err != nil {
		t.Fatalf("creating snapshot: %v", err)
	}
	for name, body := range files {
		if err := snap.Write(name, []byte(body)); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	db, err := Open(DBPath(dataDir))
	if err != nil {
		t.Fatalf("opening db: %v", err)
	}
	defer db.Close()
	if err := Ingest(db, snap, testNamer); err != nil {
		t.Fatalf("ingesting: %v", err)
	}
}

// releasesJSON builds a one-release payload whose assets carry the given
// cumulative counts, keyed by asset id.
func releasesJSON(tag string, counts map[int64]int64) string {
	assets := ""
	i := 0
	for id, count := range counts {
		if i > 0 {
			assets += ","
		}
		i++
		kind := "tar.gz"
		if id%2 == 0 {
			kind = "sha256"
		}
		assets += fmt.Sprintf(
			`{"id":%d,"name":"widget-1.0.%d-linux-amd64.%s","download_count":%d,"size":10,"created_at":"2026-01-01T00:00:00Z"}`,
			id, id, kind, count)
	}
	return fmt.Sprintf(`[{"tag_name":"%s","assets":[%s]}]`, tag, assets)
}

func openDB(t *testing.T, dataDir string) *DB {
	t.Helper()
	db, err := Open(DBPath(dataDir))
	if err != nil {
		t.Fatalf("opening db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestIngestIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	payload := map[string]string{FileReleases: releasesJSON("v1.0.0", map[int64]int64{1: 10, 2: 4})}

	snapshotAt(t, dir, at, payload)
	snapshotAt(t, dir, at, payload) // same timestamp: a re-run, not a new sample

	db := openDB(t, dir)
	var snapshots, assets int
	if err := db.QueryRow(`SELECT COUNT(*) FROM snapshot`).Scan(&snapshots); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM asset_count`).Scan(&assets); err != nil {
		t.Fatal(err)
	}
	if snapshots != 1 {
		t.Errorf("snapshot rows = %d, want 1", snapshots)
	}
	if assets != 2 {
		t.Errorf("asset_count rows = %d, want 2 (re-ingest must replace, not duplicate)", assets)
	}
}

func TestTrafficUpsertKeepsMaximum(t *testing.T) {
	dir := t.TempDir()

	// First capture: 2026-08-01 is still in progress and reads 5.
	snapshotAt(t, dir, time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC), map[string]string{
		FileViews: `{"count":5,"uniques":3,"views":[
			{"timestamp":"2026-08-01T00:00:00Z","count":5,"uniques":3}]}`,
	})
	// Later the same day the bucket has grown; the overlapping window also
	// re-reports it. The larger value must win.
	snapshotAt(t, dir, time.Date(2026, 8, 1, 18, 0, 0, 0, time.UTC), map[string]string{
		FileViews: `{"count":12,"uniques":7,"views":[
			{"timestamp":"2026-08-01T00:00:00Z","count":12,"uniques":7},
			{"timestamp":"2026-08-02T00:00:00Z","count":2,"uniques":1}]}`,
	})
	// A stale re-report of a finalised day must not reduce it.
	snapshotAt(t, dir, time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC), map[string]string{
		FileViews: `{"count":1,"uniques":1,"views":[
			{"timestamp":"2026-08-01T00:00:00Z","count":1,"uniques":1}]}`,
	})

	db := openDB(t, dir)
	points, err := db.Traffic("views")
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 2 {
		t.Fatalf("got %d merged days, want 2", len(points))
	}
	if points[0].Day != "2026-08-01" || points[0].Count != 12 {
		t.Errorf("day one = %s/%d, want 2026-08-01/12", points[0].Day, points[0].Count)
	}
	if points[0].Uniques != 7 {
		t.Errorf("day one uniques = %d, want 7", points[0].Uniques)
	}
	if points[1].Count != 2 {
		t.Errorf("day two = %d, want 2", points[1].Count)
	}
}

func TestWindowUniquesAreNotTheSumOfDailyUniques(t *testing.T) {
	dir := t.TempDir()
	// The same visitor on both days: two daily buckets of 1 unique each, but
	// the window reports 1 distinct visitor. Summing the buckets would say 2.
	snapshotAt(t, dir, time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC), map[string]string{
		FileViews: `{"count":9,"uniques":1,"views":[
			{"timestamp":"2026-08-01T00:00:00Z","count":4,"uniques":1},
			{"timestamp":"2026-08-02T00:00:00Z","count":5,"uniques":1}]}`,
	})

	db := openDB(t, dir)

	window, err := db.LatestTrafficWindow("views")
	if err != nil {
		t.Fatal(err)
	}
	if !window.Found {
		t.Fatal("no window totals recorded")
	}
	if window.Uniques != 1 {
		t.Errorf("window uniques = %d, want 1 (the API's de-duplicated figure)", window.Uniques)
	}
	if window.Count != 9 {
		t.Errorf("window count = %d, want 9", window.Count)
	}

	points, err := db.Traffic("views")
	if err != nil {
		t.Fatal(err)
	}
	var summed int64
	for _, p := range points {
		summed += p.Uniques
	}
	if summed != 2 {
		t.Fatalf("daily uniques sum = %d, want 2 — the fixture no longer exercises the distinction", summed)
	}
	if summed == window.Uniques {
		t.Error("the daily sum and the window figure must not be interchangeable")
	}
}

func TestMissingTrafficLeavesNoWindowRow(t *testing.T) {
	dir := t.TempDir()
	snapshotAt(t, dir, time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC), map[string]string{
		FileReleases: releasesJSON("v1.0.0", map[int64]int64{1: 5}),
	})

	db := openDB(t, dir)
	window, err := db.LatestTrafficWindow("views")
	if err != nil {
		t.Fatalf("LatestTrafficWindow with no traffic captured: %v", err)
	}
	if window.Found {
		t.Error("reported a window when no traffic was collected")
	}
}

func TestRebuildReproducesCollection(t *testing.T) {
	dir := t.TempDir()
	snapshotAt(t, dir, time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC), map[string]string{
		FileReleases: releasesJSON("v1.0.0", map[int64]int64{1: 10, 2: 4}),
		FileRepo:     `{"stargazers_count":78,"forks_count":11,"subscribers_count":2}`,
		FileViews:    `{"views":[{"timestamp":"2026-08-01T00:00:00Z","count":5,"uniques":3}]}`,
	})
	snapshotAt(t, dir, time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC), map[string]string{
		FileReleases: releasesJSON("v1.0.0", map[int64]int64{1: 25, 2: 9}),
	})

	before := dumpState(t, dir)

	n, err := Rebuild(dir, testNamer)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if n != 2 {
		t.Errorf("replayed %d snapshots, want 2", n)
	}

	if after := dumpState(t, dir); after != before {
		t.Errorf("rebuild did not reproduce the database:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// dumpState renders the derived tables that must survive a rebuild. raw_dir is
// excluded because it holds absolute paths.
func dumpState(t *testing.T, dataDir string) string {
	t.Helper()
	db := openDB(t, dataDir)

	var out string
	for _, q := range []string{
		`SELECT taken_at FROM snapshot ORDER BY taken_at`,
		`SELECT snapshot_id, asset_id, asset_name, os, arch, kind, download_count
		   FROM asset_count ORDER BY snapshot_id, asset_id`,
		`SELECT metric, day, count, uniques FROM traffic_day ORDER BY metric, day`,
		`SELECT snapshot_id, metric, count, uniques FROM traffic_window ORDER BY snapshot_id, metric`,
		`SELECT snapshot_id, stars, forks, watchers FROM repo_stat ORDER BY snapshot_id`,
	} {
		rows, err := db.Query(q)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			out += fmt.Sprintf("%v\n", vals)
		}
		rows.Close()
		out += "--\n"
	}
	return out
}

func TestArchiveListIgnoresNonTimestampDirs(t *testing.T) {
	dir := t.TempDir()
	snapshotAt(t, dir, time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC), map[string]string{
		FileReleases: releasesJSON("v1.0.0", map[int64]int64{1: 1}),
	})

	// A stray directory in the archive root must not break a rebuild.
	if err := ensureDir(filepath.Join(ArchiveDir(dir), "notes")); err != nil {
		t.Fatal(err)
	}
	snaps, err := NewArchive(ArchiveDir(dir)).List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(snaps) != 1 {
		t.Errorf("listed %d snapshots, want 1", len(snaps))
	}
}
