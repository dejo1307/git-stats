package report

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/dejo1307/git-stats/internal/store"
)

// impactWindow is the number of days compared on each side of an event.
//
// Seven days is chosen so both sides span the same weekdays: traffic to a
// developer tool collapses at the weekend, and a window of any other length
// compares a different mix of days before and after, which shows up as a lift
// that is really just the calendar.
const impactWindow = 7

// timelineRows caps how many events the dashboard table lists, newest first.
const timelineRows = 25

// Colours by event kind. Releases and prose changes are different claims about
// why a number moved, so they must not share a colour.
const (
	releaseColor = "--series-4"
	fileColor    = "--series-2"
)

// marksFor turns the event timeline into chart annotations.
func marksFor(events []store.Event) []Mark {
	marks := make([]Mark, 0, len(events))
	for _, e := range events {
		color := releaseColor
		if e.Kind == store.EventFileChange {
			color = fileColor
		}
		marks = append(marks, Mark{
			T: e.At, Label: e.Label, Detail: e.Detail, ColorVar: color,
		})
	}
	return marks
}

// eventSize describes how big an event was in the terms of its own kind.
func eventSize(e store.Event) string {
	if e.Kind == store.EventRelease {
		if e.Size == 0 {
			return "no notes"
		}
		return fmt.Sprintf("%d chars of notes", e.Size)
	}
	if e.Size < 0 {
		return "size not fetched"
	}
	return fmt.Sprintf("%d lines changed", e.Size)
}

// rate renders one side of a before/after comparison.
func rate(r store.Rate) string {
	if r.Days == 0 {
		return "—"
	}
	return fmt.Sprintf("%.2f", r.Mean())
}

// impactCell renders a comparison, or says why there is nothing to render.
// A number withheld with its reason beats a number that cannot carry weight.
func impactCell(imp store.Impact) string {
	if !imp.Sufficient() {
		return fmt.Sprintf("insufficient data (%d/%d days before, %d/%d after)",
			imp.Before.Days, imp.Before.Want, imp.After.Days, imp.After.Want)
	}
	text := fmt.Sprintf("%s → %s /day", rate(imp.Before), rate(imp.After))
	if ratio, ok := imp.Ratio(); ok {
		text += fmt.Sprintf(" (×%.1f)", ratio)
	} else if imp.After.Mean() > 0 {
		text += " (from nothing)"
	}
	return text
}

// buildEvents fills in everything derived from the event timeline and returns
// the chart annotations.
func buildEvents(d *htmlData, db *store.DB) ([]Mark, error) {
	events, err := db.Events()
	if err != nil {
		return nil, err
	}
	if len(events) == 0 {
		return nil, nil
	}
	d.HasEvents = true

	var releases, files int
	paths := map[string]bool{}
	for _, e := range events {
		if e.Kind == store.EventRelease {
			releases++
			continue
		}
		files++
		paths[e.Label] = true
	}
	d.HasFileChanges = files > 0
	d.EventsCounted = fmt.Sprintf("%d releases", releases)
	if files > 0 {
		d.EventsCounted += fmt.Sprintf(" and %d change(s) to %d tracked path(s)",
			files, len(paths))
		d.TrackedPaths = strings.Join(sortedStrings(paths), ", ")
	}

	stars, err := db.DailySeries(store.MetricStars)
	if err != nil {
		return nil, err
	}
	d.ImpactMetric, d.ImpactWindow = "new stars", impactWindow

	// Newest first: the recent end is the part anyone can still act on.
	ordered := append([]store.Event(nil), events...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].At.After(ordered[j].At) })
	for i, e := range ordered {
		if i >= timelineRows {
			break
		}
		imp := store.EventImpact(stars, e, events, impactWindow)
		kind := "release"
		if e.Kind == store.EventFileChange {
			kind = e.Label
		}
		d.Timeline = append(d.Timeline, row{Cells: []string{
			e.At.Format("2006-01-02"),
			kind,
			truncate(firstNonEmpty(e.Detail, e.Label), 44),
			eventSize(e),
			impactCell(imp),
			truncate(imp.ConfoundedBy(3), 52),
		}})
	}
	if len(ordered) > timelineRows {
		d.TimelineNote = fmt.Sprintf("Showing the %d most recent of %d events.",
			timelineRows, len(ordered))
	}

	if err := buildWeekly(d, db); err != nil {
		return nil, err
	}
	if err := buildWithdrawn(d, db); err != nil {
		return nil, err
	}
	return marksFor(events), nil
}

// weekCell renders one metric for one week, or an em dash when that week lies
// outside the metric's coverage. Printing zero there would turn "GitHub had
// already discarded these days" into "nobody visited".
func weekCell(w store.Week, metric string) string {
	if !w.Observed[metric] {
		return "—"
	}
	return formatNum(w.Metrics[metric])
}

// buildWeekly fills the per-week rollup.
func buildWeekly(d *htmlData, db *store.DB) error {
	metrics := []string{store.MetricStars, store.MetricViews, store.MetricCloneUniques}
	weeks, err := db.WeeklyRollup(metrics)
	if err != nil {
		return err
	}
	for _, w := range weeks {
		label := w.Start.Format("2 Jan 2006")
		if w.Partial {
			label += " *"
		}
		d.Weeks = append(d.Weeks, row{Cells: []string{
			label,
			fmt.Sprintf("%d", w.Releases),
			fmt.Sprintf("%d", w.FileChanges),
			weekCell(w, store.MetricStars),
			weekCell(w, store.MetricViews),
			weekCell(w, store.MetricCloneUniques),
		}})
	}
	if len(weeks) > 0 {
		d.WeeklyNote = "A dash is a week outside that column's coverage — not a zero. " +
			"A week marked * is only partly covered, either because collection began " +
			"mid-week or because the week is still running, so its totals are floors."
	}
	return nil
}

// buildWithdrawn reconciles the backfilled star history against what GitHub
// reported at the time.
//
// The stargazer list contains only people who still star the repository, so a
// history rebuilt from it is the stars that survived, not the stars that
// existed. Comparing it against the counter captured at the earliest snapshot
// recovers the difference, which is otherwise invisible in every chart here.
func buildWithdrawn(d *htmlData, db *store.DB) error {
	points, err := db.RepoHistory()
	if err != nil || len(points) == 0 {
		return err
	}
	days, err := db.StarsByDay()
	if err != nil || len(days) == 0 {
		return err
	}

	first := points[0]
	var surviving int64
	for _, day := range days {
		parsed, err := time.Parse("2006-01-02", day.Day)
		if err != nil || parsed.After(first.TakenAt) {
			continue
		}
		surviving = day.Uniques // running cumulative
	}
	d.Withdrawn = withdrawnNote(first.Stars, surviving, first.TakenAt)
	return nil
}

// withdrawnNote reports the stars GitHub counted at a date that the rebuilt
// history cannot see, and says nothing when the two agree.
func withdrawnNote(counted, surviving int64, at time.Time) string {
	gap := counted - surviving
	if gap <= 0 {
		return ""
	}
	return fmt.Sprintf(
		"GitHub reported %d stars on %s; the surviving star history accounts for %d of them. "+
			"The %d missing were starred before that date and withdrawn since — the stargazer "+
			"list only holds current stars, so every chart here understates the past by "+
			"roughly that much.",
		counted, at.Format("2 Jan 2006"), surviving, gap)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func sortedStrings(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// seriesPoints converts a daily series into chart points.
func seriesPoints(s store.Series) []Point {
	pts := make([]Point, 0, len(s.Days))
	for _, day := range s.Days {
		pts = append(pts, Point{T: day.Day, V: day.Value})
	}
	return pts
}
