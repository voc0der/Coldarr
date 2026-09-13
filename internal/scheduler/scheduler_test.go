package scheduler

import (
	"testing"
	"time"
)

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		sched   Schedule
		wantErr bool
	}{
		{"disabled is never rejected", Schedule{Enabled: false, Unit: "bogus", Every: -5, At: "nope"}, false},
		{"valid daily", Schedule{Enabled: true, Unit: Daily, Every: 1, At: "03:00"}, false},
		{"valid hourly", Schedule{Enabled: true, Unit: Hourly, Every: 6}, false},
		{"unknown unit", Schedule{Enabled: true, Unit: "weekly", Every: 1, At: "03:00"}, true},
		{"every zero", Schedule{Enabled: true, Unit: Daily, Every: 0, At: "03:00"}, true},
		{"every negative", Schedule{Enabled: true, Unit: Hourly, Every: -1}, true},
		{"malformed at", Schedule{Enabled: true, Unit: Daily, Every: 1, At: "3am"}, true},
		{"at out of range", Schedule{Enabled: true, Unit: Daily, Every: 1, At: "24:00"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Validate(c.sched)
			if (err != nil) != c.wantErr {
				t.Fatalf("Validate(%+v) error = %v, wantErr %v", c.sched, err, c.wantErr)
			}
		})
	}
}

func TestValidateOmitDays(t *testing.T) {
	tests := []struct {
		name    string
		days    []Weekday
		wantErr bool
	}{
		{name: "empty"},
		{name: "one day", days: []Weekday{Monday}},
		{name: "several days", days: []Weekday{Sunday, Wednesday, Saturday}},
		{name: "unknown day", days: []Weekday{"funday"}, wantErr: true},
		{name: "duplicate day", days: []Weekday{Monday, Monday}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateOmitDays(tt.days)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateOmitDays(%v) error = %v, wantErr %v", tt.days, err, tt.wantErr)
			}
		})
	}
}

func TestOmittedOn(t *testing.T) {
	days := []Weekday{Monday, Friday}
	monday := time.Date(2026, 8, 24, 1, 0, 0, 0, time.UTC)
	tuesday := monday.AddDate(0, 0, 1)

	if !OmittedOn(days, monday) {
		t.Fatal("OmittedOn() = false on selected Monday, want true")
	}
	if OmittedOn(days, tuesday) {
		t.Fatal("OmittedOn() = true on unselected Tuesday, want false")
	}
}

func TestDue(t *testing.T) {
	loc := time.UTC
	day := func(y int, m time.Month, d, hh, mm int) time.Time {
		return time.Date(y, m, d, hh, mm, 0, 0, loc)
	}

	cases := []struct {
		name    string
		sched   Schedule
		lastRun time.Time
		now     time.Time
		want    bool
	}{
		{
			"disabled never fires",
			Schedule{Enabled: false, Unit: Daily, Every: 1, At: "03:00"},
			time.Time{}, day(2026, 1, 2, 12, 0), false,
		},
		{
			"never-run daily, before today's time",
			Schedule{Enabled: true, Unit: Daily, Every: 1, At: "03:00"},
			time.Time{}, day(2026, 1, 2, 2, 59), false,
		},
		{
			"never-run daily, past today's time",
			Schedule{Enabled: true, Unit: Daily, Every: 1, At: "03:00"},
			time.Time{}, day(2026, 1, 2, 3, 0), true,
		},
		{
			"daily already ran today, does not refire",
			Schedule{Enabled: true, Unit: Daily, Every: 1, At: "03:00"},
			day(2026, 1, 2, 3, 0), day(2026, 1, 2, 20, 0), false,
		},
		{
			"daily every=1 fires the next day",
			Schedule{Enabled: true, Unit: Daily, Every: 1, At: "03:00"},
			day(2026, 1, 2, 3, 0), day(2026, 1, 3, 3, 0), true,
		},
		{
			"daily every=2 blocks the very next day",
			Schedule{Enabled: true, Unit: Daily, Every: 2, At: "03:00"},
			day(2026, 1, 2, 3, 0), day(2026, 1, 3, 3, 0), false,
		},
		{
			"daily every=2 fires two days later",
			Schedule{Enabled: true, Unit: Daily, Every: 2, At: "03:00"},
			day(2026, 1, 2, 3, 0), day(2026, 1, 4, 3, 0), true,
		},
		{
			"malformed at falls back to 00:00",
			Schedule{Enabled: true, Unit: Daily, Every: 1, At: "garbage"},
			time.Time{}, day(2026, 1, 2, 0, 0), true,
		},
		{
			"never-run hourly fires immediately",
			Schedule{Enabled: true, Unit: Hourly, Every: 6},
			time.Time{}, day(2026, 1, 2, 0, 1), true,
		},
		{
			"hourly respects every, not yet due",
			Schedule{Enabled: true, Unit: Hourly, Every: 6},
			day(2026, 1, 2, 0, 0), day(2026, 1, 2, 5, 59), false,
		},
		{
			"hourly respects every, due",
			Schedule{Enabled: true, Unit: Hourly, Every: 6},
			day(2026, 1, 2, 0, 0), day(2026, 1, 2, 6, 0), true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Due(c.sched, c.lastRun, c.now)
			if got != c.want {
				t.Fatalf("Due() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestAnchor(t *testing.T) {
	loc := time.UTC
	day := func(y int, m time.Month, d, hh, mm int) time.Time {
		return time.Date(y, m, d, hh, mm, 0, 0, loc)
	}

	cases := []struct {
		name  string
		sched Schedule
		now   time.Time
		want  time.Time
	}{
		{
			"daily armed before today's slot anchors to yesterday's slot",
			Schedule{Enabled: true, Unit: Daily, Every: 1, At: "06:00"},
			day(2026, 9, 13, 3, 0), day(2026, 9, 12, 6, 0),
		},
		{
			"daily armed after today's slot anchors to today's slot",
			Schedule{Enabled: true, Unit: Daily, Every: 1, At: "06:00"},
			day(2026, 9, 13, 14, 0), day(2026, 9, 13, 6, 0),
		},
		{
			"daily armed exactly at the slot anchors to that slot",
			Schedule{Enabled: true, Unit: Daily, Every: 1, At: "06:00"},
			day(2026, 9, 13, 6, 0), day(2026, 9, 13, 6, 0),
		},
		{
			"daily armed before the slot on the 1st crosses the month",
			Schedule{Enabled: true, Unit: Daily, Every: 1, At: "06:00"},
			day(2026, 10, 1, 0, 30), day(2026, 9, 30, 6, 0),
		},
		{
			"hourly anchors to now",
			Schedule{Enabled: true, Unit: Hourly, Every: 6},
			day(2026, 9, 13, 3, 17), day(2026, 9, 13, 3, 17),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Anchor(c.sched, c.now); !got.Equal(c.want) {
				t.Fatalf("Anchor() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestAnchor_ArmingNeverFiresNorSkips pins the two guarantees Anchor exists
// for, end to end through Due: arming a daily task never makes it due on
// the spot, and arming it before today's slot never cancels that slot - the
// bug where a restart at 03:00 silently skipped that day's 06:00 run.
func TestAnchor_ArmingNeverFiresNorSkips(t *testing.T) {
	sched := Schedule{Enabled: true, Unit: Daily, Every: 1, At: "06:00"}
	at := func(d, hh, mm int) time.Time {
		return time.Date(2026, 9, d, hh, mm, 0, 0, time.UTC)
	}

	for _, armedAt := range []time.Time{at(13, 0, 0), at(13, 3, 0), at(13, 5, 59), at(13, 6, 0), at(13, 14, 0), at(13, 23, 59)} {
		anchor := Anchor(sched, armedAt)
		if Due(sched, anchor, armedAt) {
			t.Errorf("armed at %v: Due() = true on the spot, want false", armedAt)
		}

		nextSlot := at(13, 6, 0)
		if !armedAt.Before(nextSlot) {
			nextSlot = at(14, 6, 0)
		}
		if Due(sched, anchor, nextSlot.Add(-time.Minute)) {
			t.Errorf("armed at %v: Due() = true a minute before next slot %v, want false", armedAt, nextSlot)
		}
		if !Due(sched, anchor, nextSlot) {
			t.Errorf("armed at %v: Due() = false at next slot %v, want true", armedAt, nextSlot)
		}
	}
}

// TestDue_DST proves the daily due-check survives a real US DST
// transition (a 23-hour calendar day) by comparing calendar dates
// (AddDate) rather than a naive Duration - a lastRun on the day before
// "spring forward" plus 24 real hours would land at 11:00 the next day
// (since one wall-clock hour was skipped), which would wrongly report
// "not yet due" at 04:00 the next morning. Calendar-date arithmetic isn't
// fooled by the missing hour.
func TestDue_DST(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tzdata not available in this environment: %v", err)
	}

	// 2024-03-10 is when America/New_York springs forward at 2:00am.
	lastRun := time.Date(2024, 3, 9, 10, 0, 0, 0, loc)
	now := time.Date(2024, 3, 10, 4, 0, 0, 0, loc)

	sched := Schedule{Enabled: true, Unit: Daily, Every: 1, At: "03:30"}
	if !Due(sched, lastRun, now) {
		t.Fatalf("Due() = false across a DST transition, want true (calendar day advanced despite the 23-hour day)")
	}
}
