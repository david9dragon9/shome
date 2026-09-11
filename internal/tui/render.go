// Package tui provides the drawing primitives for shome's terminal dashboard:
// colour, gauges, sparklines and byte formatting.
//
// Hand-rolled rather than built on a widget toolkit. The dashboard is a
// fixed layout redrawn on a timer, which is the case a framework helps least
// with, and a terminal UI library would be by far the largest dependency in
// the project -- for a program whose whole promise is that removing it leaves
// no trace.
package tui

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// ANSI attributes. Colour is used to encode one thing only -- how much
// attention something needs -- so that the display stays readable for anyone
// who cannot distinguish the hues.
const (
	Reset   = "\033[0m"
	Bold    = "\033[1m"
	Dim     = "\033[2m"
	Reverse = "\033[7m"

	Red     = "\033[31m"
	Green   = "\033[32m"
	Yellow  = "\033[33m"
	Blue    = "\033[34m"
	Magenta = "\033[35m"
	Cyan    = "\033[36m"
	White   = "\033[37m"
	Grey    = "\033[90m"

	BrightRed    = "\033[91m"
	BrightGreen  = "\033[92m"
	BrightYellow = "\033[93m"
	BrightBlue   = "\033[94m"
	BrightCyan   = "\033[96m"
)

// Screen control.
const (
	AltScreenOn  = "\033[?1049h"
	AltScreenOff = "\033[?1049l"
	CursorHide   = "\033[?25l"
	CursorShow   = "\033[?25h"
	Home         = "\033[H"
	ClearBelow   = "\033[J"
	ClearLine    = "\033[K"
)

// NoColor disables every escape sequence, for a pipe or a dumb terminal.
var NoColor bool

// C wraps s in a colour, unless colour is disabled.
func C(color, s string) string {
	if NoColor || color == "" {
		return s
	}
	return color + s + Reset
}

// LoadColor maps a utilisation percentage to how alarming it is.
//
// The thresholds are deliberately high. On a machine deliberately lent out for
// compute, 70% CPU is the system working as intended, and colouring it as a
// warning would train the reader to ignore the colour entirely.
func LoadColor(pct float64) string {
	switch {
	case pct < 0:
		return Grey
	case pct >= 90:
		return BrightRed
	case pct >= 70:
		return BrightYellow
	default:
		return BrightGreen
	}
}

// StateColor maps a job or node state to a colour.
func StateColor(state string) string {
	switch state {
	case "RUNNING", "UP":
		return BrightGreen
	case "PENDING", "DRAIN", "SUSPENDED":
		return BrightYellow
	case "COMPLETED":
		return Cyan
	case "FAILED", "OUT_OF_MEMORY", "TIMEOUT", "DOWN", "NODE_FAIL":
		return BrightRed
	case "CANCELLED":
		return Grey
	default:
		return ""
	}
}

// bar glyphs, finest first. Partial blocks give a 1/8-cell resolution, which
// matters at the narrow widths a per-node gauge gets.
var eighths = []rune{' ', '▏', '▎', '▍', '▌', '▋', '▊', '▉'}

// Gauge renders a proportional bar of the given width.
//
// A negative percentage means "not measured" and renders as a dashed track
// rather than an empty bar: an empty bar reads as zero, and zero utilisation
// and unknown utilisation are very different things to a person deciding
// whether a machine is working.
func Gauge(pct float64, width int, color string) string {
	if width < 1 {
		return ""
	}
	if pct < 0 {
		return C(Grey, strings.Repeat("╌", width))
	}
	if pct > 100 {
		pct = 100
	}
	exact := pct / 100 * float64(width)
	full := int(exact)
	if full > width {
		full = width
	}
	var b strings.Builder
	b.WriteString(strings.Repeat("█", full))
	if full < width {
		frac := int((exact - float64(full)) * 8)
		if frac > 0 {
			b.WriteRune(eighths[frac])
			full++
		}
	}
	filled := b.String()
	rest := width - utf8.RuneCountInString(filled)
	if rest < 0 {
		rest = 0
	}
	return C(color, filled) + C(Grey, strings.Repeat("·", rest))
}

// sparkGlyphs are the eight heights a sparkline can draw.
var sparkGlyphs = []rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

// Sparkline renders the last width values of a series, scaled 0..100.
//
// Fixed to a 0..100 scale rather than auto-scaling to the data. An
// auto-scaled sparkline of an idle machine shows dramatic mountains of noise,
// which is actively misleading next to a gauge on the same row.
func Sparkline(values []float64, width int, color string) string {
	if width < 1 {
		return ""
	}
	if len(values) > width {
		values = values[len(values)-width:]
	}
	var b strings.Builder
	// Right-align: the newest sample belongs at the right edge, next to the
	// live gauge it is the history of.
	for i := len(values); i < width; i++ {
		b.WriteRune(' ')
	}
	for _, v := range values {
		if v < 0 {
			b.WriteRune('╌')
			continue
		}
		if v > 100 {
			v = 100
		}
		idx := int(v / 100 * float64(len(sparkGlyphs)-1))
		b.WriteRune(sparkGlyphs[idx])
	}
	return C(color, b.String())
}

// Bytes formats a byte count for a dashboard column: short, aligned, and
// never more precision than the reader can use.
func Bytes(b int64) string {
	if b < 0 {
		return "-"
	}
	switch {
	case b >= 1<<40:
		return fmt.Sprintf("%.1fT", float64(b)/(1<<40))
	case b >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0fM", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0fK", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%dB", b)
	}
}

// MiB formats a mebibyte count the same way.
func MiB(m int64) string {
	if m < 0 {
		return "-"
	}
	return Bytes(m << 20)
}

// Pct formats a percentage, or "-" when it was not measured.
func Pct(v float64) string {
	if v < 0 {
		return "   -"
	}
	return fmt.Sprintf("%3.0f%%", v)
}

// Truncate shortens s to width display cells, with an ellipsis if it had to
// cut. Width is counted in runes, which is right for the ASCII-ish content
// here and avoids pulling in a full width-table dependency.
func Truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	r := []rune(s)
	return string(r[:width-1]) + "…"
}

// Pad right-pads s to width, ignoring any escape sequences it contains.
func Pad(s string, width int) string {
	n := VisibleLen(s)
	if n >= width {
		return s
	}
	return s + strings.Repeat(" ", width-n)
}

// VisibleLen counts display cells, skipping ANSI escape sequences.
//
// Needed because every layout calculation happens on strings that already
// contain colour codes; using len() would make coloured columns mysteriously
// narrower than plain ones.
func VisibleLen(s string) int {
	n, inEsc := 0, false
	for _, r := range s {
		switch {
		case inEsc:
			if r == 'm' {
				inEsc = false
			}
		case r == '\033':
			inEsc = true
		default:
			n++
		}
	}
	return n
}

// Unknown mirrors platform.Unknown for callers that only import this package,
// so a renderer does not have to depend on the platform layer to say "this
// number was never measured".
const Unknown = -1.0
