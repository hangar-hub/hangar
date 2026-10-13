package jobs

import (
	"fmt"
	"strings"
	"time"
)

// Windows are the daily times when Pull transfers may run (pull.windows),
// such as "01:00-07:00", in server.timezone (PULL-7). A window whose end is
// before its start crosses midnight; one whose start equals its end covers
// the whole day. No windows means any time.
type Windows struct {
	spans []span
	loc   *time.Location
	specs []string
}

type span struct{ start, end int } // minutes after midnight

// ParseWindows parses window specifications of the form HH:MM-HH:MM.
func ParseWindows(specs []string, loc *time.Location) (Windows, error) {
	if loc == nil {
		loc = time.UTC
	}
	w := Windows{loc: loc}
	for _, s := range specs {
		a, b, ok := strings.Cut(strings.TrimSpace(s), "-")
		if !ok {
			return Windows{}, fmt.Errorf("pull window %q must have the form HH:MM-HH:MM", s)
		}
		start, err := parseClock(a)
		if err != nil {
			return Windows{}, fmt.Errorf("pull window %q: %w", s, err)
		}
		end, err := parseClock(b)
		if err != nil {
			return Windows{}, fmt.Errorf("pull window %q: %w", s, err)
		}
		w.spans = append(w.spans, span{start: start, end: end})
		w.specs = append(w.specs, strings.TrimSpace(s))
	}
	return w, nil
}

func parseClock(s string) (int, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("%q is not a time of day (HH:MM)", s)
	}
	return t.Hour()*60 + t.Minute(), nil
}

// Any reports whether there are no windows, so transfers may run at any time.
func (w Windows) Any() bool { return len(w.spans) == 0 }

func (s span) contains(m int) bool {
	switch {
	case s.start == s.end:
		return true
	case s.start < s.end:
		return m >= s.start && m < s.end
	default:
		return m >= s.start || m < s.end
	}
}

// Open reports whether t is inside a window.
func (w Windows) Open(t time.Time) bool {
	if w.Any() {
		return true
	}
	lt := t.In(w.loc)
	m := lt.Hour()*60 + lt.Minute()
	for _, s := range w.spans {
		if s.contains(m) {
			return true
		}
	}
	return false
}

// NextOpen returns t if a window is open at t, and otherwise the start of
// the next window. Days with a daylight saving change are handled by the
// time zone rules: a window that starts in a skipped hour opens when the
// clocks have gone forward.
func (w Windows) NextOpen(t time.Time) time.Time {
	if w.Open(t) {
		return t
	}
	lt := t.In(w.loc)
	var best time.Time
	for day := 0; day <= 2; day++ {
		d := lt.AddDate(0, 0, day)
		for _, s := range w.spans {
			c := time.Date(d.Year(), d.Month(), d.Day(), s.start/60, s.start%60, 0, 0, w.loc)
			if c.After(t) && (best.IsZero() || c.Before(best)) {
				best = c
			}
		}
	}
	return best
}

// String lists the windows, such as "01:00-07:00, 22:00-23:00".
func (w Windows) String() string { return strings.Join(w.specs, ", ") }

// WaitMessage is the progress message of a Job waiting for a window.
func (w Windows) WaitMessage() string { return "Waiting for pull window (" + w.String() + ")" }
