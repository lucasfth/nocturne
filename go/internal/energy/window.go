package energy

import (
	"strconv"
	"strings"
	"time"
)

// aware is a Python aware datetime in a zoneinfo zone: wall time plus fold.
type aware struct {
	w    time.Time // naive wall clock (stored in UTC)
	fold int
}

func (a aware) in(loc *time.Location) time.Time { return resolve(a.w, a.fold, loc) }

func hm(s string) (int, int) {
	h, m, _ := strings.Cut(s, ":")
	hh, _ := strconv.Atoi(strings.TrimSpace(h))
	mm, _ := strconv.Atoi(strings.TrimSpace(m))
	return hh, mm
}

// Window ports Energy.window(): (opened, next_start, end). Python compares
// datetimes sharing a tzinfo by wall clock and adds timedeltas in wall
// time (result fold=0), which is reproduced here.
func (e *Energy) Window() (bool, time.Time, time.Time) {
	cfg := e.Config.Schedule
	now := e.Clock()
	if !cfg.truthy() {
		return true, now, now
	}
	loc, err := time.LoadLocation(cfg.TZ)
	if err != nil {
		loc = time.UTC
	}
	lw := wall(now.In(loc))
	f := fold(now, loc)
	sh, sm := hm(cfg.Start)
	eh, em := hm(cfg.End)
	day := func(h, m int) time.Time { return time.Date(lw.Year(), lw.Month(), lw.Day(), h, m, 0, 0, time.UTC) }
	start, end := aware{day(sh, sm), f}, aware{day(eh, em), f}
	shift := func(a aware, d int) aware { return aware{a.w.AddDate(0, 0, d), 0} }
	if !end.w.After(start.w) {
		if lw.Before(end.w) {
			start = shift(start, -1)
		} else {
			end = shift(end, 1)
		}
	}
	opened := !start.w.After(lw) && lw.Before(end.w)
	next := start
	if !lw.Before(start.w) {
		next = shift(start, 1)
	}
	if !opened && lw.Before(start.w) {
		end = shift(end, -1)
	}
	return opened, next.in(loc), end.in(loc)
}
