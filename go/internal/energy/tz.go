// Package energy is a port of gateway/energy.py: operating window and
// cached public day-ahead prices with an injectable provider and clock.
package energy

import (
	"fmt"
	"strings"
	"time"
)

// Copenhagen is the Europe/Copenhagen zone used for price timestamps.
var Copenhagen = mustLoad("Europe/Copenhagen")

func mustLoad(name string) *time.Location {
	l, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return l
}

// wall returns the wall-clock fields of t as a naive time (in UTC).
func wall(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
}

// resolve attaches loc to a naive wall time the way a Python zoneinfo
// datetime does: fold=0 picks the earlier offset for ambiguous times and the
// pre-transition offset for nonexistent times; fold=1 picks the later one.
func resolve(w time.Time, fold int, loc *time.Location) time.Time {
	offAt := func(t time.Time) int { _, o := t.In(loc).Zone(); return o }
	before := offAt(w.Add(-12 * time.Hour))
	after := offAt(w.Add(12 * time.Hour))
	validB := offAt(w.Add(-time.Duration(before)*time.Second)) == before
	validA := offAt(w.Add(-time.Duration(after)*time.Second)) == after
	off := before
	switch {
	case validB && validA && before != after:
		if fold == 1 {
			off = after
		}
	case validA && !validB:
		off = after
	case !validA && !validB && fold == 1:
		off = after
	}
	t := w.Add(-time.Duration(off) * time.Second).In(loc)
	if !wall(t).Equal(w) { // nonexistent wall time: Python keeps it as-is
		name, _ := w.Add(-time.Duration(off)*time.Second - time.Hour).In(loc).Zone()
		t = w.Add(-time.Duration(off) * time.Second).In(time.FixedZone(name, off))
	}
	return t
}

// fold reports Python's fold attribute of t.astimezone(loc).
func fold(t time.Time, loc *time.Location) int {
	if resolve(wall(t.In(loc)), 0, loc).Equal(t) {
		return 0
	}
	return 1
}

var layouts = []string{"2006-01-02T15:04:05.999999999", "2006-01-02T15:04", "2006-01-02"}

// ParseISO mirrors datetime.fromisoformat; naive stamps are attached to
// Europe/Copenhagen (fold=0), as current_row_price does.
func ParseISO(s string) (time.Time, error) { t, _, err := parseISO(s); return t, err }

func parseISO(s string) (time.Time, bool, error) {
	s = strings.Replace(s, " ", "T", 1)
	if strings.HasSuffix(s, "Z") {
		s = s[:len(s)-1] + "+00:00"
	}
	for _, l := range layouts {
		if t, err := time.Parse(l+"Z07:00", s); err == nil {
			return t, false, nil
		}
		if t, err := time.Parse(l, s); err == nil {
			return resolve(t, 0, Copenhagen), true, nil
		}
	}
	return time.Time{}, false, fmt.Errorf("invalid isoformat string: %q", s)
}
