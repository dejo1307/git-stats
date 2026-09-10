package report

import (
	"bytes"
	"strings"
	"testing"

	"github.com/dejo1307/git-stats/internal/store"
)

func TestSparkline(t *testing.T) {
	tests := []struct {
		name   string
		values []int64
		want   string
	}{
		{"ascending", []int64{0, 1, 2, 3, 4, 5, 6, 7}, "▁▂▃▄▅▆▇█"},
		{"flat zero renders lows, not blanks", []int64{0, 0, 0}, "▁▁▁"},
		{"flat non-zero renders highs", []int64{5, 5, 5}, "███"},
		{"empty", nil, ""},
	}
	for _, tt := range tests {
		if got := Sparkline(tt.values); got != tt.want {
			t.Errorf("%s: Sparkline(%v) = %q, want %q", tt.name, tt.values, got, tt.want)
		}
	}
}

func TestHumanDays(t *testing.T) {
	tests := []struct {
		days float64
		want string
	}{
		{2.5, "2.5 d"},
		{0.5, "12.0 h"},
		{0.02, "29 min"},
	}
	for _, tt := range tests {
		if got := humanDays(tt.days); got != tt.want {
			t.Errorf("humanDays(%v) = %q, want %q", tt.days, got, tt.want)
		}
	}
}

func TestNiceCeil(t *testing.T) {
	tests := []struct{ in, want float64 }{
		{0, 1}, {7, 10}, {45, 50}, {456, 500}, {1200, 2000},
	}
	for _, tt := range tests {
		if got := niceCeil(tt.in); got != tt.want {
			t.Errorf("niceCeil(%v) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestAddThousands(t *testing.T) {
	tests := []struct{ in, want string }{
		{"456", "456"}, {"1234", "1,234"}, {"1234567", "1,234,567"}, {"-2500", "-2,500"},
	}
	for _, tt := range tests {
		if got := addThousands(tt.in); got != tt.want {
			t.Errorf("addThousands(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestWriteInstallMixShowsUpgradesOnlyWhenPublished(t *testing.T) {
	totals := store.Totals{
		Total:         30,
		ByPlatform:    map[string]int64{"darwin-arm64": 20},
		ByKind:        map[string]int64{"tar.gz": 20, "sha256": 8, store.UpgradeChecksum: 2},
		Mix:           store.Mix{Upgrades: 2, Scripted: 8, Manual: 10},
		MixByPlatform: map[string]store.Mix{"darwin-arm64": {Upgrades: 2, Scripted: 8, Manual: 10}},
	}
	var buf bytes.Buffer
	writeInstallMix(&buf, totals, windowSummary{Empty: true})
	if !strings.Contains(buf.String(), "upgrades") {
		t.Errorf("published upgrade checksum but no upgrades column:\n%s", buf.String())
	}

	// Without the updater's own checksum there is nothing to count upgrades
	// from, and a column of zeros would read as a measurement.
	delete(totals.ByKind, store.UpgradeChecksum)
	buf.Reset()
	writeInstallMix(&buf, totals, windowSummary{Empty: true})
	if strings.Contains(buf.String(), "upgrades") {
		t.Errorf("upgrades column shown for releases with no upgrade checksum:\n%s", buf.String())
	}
}

func TestWritePlatformsShowsOtherAssets(t *testing.T) {
	totals := store.Totals{
		Total:      25,
		Other:      9,
		ByPlatform: map[string]int64{"darwin-arm64": 16},
	}
	win := windowSummary{Total: 10, Other: 4, Days: 2, ByPlatform: map[string]int64{"darwin-arm64": 6}}

	var buf bytes.Buffer
	if err := writePlatforms(&buf, totals, win, Options{}); err != nil {
		t.Fatalf("writePlatforms: %v", err)
	}
	for _, want := range []string{"darwin-arm64", "other (not installs)", "TOTAL"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("platform table is missing %q:\n%s", want, buf.String())
		}
	}

	totals.Other = 0
	win.Other = 0
	buf.Reset()
	if err := writePlatforms(&buf, totals, win, Options{}); err != nil {
		t.Fatalf("writePlatforms: %v", err)
	}
	if strings.Contains(buf.String(), "other (not installs)") {
		t.Errorf("an empty other bucket was rendered:\n%s", buf.String())
	}
}
