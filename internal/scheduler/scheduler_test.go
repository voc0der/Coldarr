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

func TestArm(t *testing.T) {
	loc := time.UTC
	day := func(d, hh, mm int) time.Time {
		return time.Date(2026, 9, d, hh, mm, 0, 0, loc)
	}
	daily := Schedule{Enabled: true, Unit: Daily, Every: 1, At: "06:00"}

	cases := []struct {
		name   string
		sched  Schedule
		anchor time.Time
		now    time.Time
		want   time.Time
	}{
		{
			"daily armed before today's slot keeps its anchor",
			daily, day(12, 6, 0), day(13, 3, 0), day(12, 6, 0),
		},
		{
			"daily armed after today's slot already ran keeps its anchor",
			daily, day(13, 6, 0), day(13, 14, 0), day(13, 6, 0),
		},
		{
			"daily armed after a slot missed while down skips to the next slot",
			daily, day(12, 6, 0), day(13, 14, 0), day(13, 6, 0),
		},
		{
			"daily armed days after its last run skips to the next slot",
			daily, day(1, 6, 0), day(13, 14, 0), day(13, 6, 0),
		},
		{
			"every=2 armed mid-cycle keeps its place",
			Schedule{Enabled: true, Unit: Daily, Every: 2, At: "06:00"},
			day(12, 6, 0), day(13, 14, 0), day(12, 6, 0),
		},
		{
			"never-run daily armed before today's slot stays never-run",
			daily, time.Time{}, day(13, 3, 0), time.Time{},
		},
		{
			"never-run daily armed after today's slot skips to the next slot",
			daily, time.Time{}, day(13, 14, 0), day(13, 6, 0),
		},
		{
			"hourly armed mid-period keeps its anchor",
			Schedule{Enabled: true, Unit: Hourly, Every: 6}, day(13, 1, 0), day(13, 3, 0), day(13, 1, 0),
		},
		{
			"hourly armed after a missed period waits one full period",
			Schedule{Enabled: true, Unit: Hourly, Every: 6}, day(13, 1, 0), day(13, 9, 17), day(13, 9, 17),
		},
		{
			"disabled keeps its anchor",
			Schedule{Enabled: false, Unit: Daily, Every: 1, At: "06:00"}, day(1, 6, 0), day(13, 14, 0), day(1, 6, 0),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Arm(c.sched, c.anchor, c.now); !got.Equal(c.want) {
				t.Fatalf("Arm() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestArm_RestartTimeNeverMatters pins the guarantee Arm exists for: a
// daily task restarted at any moment of the day, with the anchor it had
// just before the restart read back from disk, keeps exactly that anchor -
// so its next run is whatever it would have been with no restart at all -
// and is never due on the spot.
func TestArm_RestartTimeNeverMatters(t *testing.T) {
	sched := Schedule{Enabled: true, Unit: Daily, Every: 1, At: "06:00"}
	yesterdaySlot := time.Date(2026, 9, 12, 6, 0, 0, 0, time.UTC)
	todaySlot := yesterdaySlot.AddDate(0, 0, 1)

	for restartAt := todaySlot.Add(-6 * time.Hour); restartAt.Before(todaySlot.Add(18 * time.Hour)); restartAt = restartAt.Add(15 * time.Minute) {
		// The anchor a process that was up all along would hold: today's
		// run has genuinely happened once its slot has passed.
		persisted := yesterdaySlot
		if !restartAt.Before(todaySlot) {
			persisted = todaySlot
		}

		armed := Arm(sched, persisted, restartAt)
		if !armed.Equal(persisted) {
			t.Errorf("restart at %v: Arm() = %v, want unchanged %v", restartAt, armed, persisted)
		}
		if Due(sched, armed, restartAt) {
			t.Errorf("restart at %v: Due() = true on the spot, want false", restartAt)
		}
	}
}

// TestDue_PersistedAnchorInAnotherOffset proves an anchor read back from
// disk - carrying a fixed UTC offset rather than the process's Location -
// is judged on the calendar day it was recorded on in now's Location.
func TestDue_PersistedAnchorInAnotherOffset(t *testing.T) {
	loc := time.FixedZone("UTC+10", 10*60*60)
	sched := Schedule{Enabled: true, Unit: Daily, Every: 1, At: "06:00"}

	// 06:00 on the 13th in loc is 20:00 on the 12th in UTC.
	ranAt := time.Date(2026, 9, 13, 6, 0, 0, 0, loc).UTC()
	if Due(sched, ranAt, time.Date(2026, 9, 13, 14, 0, 0, 0, loc)) {
		t.Fatal("Due() = true later the same local day, want false")
	}
	if !Due(sched, ranAt, time.Date(2026, 9, 14, 6, 0, 0, 0, loc)) {
		t.Fatal("Due() = false at the next local day's slot, want true")
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

// TestParseOmitDays covers the Scheduler page's checkbox values: valid
// days come back in the order submitted, and anything a hand-crafted form
// post could send that isn't a weekday - or names one twice - is rejected.
func TestParseOmitDays(t *testing.T) {
	days, err := ParseOmitDays([]string{"saturday", "sunday"})
	if err != nil {
		t.Fatalf("ParseOmitDays: %v", err)
	}
	if len(days) != 2 || days[0] != Saturday || days[1] != Sunday {
		t.Fatalf("ParseOmitDays = %v, want [saturday sunday]", days)
	}

	if days, err := ParseOmitDays(nil); err != nil || len(days) != 0 {
		t.Fatalf("ParseOmitDays(nil) = (%v, %v), want no blackout days", days, err)
	}
	for _, bad := range [][]string{{"Saturday"}, {"funday"}, {"monday", "monday"}} {
		if days, err := ParseOmitDays(bad); err == nil {
			t.Errorf("ParseOmitDays(%q) = %v, want an error", bad, days)
		}
	}
}

// TestDueAndArm_ToleratePreValidationSchedules: a schedule saved before
// validation existed, or edited by hand, can carry Every 0 or a malformed
// At. Rather than never firing, such a schedule runs as every 1 at
// midnight.
func TestDueAndArm_ToleratePreValidationSchedules(t *testing.T) {
	now := time.Date(2026, 9, 13, 14, 0, 0, 0, time.UTC)

	hourly := Schedule{Enabled: true, Unit: Hourly, Every: 0}
	if !Due(hourly, now.Add(-time.Hour), now) {
		t.Error("an hourly schedule with every=0 should run as every 1 hour")
	}
	if Due(hourly, now.Add(-30*time.Minute), now) {
		t.Error("an hourly schedule with every=0 must still wait out its hour")
	}

	malformed := Schedule{Enabled: true, Unit: Daily, Every: 1, At: "noonish"}
	if got, want := Arm(malformed, time.Time{}, now), time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("Arm with a malformed At = %v, want today's midnight slot %v", got, want)
	}
}
