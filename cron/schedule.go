package cron

import (
	"errors"
	"sort"
	"time"
)

// Spec is a parsed cron expression.
type Spec struct {
	expression string
	seconds    []int
	minutes    []int
	hours      []int
	days       []token
	months     []int
	weekdays   []token
}

// Parse validates expression and returns its schedule.
func Parse(expression string) (*Spec, error) {
	fields, err := convert(expression)
	if err != nil {
		return nil, err
	}
	return &Spec{
		expression: expression,
		seconds:    sortedNumbers(fields[0]),
		minutes:    sortedNumbers(fields[1]),
		hours:      sortedNumbers(fields[2]),
		days:       fields[3],
		months:     sortedNumbers(fields[4]),
		weekdays:   fields[5],
	}, nil
}

// Expression is the text the schedule was parsed from.
func (s *Spec) Expression() string { return s.expression }

func sortedNumbers(tokens []token) []int {
	out := make([]int, 0, len(tokens))
	for _, t := range tokens {
		if t.isNum {
			out = append(out, t.num)
		}
	}
	sort.Ints(out)
	return out
}

func contains(values []int, v int) bool {
	for _, x := range values {
		if x == v {
			return true
		}
	}
	return false
}

func zone(loc *time.Location) *time.Location {
	if loc == nil {
		return time.Local
	}
	return loc
}

func lastDayOfMonth(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

func nearestWeekday(year int, month time.Month, target int) int {
	last := lastDayOfMonth(year, month)
	if target < 1 || target > last {
		return -1
	}
	switch time.Date(year, month, target, 0, 0, 0, 0, time.UTC).Weekday() {
	case time.Saturday:
		if target == 1 {
			return target + 2
		}
		return target - 1
	case time.Sunday:
		if target == last {
			return target - 2
		}
		return target + 1
	}
	return target
}

func matchesDayOfMonth(field []token, year int, month time.Month, day int) bool {
	for _, t := range field {
		if t.isNum {
			if t.num == day {
				return true
			}
			continue
		}
		if t.text == "L" && day == lastDayOfMonth(year, month) {
			return true
		}
		if m := weekdayWToken.FindStringSubmatch(t.text); m != nil {
			target := lastDayOfMonth(year, month)
			if m[1] != "L" {
				target, _ = jsParseInt(m[1])
			}
			if nearestWeekday(year, month, target) == day {
				return true
			}
		}
		if m := offsetToken.FindStringSubmatch(t.text); m != nil {
			n, _ := jsParseInt(m[1])
			if target := lastDayOfMonth(year, month) - n; target >= 1 && target == day {
				return true
			}
		}
	}
	return false
}

func matchesDayOfWeek(field []token, year int, month time.Month, day int, weekday time.Weekday) bool {
	for _, t := range field {
		if t.isNum {
			if t.num == int(weekday) {
				return true
			}
			continue
		}
		if m := nthWeekdayRe.FindStringSubmatch(t.text); m != nil {
			wd, _ := jsParseInt(m[1])
			nth, _ := jsParseInt(m[2])
			if wd%7 == int(weekday) && (day-1)/7+1 == nth {
				return true
			}
			continue
		}
		if m := lastWeekdayRe.FindStringSubmatch(t.text); m != nil {
			wd, _ := jsParseInt(m[1])
			if wd == 7 {
				wd = 0
			}
			// The last such weekday is the one with no same weekday a week later.
			if wd == int(weekday) && time.Date(year, month, day+7, 0, 0, 0, 0, time.UTC).Month() != month {
				return true
			}
		}
	}
	return false
}

// Match reports whether t falls on a second the schedule fires, read on the
// wall clock of loc (the local zone when nil).
func (s *Spec) Match(t time.Time, loc *time.Location) bool {
	l := t.In(zone(loc))
	year, month, day := l.Date()
	return contains(s.seconds, l.Second()) && contains(s.minutes, l.Minute()) && contains(s.hours, l.Hour()) &&
		matchesDayOfMonth(s.days, year, month, day) && contains(s.months, int(month)) &&
		matchesDayOfWeek(s.weekdays, year, month, day, l.Weekday())
}

type wallClock struct {
	year, month, day, hour, minute, second int
}

func wallClockOf(ms int64, loc *time.Location) wallClock {
	l := time.UnixMilli(ms).In(loc)
	y, m, d := l.Date()
	return wallClock{y, int(m), d, l.Hour(), l.Minute(), l.Second()}
}

// offsetMinutes is the UTC offset of loc at ms, in whole minutes.
func offsetMinutes(ms int64, loc *time.Location) int64 {
	_, seconds := time.UnixMilli(ms).In(loc).Zone()
	return int64(seconds / 60)
}

// localTimeToMillis resolves a wall-clock time to an instant. A time that does
// not exist (a spring-forward gap) resolves to the later reading, and one that
// exists twice to the earlier, exactly as node-cron does.
func localTimeToMillis(w wallClock, loc *time.Location) int64 {
	guess := time.Date(w.year, time.Month(w.month), w.day, w.hour, w.minute, w.second, 0, time.UTC).UnixMilli()
	first := offsetMinutes(guess, loc)
	candidate1 := guess - first*60000
	second := offsetMinutes(candidate1, loc)
	if second == first {
		return candidate1
	}
	candidate2 := guess - second*60000
	if wallClockOf(candidate1, loc) == w {
		return candidate1
	}
	if wallClockOf(candidate2, loc) == w {
		return candidate2
	}
	return max(candidate1, candidate2)
}

// maxSearchDays bounds the search for the next run (a hundred years).
const maxSearchDays = 366 * 100

var errNoNextRun = errors.New("could not find next matching date within reasonable time range")

// Next returns the first second after `after` that the schedule fires, on the
// wall clock of loc (the local zone when nil).
func (s *Spec) Next(after time.Time, loc *time.Location) (time.Time, error) {
	loc = zone(loc)
	baseMs := after.UnixMilli() - mod(after.UnixMilli(), 1000)
	base := wallClockOf(baseMs, loc)
	year, month, day := base.year, base.month, base.day
	for i := 0; i < maxSearchDays; i++ {
		if contains(s.months, month) &&
			matchesDayOfMonth(s.days, year, time.Month(month), day) &&
			matchesDayOfWeek(s.weekdays, year, time.Month(month), day, time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC).Weekday()) {
			var bound *wallClock
			if i == 0 {
				bound = &base
			}
			if found, ok := s.firstTimeOnDay(year, month, day, bound, baseMs, loc); ok {
				return time.UnixMilli(found), nil
			}
		}
		next := time.Date(year, time.Month(month), day+1, 0, 0, 0, 0, time.UTC)
		year, month, day = next.Year(), int(next.Month()), next.Day()
	}
	return time.Time{}, errNoNextRun
}

func mod(a, b int64) int64 { return ((a % b) + b) % b }

func secondOfDay(h, m, s int) int { return h*3600 + m*60 + s }

func (s *Spec) firstTimeOnDay(year, month, day int, bound *wallClock, baseMs int64, loc *time.Location) (int64, bool) {
	for _, hour := range s.hours {
		if bound != nil && hour < bound.hour {
			continue
		}
		for _, minute := range s.minutes {
			for _, second := range s.seconds {
				if bound != nil && secondOfDay(hour, minute, second) <= secondOfDay(bound.hour, bound.minute, bound.second) {
					continue
				}
				ts := localTimeToMillis(wallClock{year, month, day, hour, minute, second}, loc)
				if ts > baseMs && s.Match(time.UnixMilli(ts), loc) {
					return ts, true
				}
			}
		}
	}
	return 0, false
}
