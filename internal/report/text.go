// Package report renders collected metrics as a terminal summary or a
// self-contained HTML dashboard.
package report

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/dejo1307/git-stats/internal/store"
)

// Options controls what a report covers.
type Options struct {
	Repo string
	// Since limits interval-based figures to the trailing window. Zero means
	// all recorded history.
	Since time.Duration
	// PerDay normalises interval deltas to a daily rate.
	PerDay bool
	// Now is the reference time for Since; zero means time.Now.
	Now time.Time

	// Contacts adds the stargazer list to the HTML dashboard. Opt-in, because
	// it turns a file about download counts into a file holding other people's
	// names and addresses — and the dashboard is the artefact most likely to be
	// mailed to somebody or dropped in a shared folder.
	Contacts bool
	// MaxContacts caps how many stargazers are embedded. Zero means
	// defaultMaxContacts. The page says when it truncated.
	MaxContacts int
}

// defaultMaxContacts bounds the embedded list. A row costs about 120 bytes of
// JSON — measured, not guessed — so this holds the list itself to a quarter of
// a megabyte on a repository with more stars than anyone is going to read
// through. The cap keeps the newest stars, which are the ones worth asking:
// they starred something they have just seen.
const defaultMaxContacts = 2000

func (o Options) now() time.Time {
	if o.Now.IsZero() {
		return time.Now()
	}
	return o.Now
}

// Text writes a terminal summary.
func Text(w io.Writer, db *store.DB, opts Options) error {
	snaps, err := db.Snapshots()
	if err != nil {
		return err
	}
	if len(snaps) == 0 {
		fmt.Fprintln(w, "No snapshots yet. Run `git-stats collect` first.")
		return nil
	}

	totals, err := db.LatestTotals()
	if err != nil {
		return err
	}

	fmt.Fprintf(w, "%s — download stats\n", opts.Repo)
	fmt.Fprintf(w, "%d snapshot(s), %s … %s\n\n",
		len(snaps),
		snaps[0].TakenAt.Format("2006-01-02 15:04"),
		snaps[len(snaps)-1].TakenAt.Format("2006-01-02 15:04 MST"))

	intervals, err := db.Intervals()
	if err != nil {
		return err
	}
	intervals = filterSince(intervals, opts)
	window := sumIntervals(intervals)

	if err := writePlatforms(w, totals, window, opts); err != nil {
		return err
	}
	writeInstallMix(w, totals, window)
	writeReleases(w, totals)
	if err := writeTraffic(w, db); err != nil {
		return err
	}
	if err := writeRepo(w, db); err != nil {
		return err
	}
	return writeEvents(w, db)
}

// writeEvents lines the acquisition series up against releases and tracked
// file changes, at the week grain the terminal has room for.
func writeEvents(w io.Writer, db *store.DB) error {
	weeks, err := db.WeeklyRollup(
		[]string{store.MetricStars, store.MetricViews, store.MetricCloneUniques})
	if err != nil {
		return err
	}
	if len(weeks) == 0 {
		return nil
	}

	fmt.Fprintln(w, "\nby week")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  week of\treleases\tchanges\tstars\tviews\tcloners")
	// The recent end is what anyone can still act on, and a long project would
	// otherwise scroll the useful rows off the top.
	for _, wk := range lastWeeks(weeks, 10) {
		label := wk.Start.Format("2006-01-02")
		if wk.Partial {
			label += " *"
		}
		fmt.Fprintf(tw, "  %s\t%d\t%d\t%s\t%s\t%s\n",
			label, wk.Releases, wk.FileChanges,
			weekCell(wk, store.MetricStars), weekCell(wk, store.MetricViews),
			weekCell(wk, store.MetricCloneUniques))
	}
	tw.Flush()
	fmt.Fprintln(w, "  — outside that column's coverage, not a zero;")
	fmt.Fprintln(w, "  * only partly covered, so the totals are floors")

	events, err := db.Events()
	if err != nil || len(events) == 0 {
		return err
	}
	stars, err := db.DailySeries(store.MetricStars)
	if err != nil {
		return err
	}

	fmt.Fprintf(w, "\nlast events vs new stars (%dd before → %dd from the event)\n",
		impactWindow, impactWindow)
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	recent := events
	if len(recent) > 8 {
		recent = recent[len(recent)-8:]
	}
	for i := len(recent) - 1; i >= 0; i-- {
		e := recent[i]
		imp := store.EventImpact(stars, e, events, impactWindow)
		note := ""
		if n := len(imp.Confounded); n > 0 {
			note = fmt.Sprintf("  (confounded: %d other event(s) in window)", n)
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s%s\n",
			e.At.Format("2006-01-02"), truncate(e.Label, 24), impactCell(imp), note)
	}
	tw.Flush()
	fmt.Fprintln(w, "  correlation only: no control period, no weekday correction, and no")
	fmt.Fprintln(w, "  sight of whoever linked to the repository that week.")
	return nil
}

func lastWeeks(weeks []store.Week, n int) []store.Week {
	if len(weeks) <= n {
		return weeks
	}
	return weeks[len(weeks)-n:]
}

func filterSince(intervals []store.Interval, opts Options) []store.Interval {
	if opts.Since <= 0 {
		return intervals
	}
	cutoff := opts.now().Add(-opts.Since)
	var out []store.Interval
	for _, iv := range intervals {
		if iv.To.After(cutoff) {
			out = append(out, iv)
		}
	}
	return out
}

// windowSummary is the aggregate of every interval in the reporting window.
type windowSummary struct {
	Total      int64
	Other      int64 // the part of Total that did not come from a platform binary
	Checksums  int64 // the part of Total that verified an artifact
	Days       float64
	From, To   time.Time
	ByPlatform map[string]int64
	Mix        store.Mix
	Series     []int64 // per-interval totals, for the sparkline
	Empty      bool
}

// PerDay normalises the window's total to a daily rate.
func (w windowSummary) PerDay() float64 { return perDay(w.Total, w.Days) }

func sumIntervals(intervals []store.Interval) windowSummary {
	w := windowSummary{ByPlatform: map[string]int64{}, Empty: len(intervals) == 0}
	for i, iv := range intervals {
		if i == 0 {
			w.From = iv.From
		}
		w.To = iv.To
		w.Total += iv.Total
		w.Other += iv.Other
		w.Checksums += iv.Checksums
		w.Mix.Add(iv.Mix)
		w.Days += iv.Days
		for k, v := range iv.ByPlatform {
			w.ByPlatform[k] += v
		}
		w.Series = append(w.Series, iv.Total)
	}
	return w
}

func writePlatforms(w io.Writer, totals store.Totals, win windowSummary, opts Options) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

	showRate := opts.PerDay && rateMeaningful(win.Days)

	header := "platform\tall-time"
	if !win.Empty {
		header += fmt.Sprintf("\tΔ over %s", humanDays(win.Days))
		if showRate {
			header += "\t/day"
		}
	}
	fmt.Fprintln(tw, header)

	platforms := sortedKeys(totals.ByPlatform)
	for _, p := range platforms {
		row := fmt.Sprintf("  %s\t%d", p, totals.ByPlatform[p])
		if !win.Empty {
			row += fmt.Sprintf("\t%+d", win.ByPlatform[p])
			if showRate {
				row += fmt.Sprintf("\t%.1f", perDay(win.ByPlatform[p], win.Days))
			}
		}
		fmt.Fprintln(tw, row)
	}

	if sums := totals.Checksums(); sums > 0 {
		row := fmt.Sprintf("  %s\t%d", "checksums (verify an artifact)", sums)
		if !win.Empty {
			row += fmt.Sprintf("\t%+d", win.Checksums)
			if showRate {
				row += fmt.Sprintf("\t%.1f", perDay(win.Checksums, win.Days))
			}
		}
		fmt.Fprintln(tw, row)
	}

	if totals.Other > 0 {
		row := fmt.Sprintf("  %s\t%d", "other (not installs)", totals.Other)
		if !win.Empty {
			row += fmt.Sprintf("\t%+d", win.Other)
			if showRate {
				row += fmt.Sprintf("\t%.1f", perDay(win.Other, win.Days))
			}
		}
		fmt.Fprintln(tw, row)
	}

	row := fmt.Sprintf("  %s\t%d", "TOTAL", totals.Total)
	if !win.Empty {
		row += fmt.Sprintf("\t%+d", win.Total)
		if showRate {
			row += fmt.Sprintf("\t%.1f", perDay(win.Total, win.Days))
		}
	}
	fmt.Fprintln(tw, row)
	if err := tw.Flush(); err != nil {
		return err
	}

	switch {
	case win.Empty:
		fmt.Fprintln(w, "\n  (no interval yet — all-time counters are a baseline until a second"+
			"\n   snapshot exists. Run `git-stats collect` again later to see movement.)")
	default:
		fmt.Fprintf(w, "\n  window %s → %s%s\n",
			win.From.Format("2006-01-02 15:04"),
			win.To.Format("2006-01-02 15:04"),
			sparkSuffix(win.Series))
		if opts.PerDay && !showRate {
			fmt.Fprintf(w, "  (%s is too short to express as a daily rate)\n",
				humanDays(win.Days))
		}
	}
	fmt.Fprintln(w)
	return nil
}

func sparkSuffix(series []int64) string {
	if len(series) < 2 {
		return ""
	}
	return "  " + Sparkline(series)
}

// writeInstallMix splits artifact downloads by the checksum fetched alongside
// them, paired within each release and platform. It is only meaningful for
// projects that publish a checksum per artifact, and prints nothing for those
// that do not.
func writeInstallMix(w io.Writer, totals store.Totals, win windowSummary) {
	if totals.Checksums() == 0 {
		return
	}
	upgrades := totals.PublishesUpgradeChecksum()
	fmt.Fprintln(w, "install mix (all-time, paired within each release and platform)")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	head := "  platform\tartifacts"
	if upgrades {
		head += "\tupgrades"
	}
	fmt.Fprintln(tw, head+"\tscripted\tmanual")
	for _, p := range sortedKeys(totals.ByPlatform) {
		fmt.Fprintf(tw, "  %s\t%d%s\n", p, totals.ByPlatform[p], mixCols(totals.MixByPlatform[p], upgrades))
	}
	fmt.Fprintf(tw, "  %s\t%d%s\n", "TOTAL", totals.Archives(), mixCols(totals.Mix, upgrades))
	tw.Flush()
	if !win.Empty {
		line := fmt.Sprintf("+%d scripted, +%d manual", win.Mix.Scripted, win.Mix.Manual)
		if upgrades {
			line = fmt.Sprintf("+%d upgrades, ", win.Mix.Upgrades) + line
		}
		fmt.Fprintf(w, "  over the window: %s\n", line)
	}
	// Each client fetches a fixed set of files, and that is all the split reads.
	if upgrades {
		fmt.Fprintln(w, "  upgrades: artifact with the self-updater's own checksum")
	}
	fmt.Fprintln(w, "  scripted: artifact with the install checksum (install scripts, wrappers,")
	fmt.Fprintln(w, "            and self-updaters older than their own checksum)")
	fmt.Fprintln(w, "  manual:   artifact alone, as a browser or a plain curl fetches it")
	fmt.Fprintln(w)
}

// mixCols renders a mix as trailing tab-separated columns, with an upgrades
// column only when the releases publish the self-updater's own checksum.
func mixCols(m store.Mix, upgrades bool) string {
	cols := ""
	if upgrades {
		cols = fmt.Sprintf("\t%d", m.Upgrades)
	}
	return cols + fmt.Sprintf("\t%d\t%d", m.Scripted, m.Manual)
}

func writeReleases(w io.Writer, totals store.Totals) {
	if len(totals.ByRelease) == 0 {
		return
	}
	byTotal := append([]store.ReleaseTotal(nil), totals.ByRelease...)
	sort.Slice(byTotal, func(i, j int) bool { return byTotal[i].Total > byTotal[j].Total })

	hasMix, upgrades := totals.Checksums() > 0, totals.PublishesUpgradeChecksum()
	fmt.Fprintln(w, "top releases (all-time)")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	head := "  release\tpublished\tdownloads\tartifacts"
	if hasMix {
		if upgrades {
			head += "\tupgrades"
		}
		head += "\tscripted\tmanual"
	}
	fmt.Fprintln(tw, head)
	for i, r := range byTotal {
		if i >= 10 {
			break
		}
		mix := ""
		if hasMix {
			mix = mixCols(r.Mix, upgrades)
		}
		fmt.Fprintf(tw, "  %s\t%s\t%d\t%d%s\n",
			r.Tag, r.PublishedAt.Format("2006-01-02"), r.Total, r.Archives, mix)
	}
	tw.Flush()
	if len(byTotal) > 10 {
		fmt.Fprintf(w, "  … and %d more releases\n", len(byTotal)-10)
	}
	fmt.Fprintln(w)
}

func writeTraffic(w io.Writer, db *store.DB) error {
	var wrote bool
	for _, metric := range []string{"views", "clones"} {
		points, err := db.Traffic(metric)
		if err != nil {
			return err
		}
		if len(points) == 0 {
			continue
		}
		if !wrote {
			fmt.Fprintln(w, "traffic (daily buckets merged across snapshots)")
			wrote = true
		}
		var total int64
		series := make([]int64, 0, len(points))
		for _, p := range points {
			total += p.Count
			series = append(series, p.Count)
		}

		// Uniques come from the API's window total, never from summing the
		// daily buckets — a returning visitor appears in every day they were
		// active, so that sum is not a visitor count.
		window, err := db.LatestTrafficWindow(metric)
		if err != nil {
			return err
		}
		unique := ""
		if window.Found {
			unique = fmt.Sprintf(", %d unique in the last window", window.Uniques)
		}
		fmt.Fprintf(w, "  %-7s %5d over %d day(s)%s  %s\n",
			metric, total, len(points), unique, Sparkline(series))
	}

	if !wrote {
		fmt.Fprintln(w, "traffic: none recorded "+
			"(needs a classic token with the public_repo scope)")
		fmt.Fprintln(w)
		return nil
	}
	// Clones and asset downloads count disjoint distribution paths, and the
	// two are routinely compared as if one should bound the other.
	fmt.Fprintln(w, "  note: clones and release downloads do not overlap — cloning and")
	fmt.Fprintln(w, "        source-level package managers add to clones but fetch no release")
	fmt.Fprintln(w, "        asset, while install scripts and browser downloads never clone.")
	fmt.Fprintln(w, "        Clones are a weak proxy for source installs in both directions:")
	fmt.Fprintln(w, "        a module or package proxy caches each version globally, so many")
	fmt.Fprintln(w, "        installs can produce one clone, while CI, mirrors and scanners")
	fmt.Fprintln(w, "        inflate the same number.")

	for _, kind := range []struct{ key, label string }{{"referrer", "top referrers"}, {"path", "top paths"}} {
		entries, err := db.LatestTop(kind.key)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n  %s (last 14d)\n", kind.label)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for i, e := range entries {
			if i >= 5 {
				break
			}
			fmt.Fprintf(tw, "    %s\t%d\t(%d unique)\n", truncate(e.Name, 48), e.Count, e.Uniques)
		}
		tw.Flush()
	}
	fmt.Fprintln(w)
	return nil
}

func writeRepo(w io.Writer, db *store.DB) error {
	points, err := db.RepoHistory()
	if err != nil || len(points) == 0 {
		return err
	}
	latest := points[len(points)-1]
	line := fmt.Sprintf("repo: %d stars, %d forks, %d watchers",
		latest.Stars, latest.Forks, latest.Watchers)
	if len(points) > 1 {
		first := points[0]
		line += fmt.Sprintf("  (%+d stars since %s)",
			latest.Stars-first.Stars, first.TakenAt.Format("2006-01-02"))
	}
	fmt.Fprintln(w, line)
	return nil
}

// sparkChars ascends from lowest to highest bucket.
var sparkChars = []rune("▁▂▃▄▅▆▇█")

// Sparkline renders values as a unicode bar strip, scaled to the series max.
// An all-zero series renders as flat lows rather than blank.
func Sparkline(values []int64) string {
	if len(values) == 0 {
		return ""
	}
	var maxV int64
	for _, v := range values {
		if v > maxV {
			maxV = v
		}
	}
	var b strings.Builder
	for _, v := range values {
		if maxV <= 0 {
			b.WriteRune(sparkChars[0])
			continue
		}
		idx := int(v * int64(len(sparkChars)-1) / maxV)
		b.WriteRune(sparkChars[idx])
	}
	return b.String()
}

func humanDays(days float64) string {
	switch {
	case days < 1.0/24:
		return fmt.Sprintf("%.0f min", days*24*60)
	case days < 1:
		return fmt.Sprintf("%.1f h", days*24)
	default:
		return fmt.Sprintf("%.1f d", days)
	}
}

func perDay(total int64, days float64) float64 {
	if days <= 0 {
		return 0
	}
	return float64(total) / days
}

// minRateDays is the shortest window worth converting into a daily rate.
// Below it the arithmetic is dominated by when the snapshots happened to be
// taken: six downloads across 43 minutes is not "202 per day", it is six
// downloads and an accident of timing.
const minRateDays = 0.5

func rateMeaningful(days float64) bool { return days >= minRateDays }

func sortedKeys(m map[string]int64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	return keys
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
