package store

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// commitsJSON builds one tracked path's archived history.
func commitsJSON(path string, commits ...[2]string) string {
	items := ""
	for i, c := range commits {
		if i > 0 {
			items += ","
		}
		items += fmt.Sprintf(
			`{"sha":%q,"commit":{"message":%q,"committer":{"date":%q}}}`,
			c[0], "subject for "+c[0], c[1])
	}
	return fmt.Sprintf(`{"path":%q,"commits":[%s]}`, path, items)
}

// starsJSON builds a stargazer payload from a list of dates.
func starsJSON(dates ...string) string {
	items := ""
	for i, d := range dates {
		if i > 0 {
			items += ","
		}
		items += fmt.Sprintf(`{"starred_at":%q,"user":{"login":"u%d"}}`, d, i)
	}
	return "[" + items + "]"
}

func openAt(t *testing.T, dataDir string) *DB {
	t.Helper()
	db, err := Open(DBPath(dataDir))
	if err != nil {
		t.Fatalf("opening db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func onDay(s string) time.Time {
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return d
}

func TestEventsMergesReleasesAndFileChanges(t *testing.T) {
	dir := t.TempDir()
	snapshotAt(t, dir, onDay("2026-03-10"), map[string]string{
		FileReleases: `[
			{"tag_name":"v1.1.0","name":"Spring","published_at":"2026-03-05T10:00:00Z",
			 "body":"notes","assets":[]},
			{"tag_name":"v1.0.0","name":"First","published_at":"2026-03-01T10:00:00Z",
			 "body":"","assets":[]},
			{"tag_name":"v1.2.0","name":"Unfinished","draft":true,"assets":[]}
		]`,
		CommitsFile("README.md"): commitsJSON("README.md",
			[2]string{"sha2", "2026-03-03T12:00:00Z"}),
	})

	events, err := openAt(t, dir).Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	// A draft is invisible to everyone but the maintainer, so it cannot have
	// moved a number and must not appear as a marker.
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3 (the draft excluded): %+v", len(events), events)
	}
	wantOrder := []string{"v1.0.0", "README.md", "v1.1.0"}
	for i, want := range wantOrder {
		if events[i].Label != want {
			t.Errorf("event %d = %q, want %q (chronological)", i, events[i].Label, want)
		}
	}
	if events[0].Size != 0 {
		t.Errorf("release with no notes has Size %d, want 0", events[0].Size)
	}
	if events[1].Size != -1 {
		t.Errorf("file change with no detail fetched has Size %d, want -1 (unknown)",
			events[1].Size)
	}
}

func TestSeriesDistinguishesAZeroDayFromAnUnobservedOne(t *testing.T) {
	dir := t.TempDir()
	snapshotAt(t, dir, onDay("2026-03-10"), map[string]string{
		FileStargazers: starsJSON("2026-03-01T00:00:00Z", "2026-03-05T00:00:00Z"),
		FileViews: `{"count":10,"uniques":5,"views":[
			{"timestamp":"2026-03-01T00:00:00Z","count":6,"uniques":3},
			{"timestamp":"2026-03-05T00:00:00Z","count":4,"uniques":2}]}`,
	})
	db := openAt(t, dir)

	stars, err := db.DailySeries(MetricStars)
	if err != nil {
		t.Fatalf("DailySeries(stars): %v", err)
	}
	// The star history dates itself, so a day inside the covered range with no
	// row genuinely had no stars.
	if v, ok := stars.Value(onDay("2026-03-03")); !ok || v != 0 {
		t.Errorf("quiet star day = (%v, %v), want (0, true)", v, ok)
	}

	views, err := db.DailySeries(MetricViews)
	if err != nil {
		t.Fatalf("DailySeries(views): %v", err)
	}
	// A traffic day with no row was never captured — GitHub had already thrown
	// it away. Reading it as zero would invent a collapse.
	if v, ok := views.Value(onDay("2026-03-03")); ok {
		t.Errorf("uncaptured traffic day = (%v, %v), want ok=false", v, ok)
	}
	if v, ok := views.Value(onDay("2026-03-05")); !ok || v != 4 {
		t.Errorf("captured traffic day = (%v, %v), want (4, true)", v, ok)
	}
}

func TestStarSeriesCoversQuietDaysUpToTheCapture(t *testing.T) {
	dir := t.TempDir()
	// Stars stop on 1 March but the history was captured on the 10th, so the
	// nine days between are known zeroes, not gaps.
	snapshotAt(t, dir, time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC), map[string]string{
		FileStargazers: starsJSON("2026-03-01T00:00:00Z"),
	})

	stars, err := openAt(t, dir).DailySeries(MetricStars)
	if err != nil {
		t.Fatalf("DailySeries: %v", err)
	}
	if v, ok := stars.Value(onDay("2026-03-08")); !ok || v != 0 {
		t.Errorf("day after the last star = (%v, %v), want (0, true)", v, ok)
	}
	if v, ok := stars.Value(onDay("2026-03-20")); ok {
		t.Errorf("day after the capture = (%v, %v), want ok=false", v, ok)
	}
}

func TestEventImpactCountsTheEventDayAsAfter(t *testing.T) {
	series := Series{
		Metric: MetricStars, Complete: true,
		From: onDay("2026-03-01"), To: onDay("2026-03-20"),
		Days: []DayValue{
			{Day: onDay("2026-03-08"), Value: 1}, // before
			{Day: onDay("2026-03-10"), Value: 4}, // the event's own day
			{Day: onDay("2026-03-11"), Value: 6},
		},
	}
	ev := Event{At: time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC), Kind: EventRelease, Ref: "v1"}

	imp := EventImpact(series, ev, []Event{ev}, 7)
	if imp.Before.Total != 1 || imp.Before.Days != 7 {
		t.Errorf("before = %+v, want total 1 over 7 observed days", imp.Before)
	}
	// A release published in the morning has most of that day to act, so its
	// own day belongs to the after side.
	if imp.After.Total != 10 || imp.After.Days != 7 {
		t.Errorf("after = %+v, want total 10 over 7 observed days", imp.After)
	}
	if !imp.Sufficient() {
		t.Error("a fully covered window reported insufficient")
	}
	if got := imp.Change(); got < 1.28 || got > 1.29 {
		t.Errorf("Change() = %v, want ≈1.286 (10/7 − 1/7)", got)
	}
}

func TestEventImpactRefusesToCompareAcrossGaps(t *testing.T) {
	// Traffic captured for two days either side of a fortnight-wide event
	// window: the arithmetic would still produce a number, and that number
	// would describe the gaps rather than the event.
	series := Series{
		Metric: MetricViews,
		From:   onDay("2026-03-09"), To: onDay("2026-03-11"),
		Days: []DayValue{
			{Day: onDay("2026-03-09"), Value: 20},
			{Day: onDay("2026-03-10"), Value: 50},
		},
	}
	ev := Event{At: onDay("2026-03-10"), Kind: EventRelease, Ref: "v1"}

	imp := EventImpact(series, ev, []Event{ev}, 7)
	if imp.Sufficient() {
		t.Errorf("comparison over %d/%d and %d/%d days reported sufficient",
			imp.Before.Days, imp.Before.Want, imp.After.Days, imp.After.Want)
	}
}

func TestEventImpactNamesTheOtherEventsInTheWindow(t *testing.T) {
	series := Series{Metric: MetricStars, Complete: true,
		From: onDay("2026-03-01"), To: onDay("2026-03-20")}
	target := Event{At: onDay("2026-03-10"), Kind: EventRelease, Ref: "v1", Label: "v1"}
	all := []Event{
		{At: onDay("2026-03-02"), Ref: "v0", Label: "v0"}, // outside the window
		{At: onDay("2026-03-08"), Ref: "v0.9", Label: "v0.9"},
		target,
		{At: onDay("2026-03-12"), Ref: "v2", Label: "v2"},
	}

	imp := EventImpact(series, target, all, 7)
	if len(imp.Confounded) != 2 {
		t.Fatalf("confounded = %v, want the two events inside ±7 days", imp.Confounded)
	}
	if imp.Confounded[0] != "v0.9 (-2d)" || imp.Confounded[1] != "v2 (+2d)" {
		t.Errorf("confounded = %v, want signed day offsets", imp.Confounded)
	}
	if got := imp.ConfoundedBy(1); got != "v0.9 (-2d) and 1 more" {
		t.Errorf("ConfoundedBy(1) = %q", got)
	}
}

func TestWeeklyRollupBucketsEventsAndStars(t *testing.T) {
	dir := t.TempDir()
	snapshotAt(t, dir, time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC), map[string]string{
		// 2 and 4 March are the Monday and Wednesday of one ISO week; 9 March
		// is the Monday of the next.
		FileReleases: `[
			{"tag_name":"v1","name":"v1","published_at":"2026-03-02T10:00:00Z","assets":[]},
			{"tag_name":"v2","name":"v2","published_at":"2026-03-04T10:00:00Z","assets":[]},
			{"tag_name":"v3","name":"v3","published_at":"2026-03-09T10:00:00Z","assets":[]}
		]`,
		FileStargazers: starsJSON(
			"2026-03-03T00:00:00Z", "2026-03-05T00:00:00Z", "2026-03-10T00:00:00Z"),
	})

	weeks, err := openAt(t, dir).WeeklyRollup([]string{MetricStars})
	if err != nil {
		t.Fatalf("WeeklyRollup: %v", err)
	}
	if len(weeks) != 2 {
		t.Fatalf("got %d weeks, want 2: %+v", len(weeks), weeks)
	}
	if !weeks[0].Start.Equal(onDay("2026-03-02")) {
		t.Errorf("first week starts %s, want the Monday 2026-03-02", weeks[0].Start)
	}
	if weeks[0].Releases != 2 || weeks[0].Metrics[MetricStars] != 2 {
		t.Errorf("first week = %d releases / %v stars, want 2 and 2",
			weeks[0].Releases, weeks[0].Metrics[MetricStars])
	}
	if weeks[1].Releases != 1 || weeks[1].Metrics[MetricStars] != 1 {
		t.Errorf("second week = %d releases / %v stars, want 1 and 1",
			weeks[1].Releases, weeks[1].Metrics[MetricStars])
	}
}

func TestIngestReadsCommitSizesFromTheSharedCache(t *testing.T) {
	dir := t.TempDir()
	// The detail cache lives beside the snapshots, not inside one, because a
	// commit is immutable and every run can reuse the same fetch.
	cache := filepath.Join(ArchiveDir(dir), "commits")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	err := os.WriteFile(filepath.Join(cache, "sha1.json"),
		[]byte(`{"sha":"sha1","files":[{"filename":"README.md","additions":28,"deletions":9}]}`),
		0o644)
	if err != nil {
		t.Fatal(err)
	}

	snapshotAt(t, dir, onDay("2026-03-10"), map[string]string{
		CommitsFile("README.md"): commitsJSON("README.md",
			[2]string{"sha1", "2026-03-03T12:00:00Z"},
			[2]string{"sha2", "2026-03-04T12:00:00Z"}),
	})

	events, err := openAt(t, dir).Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	if events[0].Size != 37 {
		t.Errorf("cached commit Size = %d, want 37 lines changed", events[0].Size)
	}
	if events[1].Size != -1 {
		t.Errorf("uncached commit Size = %d, want -1 (unknown, not zero)", events[1].Size)
	}
}

func TestReingestKeepsASizeItCanNoLongerSee(t *testing.T) {
	dir := t.TempDir()
	cache := filepath.Join(ArchiveDir(dir), "commits")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	detail := filepath.Join(cache, "sha1.json")
	if err := os.WriteFile(detail,
		[]byte(`{"sha":"sha1","files":[{"filename":"README.md","additions":5,"deletions":5}]}`),
		0o644); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		CommitsFile("README.md"): commitsJSON("README.md",
			[2]string{"sha1", "2026-03-03T12:00:00Z"}),
	}
	snapshotAt(t, dir, onDay("2026-03-10"), files)

	// A pruned cache must degrade to "no new sizes", never to erasing one that
	// was already derived.
	if err := os.Remove(detail); err != nil {
		t.Fatal(err)
	}
	snapshotAt(t, dir, onDay("2026-03-11"), files)

	events, err := openAt(t, dir).Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 1 || events[0].Size != 10 {
		t.Errorf("after re-ingest without the cache, Size = %v, want 10 kept", events)
	}
}

func TestCommitsFileNamesCannotCollide(t *testing.T) {
	if got := CommitsFile("README.md"); got != "commits-README.md.json" {
		t.Errorf("CommitsFile(README.md) = %q, want a readable name", got)
	}
	// "docs/" and "docs_" both sanitise to the same characters; the hash of the
	// original keeps them apart so one path's history cannot overwrite another's.
	if CommitsFile("docs/") == CommitsFile("docs_") {
		t.Errorf("two different paths share the archive file %q", CommitsFile("docs/"))
	}
}
