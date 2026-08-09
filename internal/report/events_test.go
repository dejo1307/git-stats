package report

import (
	"strings"
	"testing"
	"time"

	"github.com/dejo1307/git-stats/internal/store"
)

func at(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestMarksOutsideThePlottedPeriodAreDropped(t *testing.T) {
	pts := []Point{{T: at("2026-03-10"), V: 1}, {T: at("2026-03-20"), V: 2}}
	marks := []Mark{
		{T: at("2026-01-01"), Label: "v0", ColorVar: releaseColor},
		{T: at("2026-03-15"), Label: "v1", ColorVar: releaseColor},
		{T: at("2026-06-01"), Label: "v2", ColorVar: releaseColor},
	}

	svg := string(MarkedLineChart(pts, "--series-1", "downloads", marks))
	// A release from before the first snapshot has no place on a chart of what
	// happened after it, and clamping it to the edge would assert a date it
	// never had.
	if strings.Contains(svg, "v0") || strings.Contains(svg, "v2") {
		t.Error("a mark outside the plotted period was drawn")
	}
	if !strings.Contains(svg, "v1") {
		t.Error("the mark inside the plotted period is missing")
	}
}

func TestCrowdedMarksMergeIntoOne(t *testing.T) {
	pts := []Point{{T: at("2026-01-01"), V: 1}, {T: at("2026-12-31"), V: 2}}
	// Three releases on consecutive days across a year of chart: under two
	// pixels apart, so drawing all three would be one smudge claiming to be
	// three markers.
	marks := []Mark{
		{T: at("2026-04-01"), Label: "v1", ColorVar: releaseColor},
		{T: at("2026-04-02"), Label: "v2", ColorVar: releaseColor},
		{T: at("2026-04-03"), Label: "v3", ColorVar: releaseColor},
	}

	svg := string(MarkedLineChart(pts, "--series-1", "downloads", marks))
	if strings.Count(svg, "mark-flag") != 1 {
		t.Errorf("got %d flags, want the crowded marks merged into 1",
			strings.Count(svg, "mark-flag"))
	}
	if !strings.Contains(svg, "3 events") {
		t.Error("the merged mark does not say how many events it stands for")
	}
	if !strings.Contains(svg, "v1, v2, v3") {
		t.Error("the merged mark does not name its members")
	}
}

func TestMarksOfDifferentKindsStaySeparate(t *testing.T) {
	pts := []Point{{T: at("2026-03-01"), V: 1}, {T: at("2026-06-01"), V: 2}}
	// A release and a README change on the same day are two different claims
	// about why a number moved and must not be merged into one marker.
	marks := []Mark{
		{T: at("2026-04-01"), Label: "v1", ColorVar: releaseColor},
		{T: at("2026-04-01"), Label: "README.md", ColorVar: fileColor},
	}

	svg := string(MarkedLineChart(pts, "--series-1", "downloads", marks))
	if strings.Count(svg, "mark-flag") != 2 {
		t.Errorf("got %d flags, want 2 — one per kind", strings.Count(svg, "mark-flag"))
	}
}

func TestImpactCellWithholdsUnsupportableNumbers(t *testing.T) {
	series := store.Series{Metric: store.MetricViews,
		From: at("2026-03-09"), To: at("2026-03-10"),
		Days: []store.DayValue{{Day: at("2026-03-10"), Value: 50}}}
	ev := store.Event{At: at("2026-03-10"), Ref: "v1", Label: "v1"}

	got := impactCell(store.EventImpact(series, ev, []store.Event{ev}, 7))
	if !strings.HasPrefix(got, "insufficient data") {
		t.Errorf("impactCell = %q, want a refusal naming the coverage", got)
	}
	if !strings.Contains(got, "days before") {
		t.Errorf("impactCell = %q, want it to say how many days were observed", got)
	}
}

func TestImpactCellReportsARatio(t *testing.T) {
	series := store.Series{Metric: store.MetricStars, Complete: true,
		From: at("2026-03-01"), To: at("2026-03-20"),
		Days: []store.DayValue{
			{Day: at("2026-03-05"), Value: 7},
			{Day: at("2026-03-10"), Value: 14},
		}}
	ev := store.Event{At: at("2026-03-10"), Ref: "v1", Label: "v1"}

	got := impactCell(store.EventImpact(series, ev, []store.Event{ev}, 7))
	if !strings.Contains(got, "1.00 → 2.00 /day") {
		t.Errorf("impactCell = %q, want both daily rates", got)
	}
	if !strings.Contains(got, "×2.0") {
		t.Errorf("impactCell = %q, want the multiple", got)
	}
}

func TestEventSizeNamesWhatIsUnknown(t *testing.T) {
	unknown := store.Event{Kind: store.EventFileChange, Size: -1}
	if got := eventSize(unknown); got != "size not fetched" {
		t.Errorf("eventSize(unknown) = %q, want it distinguished from an empty change", got)
	}
	empty := store.Event{Kind: store.EventRelease, Size: 0}
	if got := eventSize(empty); got != "no notes" {
		t.Errorf("eventSize(unannounced release) = %q", got)
	}
}

func TestWithdrawnNoteOnlyFiresOnARealGap(t *testing.T) {
	// The stargazer list holds only current stars, so a repository that had 78
	// stars when it was first snapshotted but whose rebuilt history reaches
	// only 70 lost 8 to unstars — invisible in every chart otherwise.
	got := withdrawnNote(78, 70, at("2026-08-04"))
	if !strings.Contains(got, "8 missing") {
		t.Errorf("withdrawnNote = %q, want it to name the gap", got)
	}
	if got := withdrawnNote(78, 78, at("2026-08-04")); got != "" {
		t.Errorf("withdrawnNote with no gap = %q, want silence", got)
	}
	// A history that reaches further than the counter did is not a negative
	// number of unstars; it is nothing worth saying.
	if got := withdrawnNote(78, 80, at("2026-08-04")); got != "" {
		t.Errorf("withdrawnNote with a surplus = %q, want silence", got)
	}
}

func TestDenseMarksDropTheVerticalsAndKeepTheFlags(t *testing.T) {
	pts := []Point{{T: at("2026-01-01"), V: 1}, {T: at("2026-04-10"), V: 2}}
	// A release every day for a hundred days: the dashed verticals would be
	// hatching across the whole plot and the series would be behind them.
	var marks []Mark
	for i := 0; i < 100; i++ {
		marks = append(marks, Mark{
			T:     at("2026-01-01").AddDate(0, 0, i),
			Label: "v" + string(rune('a'+i%26)), ColorVar: releaseColor,
		})
	}

	dense := string(MarkedLineChart(pts, "--series-1", "downloads", marks))
	if strings.Contains(dense, `class="mark"`) {
		t.Error("dense markers still drew verticals across the plot")
	}
	if !strings.Contains(dense, "mark-flag") {
		t.Error("dense markers dropped the flags too — the events vanished entirely")
	}

	// Sparse markers keep them: that is where a vertical earns its place.
	sparse := string(MarkedLineChart(pts, "--series-1", "downloads", marks[:3]))
	if !strings.Contains(sparse, `class="mark"`) {
		t.Error("sparse markers lost their verticals")
	}
}
