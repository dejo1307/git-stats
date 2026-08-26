package store

import (
	"testing"
	"time"
)

func day(n int) time.Time { return time.Date(2026, 8, n, 9, 0, 0, 0, time.UTC) }

func TestIntervalsDeltaSemantics(t *testing.T) {
	dir := t.TempDir()

	// Baseline: asset 1 already has 100 downloads of all-time history.
	snapshotAt(t, dir, day(1), map[string]string{
		FileReleases: releasesJSON("v1.0.0", map[int64]int64{1: 100}),
	})
	// Two days later it has grown by 10, and a newly published asset 3
	// appears with 7 downloads of its own.
	snapshotAt(t, dir, day(3), map[string]string{
		FileReleases: releasesJSON("v1.0.0", map[int64]int64{1: 110, 3: 7}),
	})

	db := openDB(t, dir)
	intervals, err := db.Intervals()
	if err != nil {
		t.Fatalf("Intervals: %v", err)
	}
	if len(intervals) != 1 {
		t.Fatalf("got %d intervals, want 1 (the first snapshot is a baseline)", len(intervals))
	}

	iv := intervals[0]
	// 100 is baseline and must not be attributed to the interval; 10 of growth
	// plus the whole 7 of the asset created inside the interval must be.
	if iv.Total != 17 {
		t.Errorf("interval total = %d, want 17 (10 growth + 7 from a new asset)", iv.Total)
	}
	if iv.Days != 2 {
		t.Errorf("interval days = %v, want 2", iv.Days)
	}
	if got := iv.PerDay(); got != 8.5 {
		t.Errorf("PerDay = %v, want 8.5", got)
	}
}

func TestIntervalsClampDecreases(t *testing.T) {
	dir := t.TempDir()
	snapshotAt(t, dir, day(1), map[string]string{
		FileReleases: releasesJSON("v1.0.0", map[int64]int64{1: 100}),
	})
	// A counter going backwards means the asset was replaced or its release
	// deleted — never that downloads were returned. It must not subtract.
	snapshotAt(t, dir, day(2), map[string]string{
		FileReleases: releasesJSON("v1.0.0", map[int64]int64{1: 40}),
	})

	db := openDB(t, dir)
	intervals, err := db.Intervals()
	if err != nil {
		t.Fatalf("Intervals: %v", err)
	}
	if intervals[0].Total != 0 {
		t.Errorf("interval total = %d, want 0 (a decrease contributes nothing)", intervals[0].Total)
	}
}

func TestIntervalsNeedTwoSnapshots(t *testing.T) {
	dir := t.TempDir()
	snapshotAt(t, dir, day(1), map[string]string{
		FileReleases: releasesJSON("v1.0.0", map[int64]int64{1: 100}),
	})

	db := openDB(t, dir)
	intervals, err := db.Intervals()
	if err != nil {
		t.Fatalf("Intervals: %v", err)
	}
	if len(intervals) != 0 {
		t.Errorf("got %d intervals from a single snapshot, want 0", len(intervals))
	}

	totals, err := db.LatestTotals()
	if err != nil {
		t.Fatalf("LatestTotals: %v", err)
	}
	if totals.Total != 100 {
		t.Errorf("all-time total = %d, want 100", totals.Total)
	}
}

func TestIntervalsSplitByPlatformAndKind(t *testing.T) {
	dir := t.TempDir()
	releases := func(tarCount, sumCount int64) string {
		return `[{"tag_name":"v1.0.0","assets":[
			{"id":1,"name":"widget-1.0.0-darwin-arm64.tar.gz","download_count":` + itoa(tarCount) + `,"size":1,"created_at":"2026-01-01T00:00:00Z"},
			{"id":2,"name":"widget-1.0.0-darwin-arm64.sha256","download_count":` + itoa(sumCount) + `,"size":1,"created_at":"2026-01-01T00:00:00Z"},
			{"id":3,"name":"widget-1.0.0-linux-amd64.tar.gz","download_count":0,"size":1,"created_at":"2026-01-01T00:00:00Z"}]}]`
	}
	snapshotAt(t, dir, day(1), map[string]string{FileReleases: releases(10, 10)})
	snapshotAt(t, dir, day(2), map[string]string{FileReleases: releases(16, 13)})

	db := openDB(t, dir)
	intervals, err := db.Intervals()
	if err != nil {
		t.Fatalf("Intervals: %v", err)
	}
	iv := intervals[0]
	if iv.ByPlatform["darwin-arm64"] != 9 {
		t.Errorf("darwin-arm64 delta = %d, want 9", iv.ByPlatform["darwin-arm64"])
	}
	if iv.ByPlatform["linux-amd64"] != 0 {
		t.Errorf("linux-amd64 delta = %d, want 0", iv.ByPlatform["linux-amd64"])
	}
	// 6 tarball fetches against 3 checksum fetches: the gap is what the
	// install-mix estimate is built on.
	if iv.ByKind["tar.gz"] != 6 || iv.ByKind["sha256"] != 3 {
		t.Errorf("kind split = tar.gz %d / sha256 %d, want 6 / 3",
			iv.ByKind["tar.gz"], iv.ByKind["sha256"])
	}
}

func TestNonInstallAssetsAreNotPlatforms(t *testing.T) {
	dir := t.TempDir()
	releases := func(binary, manifest int64) string {
		return `[{"tag_name":"v1.0.0","assets":[
			{"id":1,"name":"widget-1.0.0-darwin-arm64.tar.gz","download_count":` + itoa(binary) + `,"size":1,"created_at":"2026-01-01T00:00:00Z"},
			{"id":2,"name":"version.json","download_count":` + itoa(manifest) + `,"size":1,"created_at":"2026-01-01T00:00:00Z"}]}]`
	}
	snapshotAt(t, dir, day(1), map[string]string{FileReleases: releases(10, 5)})
	snapshotAt(t, dir, day(2), map[string]string{FileReleases: releases(16, 9)})

	db := openDB(t, dir)
	intervals, err := db.Intervals()
	if err != nil {
		t.Fatalf("Intervals: %v", err)
	}
	iv := intervals[0]
	if iv.ByPlatform["darwin-arm64"] != 6 {
		t.Errorf("darwin-arm64 delta = %d, want 6", iv.ByPlatform["darwin-arm64"])
	}
	if _, ok := iv.ByPlatform["unknown"]; ok {
		t.Errorf("a non-install asset was charted as a platform: %v", iv.ByPlatform)
	}
	if iv.Other != 4 {
		t.Errorf("other delta = %d, want 4 (the manifest's growth)", iv.Other)
	}
	if iv.Total != 10 {
		t.Errorf("interval total = %d, want 10 (6 binary + 4 manifest)", iv.Total)
	}

	totals, err := db.LatestTotals()
	if err != nil {
		t.Fatalf("LatestTotals: %v", err)
	}
	if totals.ByPlatform["darwin-arm64"] != 16 {
		t.Errorf("darwin-arm64 total = %d, want 16", totals.ByPlatform["darwin-arm64"])
	}
	if _, ok := totals.ByPlatform["unknown"]; ok {
		t.Errorf("a non-install asset was charted as a platform: %v", totals.ByPlatform)
	}
	if totals.Other != 9 {
		t.Errorf("other total = %d, want 9", totals.Other)
	}
	if totals.Total != 25 {
		t.Errorf("all-time total = %d, want 25 (manifests are downloads too)", totals.Total)
	}
	if got := totals.Archives(); got != 16 {
		t.Errorf("Archives = %d, want 16 (a manifest is not an install)", got)
	}
}

func TestFailedSnapshotIsNotTreatedAsZero(t *testing.T) {
	dir := t.TempDir()
	snapshotAt(t, dir, day(1), map[string]string{
		FileReleases: releasesJSON("v1.0.0", map[int64]int64{1: 100}),
	})
	// A run that died before fetching releases: a snapshot row with no
	// counters. It must not read as "downloads dropped to zero".
	snapshotAt(t, dir, day(2), map[string]string{
		FileRepo: `{"stargazers_count":78,"forks_count":11,"subscribers_count":2}`,
	})
	snapshotAt(t, dir, day(3), map[string]string{
		FileReleases: releasesJSON("v1.0.0", map[int64]int64{1: 112}),
	})

	db := openDB(t, dir)

	all, err := db.Snapshots()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("snapshot rows = %d, want 3 (the failed run is still recorded)", len(all))
	}

	observed, err := db.ObservedSnapshots()
	if err != nil {
		t.Fatal(err)
	}
	if len(observed) != 2 {
		t.Errorf("observed snapshots = %d, want 2 (the empty one is not an observation)", len(observed))
	}

	// The cumulative series must not dip through zero.
	points, err := db.Cumulative()
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 2 {
		t.Fatalf("cumulative points = %d, want 2", len(points))
	}
	for _, p := range points {
		if p.Total == 0 {
			t.Errorf("cumulative series contains a zero at %s — an unobserved snapshot "+
				"was charted as zero downloads", p.TakenAt)
		}
	}

	// All-time totals must come from the last snapshot that saw releases.
	totals, err := db.LatestTotals()
	if err != nil {
		t.Fatal(err)
	}
	if totals.Total != 112 {
		t.Errorf("all-time total = %d, want 112 (from the last observed snapshot)", totals.Total)
	}

	// One interval, spanning the two observations and skipping the gap.
	intervals, err := db.Intervals()
	if err != nil {
		t.Fatal(err)
	}
	if len(intervals) != 1 {
		t.Fatalf("intervals = %d, want 1", len(intervals))
	}
	if intervals[0].Total != 12 {
		t.Errorf("interval total = %d, want 12", intervals[0].Total)
	}
	if intervals[0].Days != 2 {
		t.Errorf("interval days = %v, want 2 (day 1 to day 3)", intervals[0].Days)
	}
}

func TestCumulativeTracksEverySnapshot(t *testing.T) {
	dir := t.TempDir()
	snapshotAt(t, dir, day(1), map[string]string{
		FileReleases: releasesJSON("v1.0.0", map[int64]int64{1: 100}),
	})
	snapshotAt(t, dir, day(4), map[string]string{
		FileReleases: releasesJSON("v1.0.0", map[int64]int64{1: 130}),
	})

	db := openDB(t, dir)
	points, err := db.Cumulative()
	if err != nil {
		t.Fatalf("Cumulative: %v", err)
	}
	if len(points) != 2 || points[0].Total != 100 || points[1].Total != 130 {
		t.Errorf("cumulative series = %+v, want totals 100 then 130", points)
	}
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf []byte
	for v > 0 {
		buf = append([]byte{byte('0' + v%10)}, buf...)
		v /= 10
	}
	return string(buf)
}
