package report

import "testing"

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
