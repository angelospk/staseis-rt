package gtfs

import (
	"slices"
	"time"
)

func yyyymmdd(d time.Time) int32 { return int32(d.Year()*10000 + int(d.Month())*100 + d.Day()) }

func fromYYYYMMDD(v int32) time.Time {
	return time.Date(int(v/10000), time.Month(v/100%100), int(v%100), 0, 0, 0, 0, Athens)
}

// Midnight returns 00:00 Athens time of the calendar day of t (in Athens).
func Midnight(t time.Time) time.Time {
	t = t.In(Athens)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, Athens)
}

// ServiceDay returns the time reference of the Athens calendar day of t: noon minus 12 h.
// GTFS stop times count from it. It is midnight except on DST change days (then 01:00 or
// 23:00 the evening before), so it must not be fed back into ServiceDay.
func ServiceDay(t time.Time) time.Time {
	t = t.In(Athens)
	return time.Date(t.Year(), t.Month(), t.Day(), 12, 0, 0, 0, Athens).Add(-12 * time.Hour)
}

// dayDate is the calendar date of a ServiceDay reference (its noon).
func dayDate(day time.Time) time.Time { return day.Add(12 * time.Hour).In(Athens) }

// ServiceDate formats a service day as GTFS YYYYMMDD.
func ServiceDate(day time.Time) string { return dayDate(day).Format("20060102") }

// ServiceActive reports whether service runs on the service day `day` (a ServiceDay).
// Outside the feed's validity (an expired feed not yet replaced by OASA), the same weekday
// whole weeks inside it decides (PlanningTime); exceptions apply to their own date only.
func (f *Feed) ServiceActive(service int32, day time.Time) bool {
	date := dayDate(day)
	s := &f.Services[service]
	if s.End == 0 {
		// No calendar row (OSETH feeds list every running date in calendar_dates.txt): the
		// dates are the schedule, so an expired feed reads them at the same weekday inside it.
		return f.Exceptions[int64(service)<<32|int64(yyyymmdd(f.PlanningTime(date)))] == 1
	}
	d := yyyymmdd(date)
	if exc, ok := f.Exceptions[int64(service)<<32|int64(d)]; ok {
		return exc == 1
	}
	wd := (int(date.Weekday()) + 6) % 7 // Monday = 0
	d = yyyymmdd(f.PlanningTime(date))
	return s.Start <= d && d <= s.End && s.Days[wd]
}

// FeedEnd is the last calendar end_date, or the zero time without a calendar. A feed with
// no calendar rows at all ends on its last added date (calendar_dates.txt).
func (f *Feed) FeedEnd() time.Time {
	var end int32
	for _, s := range f.Services {
		end = max(end, s.End)
	}
	if end == 0 && f.calendarLess() {
		_, end = f.addedSpan()
	}
	if end == 0 {
		return time.Time{}
	}
	return fromYYYYMMDD(end)
}

// FeedStart is the first calendar start_date, or the zero time without a calendar. A feed
// with no calendar rows at all starts on its first added date (calendar_dates.txt).
func (f *Feed) FeedStart() time.Time {
	var start int32
	for _, s := range f.Services {
		if start == 0 || s.Start < start {
			start = s.Start
		}
	}
	if start == 0 && f.calendarLess() {
		start, _ = f.addedSpan()
	}
	if start == 0 {
		return time.Time{}
	}
	return fromYYYYMMDD(start)
}

// calendarLess reports a feed whose services all come from calendar_dates.txt.
func (f *Feed) calendarLess() bool {
	for _, s := range f.Services {
		if s.End != 0 {
			return false
		}
	}
	return len(f.Services) > 0
}

// addedSpan is the first and last date a service is added (exception type 1), or 0, 0.
func (f *Feed) addedSpan() (first, last int32) {
	for k, exc := range f.Exceptions {
		if exc != 1 {
			continue
		}
		d := int32(k & 0xffffffff)
		if first == 0 || d < first {
			first = d
		}
		last = max(last, d)
	}
	return first, last
}

// PlanningTime maps now into the feed's validity by whole weeks (same weekday and wall
// time), so an expired or not yet valid feed still tells which lines usually run now. Inside
// the validity it returns now.
func (f *Feed) PlanningTime(now time.Time) time.Time {
	start, end := f.FeedStart(), f.FeedEnd()
	if start.IsZero() || end.Sub(start) < 6*24*time.Hour {
		return now
	}
	now = now.In(Athens)
	for Midnight(now).After(end) {
		now = now.AddDate(0, 0, -7)
	}
	for Midnight(now).Before(start) {
		now = now.AddDate(0, 0, 7)
	}
	return now
}

// Expired reports whether now lies outside the feed's validity (PlanningTime moves it).
func (f *Feed) Expired(now time.Time) bool { return !f.PlanningTime(now).Equal(now) }

// ShapesAround returns the shapes of the line's trips whose run, padded as in CandidateTrips,
// covers the time of day of now on any service day: what may run around now whatever the
// calendar says (the OASA GTFS puts some weekly variants on the wrong weekday).
func (f *Feed) ShapesAround(line string, now time.Time, before, after int32) map[int32]bool {
	out := map[int32]bool{}
	today := ServiceDay(now)
	for _, day := range []time.Time{today, ServiceDay(dayDate(today).Add(-24 * time.Hour))} {
		secs := int32(now.Sub(day) / time.Second)
		for _, ti := range f.byStart[line] {
			t := &f.Trips[ti]
			if f.Start(t)-before > secs {
				break // sorted by start
			}
			if secs <= f.End(t)+after {
				out[t.Shape] = true
			}
		}
	}
	return out
}

// Candidate is a trip whose scheduled run (padded) covers a moment.
type Candidate struct {
	Trip int32
	Day  time.Time // ServiceDay of the service day; stop times count from it
}

// CandidateTrips returns trips of a line whose run, padded by `before` seconds before the
// start and `after` seconds after the end, covers `now`. Today's and yesterday's service days
// are both checked, since GTFS times past 24:00 belong to the previous service day.
func (f *Feed) CandidateTrips(line string, now time.Time, before, after int32) []Candidate {
	return f.CandidateTripsShifted(line, now, before, after, 0)
}

// CandidateTripsShifted is CandidateTrips with the calendar of the service day `shift` days
// away: the trips that run then, at today's times (Candidate.Day stays today's service day).
func (f *Feed) CandidateTripsShifted(line string, now time.Time, before, after int32, shift int) []Candidate {
	var out []Candidate
	today := ServiceDay(now)
	for _, day := range []time.Time{today, ServiceDay(dayDate(today).Add(-24 * time.Hour))} {
		first := len(out)
		secs := int32(now.Sub(day) / time.Second)
		calendar := day
		if shift != 0 {
			calendar = ServiceDay(dayDate(day).AddDate(0, 0, shift))
		}
		for _, ti := range f.byStart[line] {
			t := &f.Trips[ti]
			start := f.Start(t)
			if start-before > secs {
				break // sorted by start
			}
			if secs <= f.End(t)+after && f.ServiceActive(t.Service, calendar) {
				out = append(out, Candidate{Trip: ti, Day: day})
			}
		}
		// Report in file order within a day, as upstream does (it decides ties later).
		slices.SortFunc(out[first:], func(a, b Candidate) int { return int(a.Trip - b.Trip) })
	}
	return out
}
