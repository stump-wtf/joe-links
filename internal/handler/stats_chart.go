// Governing: SPEC-0021 REQ "Per-Link Daily Time Series", ADR-0021
package handler

import (
	"fmt"
	"math"
	"time"

	"github.com/joestump/joe-links/internal/store"
)

// Chart geometry in SVG viewBox units. The SVG scales to its container via
// the viewBox, so these are layout proportions, not pixels.
const (
	chartWidth     = 720.0
	chartHeight    = 180.0
	chartPadTop    = 12.0
	chartPadBottom = 12.0
	chartPadLeft   = 8.0
	chartPadRight  = 8.0
	chartBarFill   = 0.68 // bar width as a fraction of the day slot
	chartZeroTickH = 2.0  // zero-height mark: the day position is present, just unclicked
)

// ChartBar is one UTC-day position in the rendered SVG chart.
// Governing: SPEC-0021 REQ "Per-Link Daily Time Series"
type ChartBar struct {
	Date   string // UTC calendar day, "2006-01-02"
	Count  int64
	Pruned bool // day opens before the retention horizon: no-data, not zero

	// Rect geometry (viewBox units), rounded to 2 decimals for stable markup.
	X, Y, Width, Height float64
	SlotX, SlotWidth    float64 // the full day slot, used by the no-data band

	Tooltip string
}

// ClickChart is the server-computed view model for the daily-clicks chart:
// coordinates are computed in Go and rendered as inline SVG by html/template —
// no chart library and no client-side rendering JavaScript (ADR-0021).
// Governing: SPEC-0021 REQ "Per-Link Daily Time Series", ADR-0021
type ClickChart struct {
	Days       int
	Width      float64
	Height     float64
	PlotTop    float64
	PlotHeight float64
	BaseY      float64 // baseline y (bottom of the plot area)
	LeftX      float64 // left edge of the plot area
	RightX     float64 // right edge of the plot area
	MaxCount   int64
	Total      int64 // observed clicks summed over the window
	HasPruned  bool  // any day precedes the retention horizon (legend trigger)
	StartLabel string
	EndLabel   string
	Bars       []ChartBar
}

// StatsChartData is the template payload for the stats_chart partial, shared
// by the full stats page render and the HTMX window-toggle fragment so both
// paths produce identical markup.
// Governing: SPEC-0021 REQ "Per-Link Daily Time Series"
type StatsChartData struct {
	LinkID string
	Days   int
	Chart  ClickChart
}

// buildClickChart converts the store's gap-filled daily series into SVG
// geometry. The series arrives with exactly one entry per day, ascending,
// zero-count days present (SPEC-0021) — this function never interpolates.
// Governing: SPEC-0021 REQ "Per-Link Daily Time Series", ADR-0021
func buildClickChart(series []store.DailyClickCount, days int) ClickChart {
	c := ClickChart{
		Days:       days,
		Width:      chartWidth,
		Height:     chartHeight,
		PlotTop:    chartPadTop,
		PlotHeight: chartHeight - chartPadTop - chartPadBottom,
		LeftX:      chartPadLeft,
		RightX:     chartWidth - chartPadRight,
	}
	c.BaseY = c.PlotTop + c.PlotHeight
	if len(series) == 0 {
		return c
	}

	for _, d := range series {
		if d.Count > c.MaxCount {
			c.MaxCount = d.Count
		}
		c.Total += d.Count
		if d.Pruned {
			c.HasPruned = true
		}
	}
	scaleMax := c.MaxCount
	if scaleMax < 1 {
		scaleMax = 1 // all-zero window still renders a flat baseline of ticks
	}

	c.StartLabel = chartDayLabel(series[0].Date)
	c.EndLabel = chartDayLabel(series[len(series)-1].Date)

	plotWidth := c.RightX - c.LeftX
	slotWidth := plotWidth / float64(len(series))
	barWidth := slotWidth * chartBarFill

	c.Bars = make([]ChartBar, 0, len(series))
	for i, d := range series {
		slotX := c.LeftX + float64(i)*slotWidth
		height := float64(d.Count) / float64(scaleMax) * c.PlotHeight
		switch {
		case d.Count == 0:
			// Zero-count days render a zero-height mark at the baseline so a
			// gap day is visibly a day position, not a hole in the chart.
			height = chartZeroTickH
		case height < chartZeroTickH:
			// Tiny non-zero counts stay visible above the zero tick.
			height = chartZeroTickH
		}
		c.Bars = append(c.Bars, ChartBar{
			Date:      d.Date,
			Count:     d.Count,
			Pruned:    d.Pruned,
			X:         round2(slotX + (slotWidth-barWidth)/2),
			Y:         round2(c.BaseY - height),
			Width:     round2(barWidth),
			Height:    round2(height),
			SlotX:     round2(slotX),
			SlotWidth: round2(slotWidth),
			Tooltip:   chartTooltip(d),
		})
	}
	return c
}

// chartTooltip renders the per-day hover text (<title> in the SVG).
//
// The bucket containing the retention horizon can be Pruned with Count > 0:
// rows newer than the horizon survived inside a UTC day that opens before it.
// Deliberate rendering decision for that partial bucket (SPEC-0021 REQ
// "Per-Link Daily Time Series"): it shows BOTH the no-data band and a bar at
// the observed count, labeled as a lower bound ("at least N"). Hiding the
// surviving rows would understate real clicks; rendering them unmarked would
// overstate a partially-pruned day's completeness.
func chartTooltip(d store.DailyClickCount) string {
	label := chartDayLabel(d.Date)
	switch {
	case d.Pruned && d.Count > 0:
		return fmt.Sprintf("%s — at least %d clicks (day crosses the retention horizon; older rows pruned)", label, d.Count)
	case d.Pruned:
		return label + " — no data (before the retention horizon)"
	case d.Count == 1:
		return label + " — 1 click"
	default:
		return fmt.Sprintf("%s — %d clicks", label, d.Count)
	}
}

// chartDayLabel formats a series date ("2006-01-02") for humans; unparseable
// input is returned verbatim rather than dropped.
func chartDayLabel(date string) string {
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		return date
	}
	return t.Format("Jan 2, 2006")
}

// round2 rounds to 2 decimals so template output stays stable and compact.
func round2(v float64) float64 { return math.Round(v*100) / 100 }
