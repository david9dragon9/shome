package owner

import (
	"fmt"
	"strings"
	"time"
)

// window is one parsed availability entry, e.g. "Mon-Fri 22:00-08:00".
type window struct {
	days     [7]bool // index 0 = Sunday, matching time.Weekday
	allDay   bool
	startMin int // minutes from midnight
	endMin   int
}

var dayNames = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday,
	"wed": time.Wednesday, "thu": time.Thursday, "fri": time.Friday,
	"sat": time.Saturday,
}

// parseWindow accepts "Mon-Fri 22:00-08:00", "Sat-Sun *", "* 09:00-17:00"
// and single days like "Wed 10:00-12:00".
func parseWindow(s string) (window, error) {
	var w window
	fields := strings.Fields(strings.TrimSpace(s))
	if len(fields) != 2 {
		return w, fmt.Errorf("expected '<days> <times>', e.g. 'Mon-Fri 22:00-08:00'")
	}
	if err := parseDays(fields[0], &w); err != nil {
		return w, err
	}
	if fields[1] == "*" {
		w.allDay = true
		return w, nil
	}
	parts := strings.Split(fields[1], "-")
	if len(parts) != 2 {
		return w, fmt.Errorf("expected a time range like 22:00-08:00")
	}
	var err error
	if w.startMin, err = parseHHMM(parts[0]); err != nil {
		return w, err
	}
	if w.endMin, err = parseHHMM(parts[1]); err != nil {
		return w, err
	}
	return w, nil
}

func parseDays(spec string, w *window) error {
	if spec == "*" {
		for i := range w.days {
			w.days[i] = true
		}
		return nil
	}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if lo, hi, ok := strings.Cut(part, "-"); ok {
			a, okA := dayNames[strings.ToLower(lo)[:min(3, len(lo))]]
			b, okB := dayNames[strings.ToLower(hi)[:min(3, len(hi))]]
			if !okA || !okB {
				return fmt.Errorf("unknown day in %q", part)
			}
			// Ranges wrap: "Fri-Mon" means Fri, Sat, Sun, Mon.
			for d := a; ; d = (d + 1) % 7 {
				w.days[d] = true
				if d == b {
					break
				}
			}
			continue
		}
		d, ok := dayNames[strings.ToLower(part)[:min(3, len(part))]]
		if !ok {
			return fmt.Errorf("unknown day %q", part)
		}
		w.days[d] = true
	}
	return nil
}

func parseHHMM(s string) (int, error) {
	var h, m int
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d:%d", &h, &m); err != nil {
		return 0, fmt.Errorf("bad time %q, expected HH:MM", s)
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, fmt.Errorf("time %q is out of range", s)
	}
	return h*60 + m, nil
}

// contains reports whether t falls inside the window.
//
// A range whose end is before its start wraps past midnight -- "22:00-08:00"
// is an overnight window, which is exactly the shape someone lending a desktop
// overnight will write.
func (w window) contains(t time.Time) bool {
	mins := t.Hour()*60 + t.Minute()
	if w.allDay {
		return w.days[t.Weekday()]
	}
	if w.startMin <= w.endMin {
		return w.days[t.Weekday()] && mins >= w.startMin && mins < w.endMin
	}
	// Overnight. Before midnight it belongs to today; after midnight it
	// belongs to the window that started yesterday.
	if mins >= w.startMin {
		return w.days[t.Weekday()]
	}
	if mins < w.endMin {
		yesterday := (t.Weekday() + 6) % 7
		return w.days[yesterday]
	}
	return false
}

func inAnyWindow(specs []string, t time.Time) bool {
	for _, s := range specs {
		w, err := parseWindow(s)
		if err != nil {
			continue // validation rejects these at load time
		}
		if w.contains(t) {
			return true
		}
	}
	return false
}
