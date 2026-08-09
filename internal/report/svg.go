package report

import (
	"fmt"
	"html"
	"html/template"
	"math"
	"sort"
	"strings"
	"time"
)

// Chart geometry. One size for every chart keeps the dashboard's plots
// aligned on a common grid.
const (
	chartW  = 760
	chartH  = 220
	padL    = 48
	padR    = 18
	padT    = 14
	padB    = 30
	plotW   = chartW - padL - padR
	plotH   = chartH - padT - padB
	gridDiv = 4
)

// Point is one observation on a time series.
type Point struct {
	T time.Time
	V float64
}

// Mark is a dated annotation drawn over a time chart: a release, or a change
// to a tracked file. Marks are the whole point of the annotated charts — a
// series alone shows that something moved, and a mark is what proposes why.
type Mark struct {
	T        time.Time
	Label    string
	Detail   string
	ColorVar string
}

// minMarkGap is the closest two marks may be drawn before they are merged into
// one. Ship every other day for two months and the markers otherwise fuse into
// a picket fence that hides the series behind it.
const minMarkGap = 7.0

// LineChart renders a single-series time chart. Single series means no legend
// box is needed — the figure's title names it. Irregular spacing between
// snapshots is preserved: x is positioned by timestamp, not by index, so a
// three-week gap looks like a three-week gap.
func LineChart(pts []Point, colorVar, unit string) template.HTML {
	return MarkedLineChart(pts, colorVar, unit, nil)
}

// MarkedLineChart is LineChart with event markers overlaid.
func MarkedLineChart(pts []Point, colorVar, unit string, marks []Mark) template.HTML {
	if len(pts) == 0 {
		return template.HTML(`<p class="empty">No data recorded yet.</p>`)
	}
	if len(pts) == 1 {
		return template.HTML(fmt.Sprintf(
			`<p class="empty">Only one observation so far (%s = %s). A line needs two.</p>`,
			html.EscapeString(pts[0].T.Format("2006-01-02 15:04")), formatNum(pts[0].V)))
	}

	minT, maxT := pts[0].T, pts[len(pts)-1].T
	span := maxT.Sub(minT).Seconds()
	if span <= 0 {
		span = 1
	}
	maxV := 0.0
	for _, p := range pts {
		maxV = math.Max(maxV, p.V)
	}
	top := niceCeil(maxV)

	x := func(t time.Time) float64 { return padL + t.Sub(minT).Seconds()/span*plotW }
	y := func(v float64) float64 { return padT + plotH - (v/top)*plotH }

	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart" viewBox="0 0 %d %d" role="img" aria-label="%s over time">`,
		chartW, chartH, html.EscapeString(unit))

	writeGrid(&b, top)

	// Area under the line, then the line itself on top.
	var line, area strings.Builder
	for i, p := range pts {
		if i > 0 {
			line.WriteByte(' ')
		}
		fmt.Fprintf(&line, "%.1f,%.1f", x(p.T), y(p.V))
	}
	baseline := float64(padT + plotH)
	fmt.Fprintf(&area, "%.1f,%.1f %s %.1f,%.1f",
		x(pts[0].T), baseline, line.String(), x(pts[len(pts)-1].T), baseline)

	fmt.Fprintf(&b, `<polygon class="area" fill="var(%s)" points="%s"/>`, colorVar, area.String())
	fmt.Fprintf(&b, `<polyline class="line" stroke="var(%s)" points="%s"/>`, colorVar, line.String())

	// Markers only when they will not collide; 8px minimum hit size.
	if len(pts) <= 40 {
		for _, p := range pts {
			fmt.Fprintf(&b, `<circle class="dot" cx="%.1f" cy="%.1f" r="4" fill="var(%s)"/>`,
				x(p.T), y(p.V), colorVar)
		}
	}

	// Transparent hover bands, one per point, wider than the mark itself.
	bandW := plotW / float64(len(pts))
	for _, p := range pts {
		fmt.Fprintf(&b,
			`<rect class="hit" x="%.1f" y="%d" width="%.1f" height="%d" `+
				`data-x="%.1f" data-y="%.1f" data-label="%s" data-value="%s %s"/>`,
			math.Max(padL, x(p.T)-bandW/2), padT, bandW, plotH,
			x(p.T), y(p.V),
			html.EscapeString(p.T.Format("Mon 2 Jan 2006 15:04")),
			formatNum(p.V), html.EscapeString(unit))
	}

	writeMarks(&b, marks, minT, maxT, x)
	writeXLabels(&b, minT, maxT)
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// writeMarks draws event markers, dropping any that fall outside the plotted
// period. A release from before the first snapshot has no place on a chart of
// what happened after it, and clamping it to the left edge would assert it
// happened at a time it did not.
func writeMarks(b *strings.Builder, marks []Mark, minT, maxT time.Time, x func(time.Time) float64) {
	inRange := make([]Mark, 0, len(marks))
	for _, m := range marks {
		if m.T.Before(minT) || m.T.After(maxT) {
			continue
		}
		inRange = append(inRange, m)
	}
	sort.Slice(inRange, func(i, j int) bool { return inRange[i].T.Before(inRange[j].T) })

	// Where the markers are dense the dashed verticals stop being annotation
	// and become a fence in front of the data — a project that ships every
	// other day would hide six months of its own history behind its release
	// history. Past that density only the flags along the top edge are drawn:
	// the events are still all there and still all hoverable, but nothing
	// crosses the series.
	type placed struct {
		x     float64
		color string
		label string
		hover string
		when  time.Time
	}
	var groups []placed

	for i := 0; i < len(inRange); {
		// Absorb every following mark of the same kind that would overlap.
		// The comparison is against the first member, not the previous one, so
		// a merged marker never grows wider than the flag that represents it —
		// chaining off each new member would let one flag swallow a whole year
		// of daily releases and claim to point at all of them.
		group := inRange[i : i+1]
		j := i + 1
		for j < len(inRange) &&
			inRange[j].ColorVar == inRange[i].ColorVar &&
			x(inRange[j].T)-x(inRange[i].T) < minMarkGap {
			group = inRange[i : j+1]
			j++
		}
		i = j

		first, last := group[0], group[len(group)-1]
		// A merged group is drawn at its midpoint so the marker spans the
		// period it stands for rather than pointing at its first member.
		at := (x(first.T) + x(last.T)) / 2
		color := first.ColorVar
		if color == "" {
			color = "--muted"
		}

		label, detail := first.Label, first.Detail
		if len(group) > 1 {
			names := make([]string, 0, len(group))
			for _, m := range group {
				names = append(names, m.Label)
			}
			label = fmt.Sprintf("%d events", len(group))
			detail = strings.Join(names, ", ")
			if first.T.Format("2 Jan") != last.T.Format("2 Jan") {
				detail = fmt.Sprintf("%s – %s: %s",
					first.T.Format("2 Jan"), last.T.Format("2 Jan"), detail)
			}
		}

		groups = append(groups, placed{
			x: at, color: color, label: label, hover: detail, when: first.T,
		})
	}
	if len(groups) == 0 {
		return
	}

	// Two flag widths of clear space per marker is the point at which the
	// verticals still read as separate lines rather than as hatching.
	drawLines := plotW/float64(len(groups)) >= 2*minMarkGap

	for _, g := range groups {
		if drawLines {
			fmt.Fprintf(b,
				`<line class="mark" stroke="var(%s)" x1="%.1f" y1="%d" x2="%.1f" y2="%d"/>`,
				g.color, g.x, padT, g.x, padT+plotH)
		}
		fmt.Fprintf(b,
			`<polygon class="mark-flag" fill="var(%s)" points="%.1f,%d %.1f,%d %.1f,%d"/>`,
			g.color, g.x-3.5, padT, g.x+3.5, padT, g.x, padT+6)
		// The hit area is only the flag at the top, so hovering the plot still
		// reads the series rather than the annotation.
		fmt.Fprintf(b,
			`<rect class="hit" x="%.1f" y="%d" width="9" height="14" `+
				`data-label="%s" data-value="%s"/>`,
			g.x-4.5, padT-4,
			html.EscapeString(g.when.Format("Mon 2 Jan 2006")+" · "+g.label),
			html.EscapeString(truncate(g.hover, 80)))
	}
}

// ColumnChart renders one bar per day. Where a cumulative line answers "how
// many", this answers "when" — a single day's spike is a visible column here
// and an imperceptible change of slope on the cumulative curve.
//
// Bars are positioned by date, so days with no observation leave a gap rather
// than closing up and shifting every later day leftwards.
func ColumnChart(pts []Point, colorVar, unit string, marks []Mark) template.HTML {
	if len(pts) == 0 {
		return template.HTML(`<p class="empty">No data recorded yet.</p>`)
	}

	minT, maxT := pts[0].T, pts[len(pts)-1].T
	span := maxT.Sub(minT).Seconds()
	if span <= 0 {
		// A single day still deserves its bar, centred in the plot.
		span = 24 * 3600
	}
	maxV := 0.0
	for _, p := range pts {
		maxV = math.Max(maxV, p.V)
	}
	top := niceCeil(maxV)

	x := func(t time.Time) float64 { return padL + t.Sub(minT).Seconds()/span*plotW }
	y := func(v float64) float64 { return padT + plotH - (v/top)*plotH }

	// One day wide, minus a hairline so adjacent days read as separate bars.
	days := math.Max(1, span/(24*3600))
	barW := math.Max(1.5, plotW/days-1)

	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart" viewBox="0 0 %d %d" role="img" aria-label="%s per day">`,
		chartW, chartH, html.EscapeString(unit))
	writeGrid(&b, top)

	for _, p := range pts {
		height := float64(padT+plotH) - y(p.V)
		if p.V > 0 {
			height = math.Max(height, 1) // never render a real value as nothing
		}
		fmt.Fprintf(&b,
			`<rect class="bar" x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="var(%s)" `+
				`data-label="%s" data-value="%s %s"/>`,
			x(p.T)-barW/2, float64(padT+plotH)-height, barW, height, colorVar,
			html.EscapeString(p.T.Format("Mon 2 Jan 2006")),
			formatNum(p.V), html.EscapeString(unit))
	}

	writeMarks(&b, marks, minT, maxT, x)
	writeXLabels(&b, minT, maxT)
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

func writeGrid(b *strings.Builder, top float64) {
	for i := 0; i <= gridDiv; i++ {
		v := top * float64(gridDiv-i) / float64(gridDiv)
		yy := padT + plotH*float64(i)/float64(gridDiv)
		fmt.Fprintf(b, `<line class="grid" x1="%d" y1="%.1f" x2="%d" y2="%.1f"/>`,
			padL, yy, chartW-padR, yy)
		fmt.Fprintf(b, `<text class="tick" x="%d" y="%.1f" text-anchor="end">%s</text>`,
			padL-8, yy+4, formatNum(v))
	}
	fmt.Fprintf(b, `<line class="axis" x1="%d" y1="%d" x2="%d" y2="%d"/>`,
		padL, padT+plotH, chartW-padR, padT+plotH)
}

func writeXLabels(b *strings.Builder, minT, maxT time.Time) {
	fmt.Fprintf(b, `<text class="tick" x="%d" y="%d" text-anchor="start">%s</text>`,
		padL, chartH-8, html.EscapeString(minT.Format("2 Jan")))
	fmt.Fprintf(b, `<text class="tick" x="%d" y="%d" text-anchor="end">%s</text>`,
		chartW-padR, chartH-8, html.EscapeString(maxT.Format("2 Jan 2006")))
}

// Bar is one category in a horizontal bar chart.
type Bar struct {
	Label string
	Value float64
	Note  string
}

// BarChart renders horizontal bars for categorical magnitude. Every bar is
// directly labelled with its category and value, which is also what satisfies
// the light-mode contrast relief rule for the lower-contrast slots.
func BarChart(bars []Bar, colorVars []string) template.HTML {
	if len(bars) == 0 {
		return template.HTML(`<p class="empty">No data recorded yet.</p>`)
	}
	maxV := 0.0
	for _, bar := range bars {
		maxV = math.Max(maxV, bar.Value)
	}
	if maxV <= 0 {
		maxV = 1
	}

	const (
		rowH   = 34
		barH   = 18
		labelW = 132
		valueW = 140
	)
	height := rowH * len(bars)
	trackW := float64(chartW - labelW - valueW)

	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart" viewBox="0 0 %d %d" role="img" aria-label="downloads by platform">`,
		chartW, height)
	for i, bar := range bars {
		yy := float64(i*rowH) + (rowH-barH)/2
		w := math.Max(2, bar.Value/maxV*trackW)
		color := colorVars[i%len(colorVars)]

		fmt.Fprintf(&b, `<text class="bar-label" x="0" y="%.1f">%s</text>`,
			yy+float64(barH)*0.75, html.EscapeString(bar.Label))
		// 4px rounded data-end, anchored flat to the baseline at x=labelW.
		fmt.Fprintf(&b, `<rect class="bar" x="%d" y="%.1f" width="%.1f" height="%d" rx="4" `+
			`fill="var(%s)" data-label="%s" data-value="%s downloads"/>`,
			labelW, yy, w, barH, color,
			html.EscapeString(bar.Label), formatNum(bar.Value))
		fmt.Fprintf(&b, `<text class="bar-value" x="%.1f" y="%.1f">%s%s</text>`,
			float64(labelW)+w+8, yy+float64(barH)*0.75,
			formatNum(bar.Value), html.EscapeString(bar.Note))
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// niceCeil rounds a maximum up to a readable axis top.
func niceCeil(v float64) float64 {
	if v <= 0 {
		return 1
	}
	mag := math.Pow(10, math.Floor(math.Log10(v)))
	for _, step := range []float64{1, 2, 2.5, 5, 10} {
		if top := step * mag; top >= v {
			return top
		}
	}
	return 10 * mag
}

func formatNum(v float64) string {
	if v == math.Trunc(v) {
		return addThousands(fmt.Sprintf("%.0f", v))
	}
	return fmt.Sprintf("%.1f", v)
}

func addThousands(s string) string {
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}
