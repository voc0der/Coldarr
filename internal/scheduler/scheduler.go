// Package scheduler is the pure due-check logic behind Coldarr's in-app
// task scheduler: given a Schedule and the last time a task actually ran,
// decide whether it's due now. It has no goroutines and does no I/O - the
// ticking loop and the tasks themselves (running a plan, rescanning cold
// storage) live in internal/webui, which is what makes this package
// trivially testable with fixed time.Time values instead of real waits.
package scheduler

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Unit is how a Schedule's Every field is measured.
type Unit string

const (
	Daily  Unit = "daily"
	Hourly Unit = "hourly"
)

// Weekday is the stable, human-readable form used for weekly scheduler
// blackout days in coldarr.yaml.
type Weekday string

const (
	Sunday    Weekday = "sunday"
	Monday    Weekday = "monday"
	Tuesday   Weekday = "tuesday"
	Wednesday Weekday = "wednesday"
	Thursday  Weekday = "thursday"
	Friday    Weekday = "friday"
	Saturday  Weekday = "saturday"
)

var weekdayForTime = [...]Weekday{
	Sunday,
	Monday,
	Tuesday,
	Wednesday,
	Thursday,
	Friday,
	Saturday,
}

// Schedule is one task's recurrence: run every Every Unit(s), and for a
// Daily schedule, at clock time At.
type Schedule struct {
	Enabled bool   `yaml:"enabled"`
	Unit    Unit   `yaml:"unit"`
	Every   int    `yaml:"every"`
	At      string `yaml:"at"` // "HH:MM", 24h local time - Daily only
}

// Validate checks that an enabled schedule is well-formed. A disabled
// schedule is never rejected regardless of its other fields, so turning a
// task off never requires first fixing unrelated fields.
func Validate(s Schedule) error {
	if !s.Enabled {
		return nil
	}
	if s.Unit != Daily && s.Unit != Hourly {
		return fmt.Errorf("schedule: unit must be %q or %q, got %q", Daily, Hourly, s.Unit)
	}
	if s.Every < 1 {
		return fmt.Errorf("schedule: every must be at least 1, got %d", s.Every)
	}
	if s.Unit == Daily {
		if _, _, ok := parseHHMM(s.At); !ok {
			return fmt.Errorf("schedule: at must be in HH:MM form, got %q", s.At)
		}
	}
	return nil
}

// ValidateOmitDays checks that a weekly omit-day list only contains known
// weekdays and does not repeat one. An empty list means there is no weekly
// blackout.
func ValidateOmitDays(days []Weekday) error {
	seen := make(map[Weekday]bool, len(days))
	for _, day := range days {
		if !validWeekday(day) {
			return fmt.Errorf("weekly omit days: unknown weekday %q", day)
		}
		if seen[day] {
			return fmt.Errorf("weekly omit days: weekday %q is repeated", day)
		}
		seen[day] = true
	}
	return nil
}

// ParseOmitDays converts form values to Weekdays and validates the result.
func ParseOmitDays(values []string) ([]Weekday, error) {
	days := make([]Weekday, len(values))
	for i, value := range values {
		days[i] = Weekday(value)
	}
	if err := ValidateOmitDays(days); err != nil {
		return nil, err
	}
	return days, nil
}

// OmittedOn reports whether automated scheduler work should be suppressed
// at now. time.Weekday is location-aware through now, so this follows the
// same server-local calendar as daily schedules.
func OmittedOn(days []Weekday, now time.Time) bool {
	day := weekdayForTime[now.Weekday()]
	for _, omitted := range days {
		if omitted == day {
			return true
		}
	}
	return false
}

func validWeekday(day Weekday) bool {
	for _, candidate := range weekdayForTime {
		if day == candidate {
			return true
		}
	}
	return false
}

// Due reports whether s should fire now, given the last time it actually
// started running (the zero Time means "hasn't run this process's
// lifetime yet"). lastRun and now must share a Location.
func Due(s Schedule, lastRun, now time.Time) bool {
	if !s.Enabled {
		return false
	}
	every := s.Every
	if every < 1 {
		every = 1
	}
	if s.Unit == Hourly {
		if lastRun.IsZero() {
			return true
		}
		return !now.Before(lastRun.Add(time.Duration(every) * time.Hour))
	}
	return dueDaily(s, every, lastRun, now)
}

func dueDaily(s Schedule, every int, lastRun, now time.Time) bool {
	// Validate should already have rejected a malformed At at save time -
	// this fallback only matters for a schedule persisted before
	// validation existed, or edited by hand.
	hh, mm, ok := parseHHMM(s.At)
	if !ok {
		hh, mm = 0, 0
	}

	todayAt := time.Date(now.Year(), now.Month(), now.Day(), hh, mm, 0, 0, now.Location())
	if now.Before(todayAt) {
		return false
	}
	if lastRun.IsZero() {
		return true
	}

	// Calendar-date arithmetic (AddDate), not a 24h*every Duration - a
	// 23/25-hour DST day must never shift which calendar day counts as
	// "every days later."
	lastRunDate := time.Date(lastRun.Year(), lastRun.Month(), lastRun.Day(), 0, 0, 0, 0, now.Location())
	nextDue := lastRunDate.AddDate(0, 0, every)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	return !today.Before(nextDue)
}

func parseHHMM(s string) (hh, mm int, ok bool) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}
