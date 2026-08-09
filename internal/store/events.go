package store

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// EventKind classifies a dated thing that happened to the repository.
type EventKind string

const (
	// EventRelease is a published release.
	EventRelease EventKind = "release"
	// EventFileChange is a commit touching a tracked path, README.md above all.
	EventFileChange EventKind = "file"
)

// Event is one dated occurrence to line up against the acquisition series.
type Event struct {
	At    time.Time
	Kind  EventKind
	Ref   string // release tag, or "path@sha"
	Label string // "v0.3.5" or "README.md"
	// Detail is the release name or the commit subject.
	Detail string
	// Size is the release-notes length for a release and the number of lines
	// changed for a file change. Negative means unknown, which for a file
	// change means the per-commit detail has not been fetched.
	Size int64
	// Prerelease marks a release nobody is offered by default.
	Prerelease bool
}

// Day is the event's UTC date, which is the grain every daily series uses.
func (e Event) Day() time.Time {
	return time.Date(e.At.Year(), e.At.Month(), e.At.Day(), 0, 0, 0, 0, time.UTC)
}

// Events returns releases and tracked-path changes as one chronological
// timeline, oldest first.
//
// Both halves are retroactively complete: a release carries its publication
// date and a commit carries its own, so this timeline reaches back to the
// project's beginning regardless of when collection started. The series it gets
// compared against mostly do not — traffic in particular exists only for days
// captured inside GitHub's 14-day window.
func (db *DB) Events() ([]Event, error) {
	var out []Event

	rows, err := db.Query(`
		SELECT tag, name, published_at, prerelease, body_len
		FROM release ORDER BY published_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var e Event
		var tag, name, at string
		if err := rows.Scan(&tag, &name, &at, &e.Prerelease, &e.Size); err != nil {
			return nil, err
		}
		if e.At, err = time.Parse(time.RFC3339, at); err != nil {
			return nil, fmt.Errorf("parsing release date %q: %w", at, err)
		}
		e.Kind, e.Ref, e.Label, e.Detail = EventRelease, tag, tag, name
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	changes, err := db.Query(`
		SELECT path, sha, committed_at, subject, additions, deletions
		FROM file_change ORDER BY committed_at`)
	if err != nil {
		return nil, err
	}
	defer changes.Close()
	for changes.Next() {
		var e Event
		var path, sha, at string
		var adds, dels *int64
		if err := changes.Scan(&path, &sha, &at, &e.Detail, &adds, &dels); err != nil {
			return nil, err
		}
		if e.At, err = time.Parse(time.RFC3339, at); err != nil {
			return nil, fmt.Errorf("parsing commit date %q: %w", at, err)
		}
		e.Kind, e.Label = EventFileChange, path
		e.Ref = path + "@" + sha
		e.Size = -1
		if adds != nil && dels != nil {
			e.Size = *adds + *dels
		}
		out = append(out, e)
	}
	if err := changes.Err(); err != nil {
		return nil, err
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// Metric names for the daily series.
const (
	MetricStars        = "stars"
	MetricForks        = "forks"
	MetricViews        = "views"
	MetricViewUniques  = "view_uniques"
	MetricClones       = "clones"
	MetricCloneUniques = "clone_uniques"
)

// DayValue is one day's value of one metric.
type DayValue struct {
	Day   time.Time
	Value float64
}

// Series is a daily metric together with the rule for reading a day it does
// not contain, which differs by metric and cannot be guessed from the data.
//
// A star history dates itself, so a day with no row inside the covered range
// genuinely had no stars — absence is a zero. A traffic day with no row was
// never captured, because GitHub had already discarded it — absence is a hole.
// Treating the second like the first is the single easiest way to manufacture a
// drop that never happened, so every consumer goes through Value.
type Series struct {
	Metric string
	Days   []DayValue
	// Complete says an absent day inside [From, To] is a true zero.
	Complete bool
	From, To time.Time
}

// Value returns a day's value and whether that day is known at all.
func (s Series) Value(day time.Time) (float64, bool) {
	day = day.UTC().Truncate(24 * time.Hour)
	// Days are few enough that a scan beats carrying an index around.
	for _, d := range s.Days {
		if d.Day.Equal(day) {
			return d.Value, true
		}
	}
	if s.Complete && !day.Before(s.From) && !day.After(s.To) {
		return 0, true
	}
	return 0, false
}

// Total sums the series, which is only meaningful for count metrics.
func (s Series) Total() float64 {
	var sum float64
	for _, d := range s.Days {
		sum += d.Value
	}
	return sum
}

// DailySeries returns one metric as a daily series.
func (db *DB) DailySeries(metric string) (Series, error) {
	s := Series{Metric: metric}

	var query string
	var args []any
	switch metric {
	case MetricStars:
		query = `SELECT substr(starred_at, 1, 10), COUNT(*) FROM stargazer
		         GROUP BY 1 ORDER BY 1`
		s.Complete = true
	case MetricForks:
		query = `SELECT substr(created_at, 1, 10), COUNT(*) FROM fork
		         GROUP BY 1 ORDER BY 1`
		s.Complete = true
	case MetricViews, MetricViewUniques, MetricClones, MetricCloneUniques:
		column, table := "count", "views"
		if metric == MetricViewUniques || metric == MetricCloneUniques {
			column = "uniques"
		}
		if metric == MetricClones || metric == MetricCloneUniques {
			table = "clones"
		}
		query = `SELECT day, ` + column + ` FROM traffic_day WHERE metric = ? ORDER BY day`
		args = []any{table}
	default:
		return s, fmt.Errorf("unknown metric %q", metric)
	}

	rows, err := db.Query(query, args...)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var day string
		var value float64
		if err := rows.Scan(&day, &value); err != nil {
			return s, err
		}
		parsed, err := time.Parse("2006-01-02", day)
		if err != nil {
			return s, fmt.Errorf("parsing day %q: %w", day, err)
		}
		s.Days = append(s.Days, DayValue{Day: parsed, Value: value})
	}
	if err := rows.Err(); err != nil {
		return s, err
	}
	if len(s.Days) > 0 {
		s.From, s.To = s.Days[0].Day, s.Days[len(s.Days)-1].Day
	}

	// A complete history runs to the moment it was captured, not to its last
	// non-empty day: a week with no new stars is a week of zeroes, and reading
	// it as "not collected" would silently excuse the quietest weeks from every
	// comparison.
	if s.Complete {
		captured, err := db.backfilledAt(metric)
		if err != nil {
			return s, err
		}
		if !captured.IsZero() {
			day := captured.UTC().Truncate(24 * time.Hour)
			if s.From.IsZero() {
				s.From = day
			}
			if day.After(s.To) {
				s.To = day
			}
		}
	}
	return s, nil
}

// backfilledAt returns when a self-dating history was last captured.
func (db *DB) backfilledAt(metric string) (time.Time, error) {
	kind := map[string]string{MetricStars: "stars", MetricForks: "forks"}[metric]
	if kind == "" {
		return time.Time{}, nil
	}
	var at string
	err := db.QueryRow(`SELECT captured_at FROM backfill WHERE kind = ?`, kind).Scan(&at)
	if err != nil {
		// No row means the history has never been fetched; the caller falls
		// back to the range the data itself spans.
		return time.Time{}, nil //nolint:nilerr // absence is not a failure
	}
	return time.Parse(time.RFC3339, at)
}

// Rate is a metric's average over one side of an event window.
type Rate struct {
	// Total is the sum over the days actually observed, never over the days
	// the window nominally covers.
	Total float64
	// Days is how many of the window's days were observed.
	Days int
	// Want is how many days the window asked for.
	Want int
}

// Mean is the daily rate over the observed days.
func (r Rate) Mean() float64 {
	if r.Days == 0 {
		return 0
	}
	return r.Total / float64(r.Days)
}

// Coverage is the fraction of the requested window that was observed.
func (r Rate) Coverage() float64 {
	if r.Want == 0 {
		return 0
	}
	return float64(r.Days) / float64(r.Want)
}

// Impact is one event's before-and-after comparison for one metric.
type Impact struct {
	Event  Event
	Metric string
	Window int
	Before Rate
	After  Rate
	// Confounded lists other events inside either window. With a release every
	// day or two, this is the normal case rather than the exception, and a lift
	// figure that ignores it is attributing the whole neighbourhood's effect to
	// one marker.
	Confounded []string
}

// Change is the difference in daily rate, after minus before.
func (i Impact) Change() float64 { return i.After.Mean() - i.Before.Mean() }

// Ratio is the after rate as a multiple of the before rate. ok is false when
// the before rate is zero, where a multiple is undefined rather than infinite.
func (i Impact) Ratio() (float64, bool) {
	if i.Before.Mean() == 0 {
		return 0, false
	}
	return i.After.Mean() / i.Before.Mean(), true
}

// minCoverage is the fraction of a window that must have been observed before
// a before/after comparison is worth printing. Below it the two sides are
// averaging different numbers of days over different parts of the week, and
// the difference says more about the gaps than about the event.
const minCoverage = 0.6

// Sufficient reports whether both sides saw enough days to be compared.
func (i Impact) Sufficient() bool {
	return i.Before.Days >= 2 && i.After.Days >= 2 &&
		i.Before.Coverage() >= minCoverage && i.After.Coverage() >= minCoverage
}

// EventImpact compares the window days before an event against the window days
// from the event onward.
//
// The event's own day counts as "after": a release published at nine in the
// morning has most of that day to act, and splitting the day would need an
// hourly series that no source here provides.
func EventImpact(s Series, ev Event, all []Event, window int) Impact {
	imp := Impact{Event: ev, Metric: s.Metric, Window: window}
	day := ev.Day()

	for offset := 1; offset <= window; offset++ {
		if v, ok := s.Value(day.AddDate(0, 0, -offset)); ok {
			imp.Before.Total += v
			imp.Before.Days++
		}
		imp.Before.Want++
	}
	for offset := 0; offset < window; offset++ {
		if v, ok := s.Value(day.AddDate(0, 0, offset)); ok {
			imp.After.Total += v
			imp.After.Days++
		}
		imp.After.Want++
	}

	for _, other := range all {
		if other.Ref == ev.Ref {
			continue
		}
		days := int(math.Round(other.Day().Sub(day).Hours() / 24))
		if days <= -window || days >= window {
			continue
		}
		imp.Confounded = append(imp.Confounded,
			fmt.Sprintf("%s (%+dd)", other.Label, days))
	}
	return imp
}

// ConfoundedBy renders the confounding events, capped so a dense release
// history does not print a paragraph per row.
func (i Impact) ConfoundedBy(limit int) string {
	if len(i.Confounded) == 0 {
		return ""
	}
	if len(i.Confounded) <= limit {
		return strings.Join(i.Confounded, ", ")
	}
	return fmt.Sprintf("%s and %d more",
		strings.Join(i.Confounded[:limit], ", "), len(i.Confounded)-limit)
}

// Week is one ISO week of events and acquisition.
//
// When events are only a day or two apart, no single event has a clean window
// and every per-event figure is confounded. Aggregating to the week asks the
// question that does survive that: whether weeks with more shipping and more
// writing are weeks with more arrivals.
type Week struct {
	Start       time.Time
	Releases    int
	FileChanges int
	Metrics     map[string]float64
	// Observed says, per metric, whether any of the week lies inside that
	// metric's coverage. A week outside it has no value at all, which is not
	// the same as a value of zero and must not be rendered as one.
	Observed map[string]bool
	// Partial marks a week that some metric covers only in part, either because
	// collection began mid-week or because the week is still running.
	Partial bool
}

// WeeklyRollup buckets events and the given metrics into ISO weeks spanning
// every week that has either an event or an observation.
func (db *DB) WeeklyRollup(metrics []string) ([]Week, error) {
	events, err := db.Events()
	if err != nil {
		return nil, err
	}
	series := make([]Series, 0, len(metrics))
	for _, m := range metrics {
		s, err := db.DailySeries(m)
		if err != nil {
			return nil, err
		}
		series = append(series, s)
	}

	weeks := map[time.Time]*Week{}
	at := func(day time.Time) *Week {
		start := weekStart(day)
		w, ok := weeks[start]
		if !ok {
			w = &Week{Start: start,
				Metrics: map[string]float64{}, Observed: map[string]bool{}}
			weeks[start] = w
		}
		return w
	}

	for _, e := range events {
		w := at(e.Day())
		if e.Kind == EventRelease {
			w.Releases++
		} else {
			w.FileChanges++
		}
	}
	for _, s := range series {
		for _, d := range s.Days {
			at(d.Day).Metrics[s.Metric] += d.Value
		}
	}

	// A week with nothing in it still happened. Leaving it out would print a
	// table whose rows read as consecutive weeks while silently skipping the
	// quiet ones, which is exactly the shape that makes a slow period look like
	// a steady one.
	if len(weeks) > 0 {
		first, last := weekStart(time.Now()), time.Time{}
		for start := range weeks {
			if start.Before(first) {
				first = start
			}
			if start.After(last) {
				last = start
			}
		}
		for w := first; !w.After(last); w = w.AddDate(0, 0, 7) {
			at(w)
		}
	}

	for _, s := range series {
		if s.From.IsZero() {
			continue // a series with no coverage is not evidence of anything
		}
		for _, w := range weeks {
			end := w.Start.AddDate(0, 0, 6)
			w.Observed[s.Metric] = !end.Before(s.From) && !w.Start.After(s.To)
			if w.Observed[s.Metric] && (w.Start.Before(s.From) || end.After(s.To)) {
				w.Partial = true
			}
		}
	}

	out := make([]Week, 0, len(weeks))
	for _, w := range weeks {
		out = append(out, *w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out, nil
}

// weekStart returns the Monday of a day's ISO week, in UTC.
func weekStart(day time.Time) time.Time {
	day = day.UTC().Truncate(24 * time.Hour)
	offset := (int(day.Weekday()) + 6) % 7 // Monday = 0
	return day.AddDate(0, 0, -offset)
}
