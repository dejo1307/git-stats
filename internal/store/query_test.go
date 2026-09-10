package store

import (
	"fmt"
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
	// Artifacts alone: counting the checksum per platform would count each
	// scripted install twice.
	if iv.ByPlatform["darwin-arm64"] != 6 {
		t.Errorf("darwin-arm64 delta = %d, want 6", iv.ByPlatform["darwin-arm64"])
	}
	if iv.Checksums != 3 {
		t.Errorf("checksum delta = %d, want 3", iv.Checksums)
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

func TestSplitMix(t *testing.T) {
	tests := []struct {
		name                          string
		artifacts, installs, upgrades int64
		want                          Mix
	}{
		{"every kind of client", 10, 6, 3, Mix{Upgrades: 3, Scripted: 6, Manual: 1}},
		{"no checksums means manual", 4, 0, 0, Mix{Manual: 4}},
		// A mirror or scanner fetching checksums alone must not invent installs.
		{"checksums without artifacts", 0, 5, 2, Mix{}},
		{"install checksums capped by what upgrades left", 5, 5, 3, Mix{Upgrades: 3, Scripted: 2}},
	}
	for _, tt := range tests {
		if got := splitMix(tt.artifacts, tt.installs, tt.upgrades); got != tt.want {
			t.Errorf("%s: splitMix(%d, %d, %d) = %+v, want %+v",
				tt.name, tt.artifacts, tt.installs, tt.upgrades, got, tt.want)
		}
	}
}

// assetJSON is one release asset as the GitHub API reports it.
func assetJSON(id int64, name string, count int64) string {
	return fmt.Sprintf(`{"id":%d,"name":%q,"download_count":%d,"size":1,"created_at":"2026-01-01T00:00:00Z"}`,
		id, name, count)
}

// TestInstallMixPairsWithinReleaseAndPlatform pins the grain of the split.
//
// v0.9.0 has five checksum fetches against one artifact, the shape a scanner
// leaves. Paired across all releases, those spare checksums would turn
// v1.0.0's browser downloads into scripted installs, and the totals below would
// read 11 scripted instead of 7.
func TestInstallMixPairsWithinReleaseAndPlatform(t *testing.T) {
	dir := t.TempDir()
	releases := func(darwinTar, darwinUpgrade int64) string {
		return `[{"tag_name":"v1.0.0","assets":[` +
			assetJSON(1, "widget-1.0.0-darwin-arm64.tar.gz", darwinTar) + "," +
			assetJSON(2, "widget-1.0.0-darwin-arm64.sha256", 6) + "," +
			assetJSON(3, "widget-1.0.0-darwin-arm64.upgrade.sha256", darwinUpgrade) + "," +
			assetJSON(4, "widget-1.0.0-linux-amd64.tar.gz", 4) + "," +
			assetJSON(5, "widget-1.0.0-linux-amd64.sha256", 0) + "," +
			assetJSON(6, "version.json", 20) + `]},` +
			`{"tag_name":"v0.9.0","assets":[` +
			assetJSON(7, "widget-0.9.0-linux-amd64.tar.gz", 1) + "," +
			assetJSON(8, "widget-0.9.0-linux-amd64.sha256", 5) + `]}]`
	}
	snapshotAt(t, dir, day(1), map[string]string{FileReleases: releases(10, 3)})
	// Two upgrades in between, each fetching the artifact and the updater's checksum.
	snapshotAt(t, dir, day(2), map[string]string{FileReleases: releases(12, 5)})

	db := openDB(t, dir)
	totals, err := db.LatestTotals()
	if err != nil {
		t.Fatalf("LatestTotals: %v", err)
	}
	if !totals.PublishesUpgradeChecksum() {
		t.Error("an upgrade checksum is published but was not recognised")
	}
	if want := (Mix{Upgrades: 5, Scripted: 7, Manual: 5}); totals.Mix != want {
		t.Errorf("mix = %+v, want %+v", totals.Mix, want)
	}
	if got := totals.Mix.Total(); got != totals.Archives() || got != 17 {
		t.Errorf("mix covers %d artifacts, Archives = %d, want both 17", got, totals.Archives())
	}
	if got := totals.ByPlatform["darwin-arm64"]; got != 12 {
		t.Errorf("darwin-arm64 = %d, want 12 (artifacts alone, no checksums)", got)
	}
	if want := (Mix{Scripted: 1, Manual: 4}); totals.MixByPlatform["linux-amd64"] != want {
		t.Errorf("linux-amd64 mix = %+v, want %+v", totals.MixByPlatform["linux-amd64"], want)
	}

	var v1 *ReleaseTotal
	for i := range totals.ByRelease {
		if totals.ByRelease[i].Tag == "v1.0.0" {
			v1 = &totals.ByRelease[i]
		}
	}
	if v1 == nil {
		t.Fatal("v1.0.0 missing from ByRelease")
	}
	if v1.Total != 47 || v1.Archives != 16 {
		t.Errorf("v1.0.0 total/archives = %d/%d, want 47/16 (the manifest is a download, not an artifact)",
			v1.Total, v1.Archives)
	}
	if want := (Mix{Upgrades: 5, Scripted: 6, Manual: 5}); v1.Mix != want {
		t.Errorf("v1.0.0 mix = %+v, want %+v", v1.Mix, want)
	}

	intervals, err := db.Intervals()
	if err != nil {
		t.Fatalf("Intervals: %v", err)
	}
	iv := intervals[0]
	if want := (Mix{Upgrades: 2}); iv.Mix != want {
		t.Errorf("interval mix = %+v, want %+v", iv.Mix, want)
	}
	if iv.Checksums != 2 || iv.ByPlatform["darwin-arm64"] != 2 {
		t.Errorf("interval checksums/darwin = %d/%d, want 2/2", iv.Checksums, iv.ByPlatform["darwin-arm64"])
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
