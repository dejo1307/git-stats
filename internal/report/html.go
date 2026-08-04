package report

import (
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/dejo1307/git-stats/internal/store"
)

// HTMLFile writes a self-contained dashboard. No external requests: all CSS,
// JS and charts are inline, so the file works opened straight from disk.
func HTMLFile(path string, db *store.DB, opts Options) error {
	data, err := buildHTML(db, opts)
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return dashboardTmpl.Execute(f, data)
}

// tile is one headline number.
type tile struct {
	Label  string
	Value  string
	Detail string
}

// row is a generic table row rendered with a right-aligned numeric column.
type row struct {
	Cells []string
}

type htmlData struct {
	Repo         string
	Generated    string
	Snapshots    int
	Observed     int
	First, Last  string
	Tiles        []tile
	Cumulative   template.HTML
	Platforms    template.HTML
	PlatformRows []row
	Views        template.HTML
	Clones       template.HTML
	Stars        template.HTML
	// Coverage strings state the period each series spans, which is not the
	// same as when the snapshots were taken.
	ViewsCoverage  string
	ClonesCoverage string
	ViewsUniques   string
	ClonesUniques  string
	HasTraffic     bool
	HasStars       bool
	Releases       []row
	Referrers      []row
	Paths          []row
	Notes          []string
}

func buildHTML(db *store.DB, opts Options) (htmlData, error) {
	d := htmlData{Repo: opts.Repo, Generated: opts.now().Format("2006-01-02 15:04 MST")}

	snaps, err := db.Snapshots()
	if err != nil {
		return d, err
	}
	d.Snapshots = len(snaps)
	observed, err := db.ObservedSnapshots()
	if err != nil {
		return d, err
	}
	d.Observed = len(observed)
	if len(snaps) > 0 {
		d.First = snaps[0].TakenAt.Format("2006-01-02 15:04")
		d.Last = snaps[len(snaps)-1].TakenAt.Format("2006-01-02 15:04")
	}

	totals, err := db.LatestTotals()
	if err != nil {
		return d, err
	}
	intervals, err := db.Intervals()
	if err != nil {
		return d, err
	}
	win := sumIntervals(filterSince(intervals, opts))

	// Headline tiles. Each states the period it describes, because those
	// periods differ wildly: an all-time counter reaches back to the first
	// release, while a delta covers only the gap between two snapshots.
	allTime := fmt.Sprintf("%d releases", len(totals.ByRelease))
	if since := oldestRelease(totals); !since.IsZero() {
		allTime = fmt.Sprintf("%d releases since %s", len(totals.ByRelease), since.Format("2 Jan 2006"))
	}
	d.Tiles = append(d.Tiles, tile{
		Label:  "Downloads, all time",
		Value:  formatNum(float64(totals.Total)),
		Detail: allTime,
	})
	if win.Empty {
		d.Tiles = append(d.Tiles, tile{
			Label:  "Recent change",
			Value:  "—",
			Detail: "needs a second snapshot",
		})
	} else {
		detail := fmt.Sprintf("%s → %s",
			win.From.Format("2 Jan 15:04"), win.To.Format("2 Jan 15:04"))
		if rateMeaningful(win.Days) {
			detail += fmt.Sprintf(" · %.1f/day", win.PerDay())
		} else {
			// Refuse to extrapolate a rate from a window this short.
			detail += fmt.Sprintf(" · %s, too short for a rate", humanDays(win.Days))
		}
		d.Tiles = append(d.Tiles, tile{
			Label:  "Downloads in window",
			Value:  fmt.Sprintf("+%s", formatNum(float64(win.Total))),
			Detail: detail,
		})
	}
	// Only meaningful when the project publishes a checksum per artifact.
	if archives, sums := totals.Archives(), totals.Checksums(); sums > 0 {
		d.Tiles = append(d.Tiles, tile{
			Label: "Scripted installs",
			Value: formatNum(float64(min64(archives, sums))),
			Detail: fmt.Sprintf("checksum fetched too · %s manual",
				formatNum(float64(maxZero(archives-sums)))),
		})
	}
	if repoPoints, err := db.RepoHistory(); err == nil && len(repoPoints) > 0 {
		latest := repoPoints[len(repoPoints)-1]
		detail := fmt.Sprintf("%d forks · %d watchers", latest.Forks, latest.Watchers)
		if len(repoPoints) > 1 {
			detail = fmt.Sprintf("%+d since %s · %s",
				latest.Stars-repoPoints[0].Stars,
				repoPoints[0].TakenAt.Format("2 Jan"), detail)
		}
		d.Tiles = append(d.Tiles, tile{
			Label: "Stars", Value: formatNum(float64(latest.Stars)), Detail: detail,
		})
	}

	// Cumulative downloads: exact at every snapshot, unlike interval deltas,
	// which makes it the honest series to chart when spacing is irregular.
	cum, err := db.Cumulative()
	if err != nil {
		return d, err
	}
	pts := make([]Point, 0, len(cum))
	for _, c := range cum {
		pts = append(pts, Point{T: c.TakenAt, V: float64(c.Total)})
	}
	d.Cumulative = LineChart(pts, "--series-1", "downloads")

	// Platform breakdown.
	platformKeys := sortedKeys(totals.ByPlatform)
	bars := make([]Bar, 0, len(platformKeys))
	for _, p := range platformKeys {
		note := ""
		if delta, ok := win.ByPlatform[p]; ok && !win.Empty && delta > 0 {
			note = fmt.Sprintf("  (+%d)", delta)
		}
		bars = append(bars, Bar{Label: p, Value: float64(totals.ByPlatform[p]), Note: note})
		d.PlatformRows = append(d.PlatformRows, row{Cells: []string{
			p, formatNum(float64(totals.ByPlatform[p])), fmt.Sprintf("%+d", win.ByPlatform[p]),
		}})
	}
	d.Platforms = BarChart(bars, []string{"--series-1", "--series-2", "--series-3", "--series-4"})

	// Traffic: two measures on very different scales, so two charts rather
	// than one chart with two y-axes.
	views, err := trafficPoints(db, "views")
	if err != nil {
		return d, err
	}
	clones, err := trafficPoints(db, "clones")
	if err != nil {
		return d, err
	}
	d.HasTraffic = len(views) > 0 || len(clones) > 0
	d.Views = LineChart(views, "--series-1", "views")
	d.Clones = LineChart(clones, "--series-2", "clones")
	d.ViewsCoverage = coverageOf(views, opts.now())
	d.ClonesCoverage = coverageOf(clones, opts.now())

	for _, spec := range []struct {
		metric string
		dst    *string
	}{{"views", &d.ViewsUniques}, {"clones", &d.ClonesUniques}} {
		window, err := db.LatestTrafficWindow(spec.metric)
		if err != nil {
			return d, err
		}
		if window.Found {
			// The API's de-duplicated figure, never a sum of daily uniques.
			*spec.dst = fmt.Sprintf("%s distinct %s in the last window captured, %s total.",
				formatNum(float64(window.Uniques)), spec.metric, formatNum(float64(window.Count)))
		}
	}

	starDays, err := db.StarsByDay()
	if err != nil {
		return d, err
	}
	if len(starDays) > 0 {
		d.HasStars = true
		starPts := make([]Point, 0, len(starDays))
		for _, s := range starDays {
			day, err := time.Parse("2006-01-02", s.Day)
			if err != nil {
				continue
			}
			starPts = append(starPts, Point{T: day, V: float64(s.Uniques)}) // running total
		}
		d.Stars = LineChart(starPts, "--series-3", "stars")
	}

	byTotal := append([]store.ReleaseTotal(nil), totals.ByRelease...)
	sort.Slice(byTotal, func(i, j int) bool { return byTotal[i].Total > byTotal[j].Total })
	for i, r := range byTotal {
		if i >= 15 {
			break
		}
		d.Releases = append(d.Releases, row{Cells: []string{
			r.Tag, r.PublishedAt.Format("2006-01-02"),
			formatNum(float64(r.Total)), formatNum(float64(r.Archives)),
		}})
	}

	for _, spec := range []struct {
		kind string
		dst  *[]row
	}{{"referrer", &d.Referrers}, {"path", &d.Paths}} {
		entries, err := db.LatestTop(spec.kind)
		if err != nil {
			return d, err
		}
		for _, e := range entries {
			*spec.dst = append(*spec.dst, row{Cells: []string{
				e.Name, formatNum(float64(e.Count)), formatNum(float64(e.Uniques)),
			}})
		}
	}

	d.Notes = notes(d, win)
	return d, nil
}

func notes(d htmlData, win windowSummary) []string {
	var out []string
	if d.Snapshots < 2 {
		out = append(out, "Only one snapshot exists, so no change can be shown yet. "+
			"Release counters are cumulative all-time totals and are a baseline until a second run.")
	}
	if !d.HasTraffic {
		out = append(out, "No traffic data: the Traffic API needs a classic token with the "+
			"public_repo scope — it refuses fine-grained tokens whatever their permissions. "+
			"GitHub keeps views and clones for 14 days only, so days not captured inside that "+
			"window cannot be recovered later.")
	}
	if !win.Empty {
		out = append(out, fmt.Sprintf(
			"Deltas cover %s of wall-clock time between snapshots, not a fixed period. "+
				"Collection is manual, so intervals are irregular by design.", humanDays(win.Days)))
	}
	out = append(out,
		"Install scripts and self-updaters fetch an artifact and its checksum together, so the "+
			"smaller of the two counts approximates scripted installs and the excess artifact "+
			"downloads approximate manual ones. Such clients issue identical requests, so "+
			"GitHub's counters cannot tell a fresh install from a self-update.",
		"Source-level installs have no download counter anywhere. Clone count is the nearest "+
			"signal but a weak one in both directions: a module or package proxy caches every "+
			"version globally, so many installs can produce a single clone, while CI, mirrors "+
			"and scanners inflate the same number. Do not read clones as a user count.",
		"Counts include automated traffic — mirrors, scanners and CI — and GitHub does not break that out. "+
			"CI in particular re-clones on every run, while a developer clones once and pulls forever after.",
		"Every figure here is an acquisition event, never usage. A `git pull` into an existing "+
			"checkout is not a clone and is not counted anywhere, and an artifact download says "+
			"nothing about whether it was ever used. These are not user counts.")
	return out
}

// oldestRelease returns the publication date of the earliest release, which is
// how far back the all-time download counters actually reach.
func oldestRelease(totals store.Totals) time.Time {
	var oldest time.Time
	for _, r := range totals.ByRelease {
		if r.PublishedAt.IsZero() {
			continue
		}
		if oldest.IsZero() || r.PublishedAt.Before(oldest) {
			oldest = r.PublishedAt
		}
	}
	return oldest
}

// coverageOf describes the period a daily series actually spans, plus a
// staleness note when GitHub's newest bucket lags the collection date.
func coverageOf(points []Point, now time.Time) string {
	if len(points) == 0 {
		return ""
	}
	first, last := points[0].T, points[len(points)-1].T
	text := fmt.Sprintf("Covers %s – %s (%d days).",
		first.Format("2 Jan"), last.Format("2 Jan 2006"), len(points))

	if lag := int(now.Sub(last).Hours() / 24); lag >= 2 {
		text += fmt.Sprintf(" GitHub's newest bucket is %d days behind today, "+
			"so the right-hand edge is not current.", lag)
	}
	return text
}

func trafficPoints(db *store.DB, metric string) ([]Point, error) {
	days, err := db.Traffic(metric)
	if err != nil {
		return nil, err
	}
	pts := make([]Point, 0, len(days))
	for _, day := range days {
		t, err := time.Parse("2006-01-02", day.Day)
		if err != nil {
			continue
		}
		pts = append(pts, Point{T: t, V: float64(day.Count)})
	}
	return pts, nil
}

func maxZero(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

var dashboardTmpl = template.Must(template.New("dashboard").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Repo}} — distribution stats</title>
<style>
  :root {
    color-scheme: light;
    --page:           #f9f9f7;
    --surface-1:      #fcfcfb;
    --text-primary:   #0b0b0b;
    --text-secondary: #52514e;
    --muted:          #898781;
    --grid:           #e1e0d9;
    --axis:           #c3c2b7;
    --border:         rgba(11,11,11,0.10);
    --series-1:       #2a78d6;
    --series-2:       #eb6834;
    --series-3:       #1baf7a;
    --series-4:       #eda100;
  }
  /* Dark steps are selected for the dark surface, not an automatic flip of the
     light ones. Declared under both scopes so the toggle wins either way. */
  @media (prefers-color-scheme: dark) {
    :root:where(:not([data-theme="light"])) {
      color-scheme: dark;
      --page:           #0d0d0d;
      --surface-1:      #1a1a19;
      --text-primary:   #ffffff;
      --text-secondary: #c3c2b7;
      --muted:          #898781;
      --grid:           #2c2c2a;
      --axis:           #383835;
      --border:         rgba(255,255,255,0.10);
      --series-1:       #3987e5;
      --series-2:       #d95926;
      --series-3:       #199e70;
      --series-4:       #c98500;
    }
  }
  :root[data-theme="dark"] {
    color-scheme: dark;
    --page:           #0d0d0d;
    --surface-1:      #1a1a19;
    --text-primary:   #ffffff;
    --text-secondary: #c3c2b7;
    --muted:          #898781;
    --grid:           #2c2c2a;
    --axis:           #383835;
    --border:         rgba(255,255,255,0.10);
    --series-1:       #3987e5;
    --series-2:       #d95926;
    --series-3:       #199e70;
    --series-4:       #c98500;
  }
  * { box-sizing: border-box; }
  body {
    margin: 0; padding: 32px 20px 64px;
    background: var(--page); color: var(--text-primary);
    font: 15px/1.5 system-ui, -apple-system, "Segoe UI", sans-serif;
  }
  main { max-width: 880px; margin: 0 auto; }
  h1 { font-size: 22px; margin: 0 0 4px; }
  h2 { font-size: 15px; margin: 0 0 2px; font-weight: 600; }
  .sub { color: var(--text-secondary); font-size: 13px; margin: 0 0 28px; }
  .caption { color: var(--muted); font-size: 12.5px; margin: 0 0 12px; }
  .tiles { display: grid; grid-template-columns: repeat(auto-fit, minmax(180px, 1fr)); gap: 12px; margin-bottom: 28px; }
  .tile { background: var(--surface-1); border: 1px solid var(--border); border-radius: 10px; padding: 14px 16px; }
  .tile .label { font-size: 12px; color: var(--text-secondary); text-transform: uppercase; letter-spacing: .04em; }
  .tile .value { font-size: 30px; font-weight: 600; margin: 4px 0 2px; }
  .tile .detail { font-size: 12.5px; color: var(--muted); }
  figure { background: var(--surface-1); border: 1px solid var(--border); border-radius: 10px;
           padding: 16px 18px 12px; margin: 0 0 20px; overflow-x: auto; }
  .chart { width: 100%; height: auto; min-width: 520px; display: block; }
  .grid { stroke: var(--grid); stroke-width: 1; }
  .axis { stroke: var(--axis); stroke-width: 1; }
  .tick, .bar-label, .bar-value { fill: var(--muted); font-size: 11px;
    font-family: system-ui, sans-serif; font-variant-numeric: tabular-nums; }
  .bar-label { fill: var(--text-secondary); font-size: 12.5px; }
  .bar-value { fill: var(--text-primary); font-size: 12.5px; font-weight: 600; }
  .line { fill: none; stroke-width: 2; stroke-linejoin: round; stroke-linecap: round; }
  .area { opacity: .10; stroke: none; }
  .dot { stroke: var(--surface-1); stroke-width: 2; }
  .hit, .bar { cursor: crosshair; }
  .hit { fill: transparent; }
  .bar:hover, .hit:hover { opacity: .85; }
  .empty { color: var(--muted); font-size: 13px; margin: 8px 0 12px; }
  table { border-collapse: collapse; width: 100%; font-size: 13.5px; }
  th, td { text-align: left; padding: 7px 10px; border-bottom: 1px solid var(--border); }
  td:not(:first-child), th:not(:first-child) { text-align: right; font-variant-numeric: tabular-nums; }
  th { color: var(--text-secondary); font-weight: 600; font-size: 12px;
       text-transform: uppercase; letter-spacing: .04em; }
  details { background: var(--surface-1); border: 1px solid var(--border);
            border-radius: 10px; padding: 12px 16px; margin-bottom: 20px; }
  summary { cursor: pointer; font-weight: 600; font-size: 14px; }
  .notes { color: var(--text-secondary); font-size: 13px; padding-left: 18px; }
  .notes li { margin-bottom: 6px; }
  #tip { position: fixed; pointer-events: none; opacity: 0; transition: opacity .1s;
         background: var(--surface-1); color: var(--text-primary);
         border: 1px solid var(--border); border-radius: 8px; padding: 6px 10px;
         font-size: 12.5px; box-shadow: 0 4px 14px rgba(0,0,0,.16); z-index: 10; }
  #tip .t { color: var(--text-secondary); display: block; font-size: 11.5px; }
  #theme { position: fixed; top: 16px; right: 16px; background: var(--surface-1);
           color: var(--text-secondary); border: 1px solid var(--border);
           border-radius: 999px; padding: 6px 14px; font-size: 12.5px; cursor: pointer;
           font-family: inherit; }
</style>
</head>
<body>
<button id="theme" type="button" aria-label="Switch between light and dark">◐ theme</button>
<main>
  <h1>{{.Repo}} — distribution stats</h1>
  <p class="sub">Generated {{.Generated}} from {{.Snapshots}} snapshot(s) collected
    {{.First}} → {{.Last}}{{if lt .Observed .Snapshots}}, of which {{.Observed}} captured
    release data — the rest were failed runs and are excluded from download figures rather
    than counted as zero{{end}}. Those are collection times; each figure below states the
    period it actually describes, which is usually much longer.</p>

  <div class="tiles">
    {{range .Tiles}}
    <div class="tile">
      <div class="label">{{.Label}}</div>
      <div class="value">{{.Value}}</div>
      <div class="detail">{{.Detail}}</div>
    </div>
    {{end}}
  </div>

  <figure>
    <h2>Cumulative release downloads</h2>
    <p class="caption">All-time total observed at each snapshot, so the x-axis spans when
      snapshots were taken — not the months over which those downloads accumulated. Exact at
      every point, unlike per-interval deltas, which depend on snapshot spacing.</p>
    {{.Cumulative}}
  </figure>

  <figure>
    <h2>Downloads by platform</h2>
    <p class="caption">All-time, with the change over the reporting window in parentheses.</p>
    {{.Platforms}}
  </figure>

  {{if .HasTraffic}}
  <figure>
    <h2>Repository views</h2>
    <p class="caption">{{.ViewsCoverage}} {{.ViewsUniques}}
      Daily buckets merged across snapshots; only days captured inside GitHub's 14-day
      retention window exist here.</p>
    {{.Views}}
  </figure>
  <figure>
    <h2>Clones</h2>
    <p class="caption">{{.ClonesCoverage}} {{.ClonesUniques}}
      <strong>Clones do not overlap with release downloads</strong> — <code>git clone</code> and
      source-level package managers fetch no release asset, while install scripts, self-updaters
      and browser downloads never clone. A clone total far above the all-time download total is
      therefore expected, not a contradiction. Charted separately from views rather than sharing
      an axis.</p>
    {{.Clones}}
  </figure>
  {{end}}

  {{if .HasStars}}
  <figure>
    <h2>Stars over time</h2>
    <p class="caption">Backfilled from each star's own timestamp, so this history is complete
      rather than sampled.</p>
    {{.Stars}}
  </figure>
  {{end}}

  {{if .Releases}}
  <figure>
    <h2>Top releases</h2>
    <table>
      <thead><tr><th>Release</th><th>Published</th><th>Downloads</th><th>Artifacts</th></tr></thead>
      <tbody>{{range .Releases}}<tr>{{range .Cells}}<td>{{.}}</td>{{end}}</tr>{{end}}</tbody>
    </table>
  </figure>
  {{end}}

  {{if .Referrers}}
  <figure>
    <h2>Top referrers <span class="caption">(last 14 days)</span></h2>
    <table>
      <thead><tr><th>Referrer</th><th>Views</th><th>Unique</th></tr></thead>
      <tbody>{{range .Referrers}}<tr>{{range .Cells}}<td>{{.}}</td>{{end}}</tr>{{end}}</tbody>
    </table>
  </figure>
  {{end}}

  {{if .Paths}}
  <figure>
    <h2>Top paths <span class="caption">(last 14 days)</span></h2>
    <table>
      <thead><tr><th>Path</th><th>Views</th><th>Unique</th></tr></thead>
      <tbody>{{range .Paths}}<tr>{{range .Cells}}<td>{{.}}</td>{{end}}</tr>{{end}}</tbody>
    </table>
  </figure>
  {{end}}

  <details>
    <summary>Platform table</summary>
    <table>
      <thead><tr><th>Platform</th><th>All time</th><th>Window</th></tr></thead>
      <tbody>{{range .PlatformRows}}<tr>{{range .Cells}}<td>{{.}}</td>{{end}}</tr>{{end}}</tbody>
    </table>
  </details>

  <details open>
    <summary>How to read this</summary>
    <ul class="notes">{{range .Notes}}<li>{{.}}</li>{{end}}</ul>
  </details>
</main>

<div id="tip" role="status" aria-live="polite"></div>
<script>
// Hover layer: every mark carrying data-label/data-value gets a tooltip.
(function () {
  const tip = document.getElementById('tip');
  function show(e, el) {
    tip.innerHTML = '<span class="t">' + el.dataset.label + '</span>' + el.dataset.value;
    tip.style.opacity = '1';
    const pad = 14, box = tip.getBoundingClientRect();
    let x = e.clientX + pad, y = e.clientY + pad;
    if (x + box.width > innerWidth) x = e.clientX - box.width - pad;
    if (y + box.height > innerHeight) y = e.clientY - box.height - pad;
    tip.style.left = x + 'px';
    tip.style.top = y + 'px';
  }
  document.addEventListener('mousemove', function (e) {
    const el = e.target.closest('[data-value]');
    if (el) { show(e, el); } else { tip.style.opacity = '0'; }
  });
  document.addEventListener('mouseleave', () => { tip.style.opacity = '0'; });

  document.getElementById('theme').addEventListener('click', function () {
    const root = document.documentElement;
    const dark = root.dataset.theme
      ? root.dataset.theme === 'dark'
      : matchMedia('(prefers-color-scheme: dark)').matches;
    root.dataset.theme = dark ? 'light' : 'dark';
  });
})();
</script>
</body>
</html>
`))
