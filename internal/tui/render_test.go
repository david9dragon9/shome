package tui

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func init() { NoColor = true }

func TestGaugeWidthIsExact(t *testing.T) {
	// Every gauge in a column has to be the same width or the layout shears.
	for _, pct := range []float64{-1, 0, 0.4, 1, 33.3, 50, 99.9, 100, 150} {
		for _, w := range []int{1, 8, 20, 41} {
			got := utf8.RuneCountInString(Gauge(pct, w, ""))
			if got != w {
				t.Errorf("Gauge(%v, %d) is %d cells wide, want %d", pct, w, got, w)
			}
		}
	}
}

// Unknown must not render as an empty bar. A reader cannot tell "idle" from
// "no idea" if both draw as nothing, and that difference is the whole reason
// the sentinel exists.
func TestGaugeDistinguishesUnknownFromZero(t *testing.T) {
	unknown := Gauge(-1, 10, "")
	zero := Gauge(0, 10, "")
	if unknown == zero {
		t.Fatalf("unknown and zero render identically as %q", zero)
	}
	if !strings.Contains(unknown, "╌") {
		t.Errorf("unknown gauge %q does not use the dashed track", unknown)
	}
}

func TestGaugeIsMonotonic(t *testing.T) {
	prev := -1
	for pct := 0; pct <= 100; pct += 5 {
		filled := strings.Count(Gauge(float64(pct), 20, ""), "█")
		if filled < prev {
			t.Fatalf("gauge shrank going from below to %d%%: %d < %d", pct, filled, prev)
		}
		prev = filled
	}
}

func TestSparklineWidthAndAlignment(t *testing.T) {
	got := Sparkline([]float64{10, 20, 30}, 8, "")
	if n := utf8.RuneCountInString(got); n != 8 {
		t.Fatalf("sparkline is %d cells, want 8", n)
	}
	// Newest sample belongs at the right, next to the live gauge.
	if !strings.HasPrefix(got, "     ") {
		t.Errorf("short series should be right-aligned, got %q", got)
	}
	long := make([]float64, 50)
	if n := utf8.RuneCountInString(Sparkline(long, 10, "")); n != 10 {
		t.Errorf("a long series was not clipped to the width")
	}
}

// A fixed 0..100 scale: identical flat series at different levels must render
// at different heights, which auto-scaling would flatten to the same glyph.
func TestSparklineIsAbsolutelyScaled(t *testing.T) {
	low := Sparkline([]float64{5, 5, 5}, 3, "")
	high := Sparkline([]float64{95, 95, 95}, 3, "")
	if low == high {
		t.Fatalf("5%% and 95%% render identically as %q", low)
	}
}

func TestVisibleLenIgnoresEscapes(t *testing.T) {
	plain := "hello"
	colored := Red + "hello" + Reset
	if got := VisibleLen(colored); got != len(plain) {
		t.Errorf("VisibleLen(%q) = %d, want %d", colored, got, len(plain))
	}
}

func TestPadUsesVisibleWidth(t *testing.T) {
	NoColor = false
	defer func() { NoColor = true }()
	s := C(Red, "abc")
	if got := VisibleLen(Pad(s, 10)); got != 10 {
		t.Errorf("padded coloured string is %d cells, want 10", got)
	}
}

func TestTruncate(t *testing.T) {
	for _, tc := range []struct {
		in   string
		w    int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello", 4, "hel…"},
		{"hello", 1, "…"},
		{"hello", 0, ""},
	} {
		if got := Truncate(tc.in, tc.w); got != tc.want {
			t.Errorf("Truncate(%q,%d) = %q, want %q", tc.in, tc.w, got, tc.want)
		}
	}
}

func TestBytes(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{-1, "-"}, {512, "512B"}, {2048, "2K"},
		{5 << 20, "5M"}, {3 << 30, "3.0G"}, {2 << 40, "2.0T"},
	} {
		if got := Bytes(tc.in); got != tc.want {
			t.Errorf("Bytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPctMarksUnknown(t *testing.T) {
	if got := Pct(-1); strings.TrimSpace(got) != "-" {
		t.Errorf("Pct(-1) = %q, want a dash", got)
	}
	if got := Pct(42.4); strings.TrimSpace(got) != "42%" {
		t.Errorf("Pct(42.4) = %q", got)
	}
	// Fixed width keeps columns aligned.
	if len(Pct(-1)) != len(Pct(100)) {
		t.Error("Pct widths differ between known and unknown")
	}
}

func TestLoadColorThresholds(t *testing.T) {
	if LoadColor(-1) != Grey {
		t.Error("unmeasured load should be grey, not a health colour")
	}
	if LoadColor(50) != BrightGreen || LoadColor(75) != BrightYellow || LoadColor(95) != BrightRed {
		t.Error("load colour thresholds changed unexpectedly")
	}
}
