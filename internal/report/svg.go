package report

import (
	"fmt"
	"html"
	"html/template"
	"math"
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

// LineChart renders a single-series time chart. Single series means no legend
// box is needed — the figure's title names it. Irregular spacing between
// snapshots is preserved: x is positioned by timestamp, not by index, so a
// three-week gap looks like a three-week gap.
func LineChart(pts []Point, colorVar, unit string) template.HTML {
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
