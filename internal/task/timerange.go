package task

import (
	"fmt"
	"time"
)

// Range is a half-open span of time: Start is included, End is not. Half-open
// ranges tile with no gap and no overlap: "today" ends exactly where
// "tomorrow" begins, so an event at midnight belongs to exactly one day.
type Range struct {
	Start, End time.Time
	Label      string
}

// Contains reports whether t is inside the range.
func (r Range) Contains(t time.Time) bool {
	return !t.Before(r.Start) && t.Before(r.End)
}

// ValidWhen reports whether s is one of the supported time words.
func ValidWhen(s string) bool {
	switch s {
	case "today", "yesterday", "this_week":
		return true
	}
	return false
}

// RangeFor turns a time word into a concrete range, relative to now.
//
// Days are computed with calendar arithmetic (AddDate), never by adding 24
// hours: on a day when the clocks change, a day is 23 or 25 hours long, and
// "+24h" would land on the wrong date. The week starts on Monday.
//
// The model never does this arithmetic. It only says "yesterday"; the code
// decides what that means.
func RangeFor(when string, now time.Time) (Range, error) {
	y, m, d := now.Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	switch when {
	case "today":
		return Range{today, today.AddDate(0, 0, 1), "today"}, nil
	case "yesterday":
		return Range{today.AddDate(0, 0, -1), today, "yesterday"}, nil
	case "this_week":
		back := (int(today.Weekday()) + 6) % 7 // Monday is 0 days back
		start := today.AddDate(0, 0, -back)
		return Range{start, start.AddDate(0, 0, 7), "this week"}, nil
	}
	return Range{}, fmt.Errorf("when %q is not one of today, yesterday, this_week", when)
}
