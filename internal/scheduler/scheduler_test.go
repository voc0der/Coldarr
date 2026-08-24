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
