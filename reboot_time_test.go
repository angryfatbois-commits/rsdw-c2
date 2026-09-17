package main

import (
	"strings"
	"testing"
	"time"
)

func rebootAt(raw string) time.Time {
	value, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		panic(err)
	}
	return value
}

func TestRebootCronAndDailyShareCivilDSTRules(t *testing.T) {
	for _, definition := range []rebootDefinition{
		{Mode: rebootModeCron, Cron: "30 2 * * *", ExecutionTimezone: "America/New_York"},
		{Mode: rebootModeDaily, DailyTimes: []string{"02:30"}, ExecutionTimezone: "America/New_York"},
	} {
		next, err := nextReboot(definition, time.Time{}, rebootAt("2027-03-14T00:00:00Z"))
		if err != nil {
			t.Fatal(err)
		}
		if want := rebootAt("2027-03-15T06:30:00Z"); !next.Equal(want) {
			t.Fatalf("spring gap next = %s, want %s", next, want)
		}
	}
	cronDefinition := rebootDefinition{Mode: rebootModeCron, Cron: "0 5 * * *", ExecutionTimezone: "America/New_York"}
	if next, err := nextReboot(cronDefinition, time.Time{}, rebootAt("2026-09-17T08:00:00Z")); err != nil {
		t.Fatal(err)
	} else if want := rebootAt("2026-09-17T09:00:00Z"); !next.Equal(want) {
		t.Fatalf("same-day non-UTC cron = %s, want %s", next, want)
	}

	definition := rebootDefinition{Mode: rebootModeDaily, DailyTimes: []string{"01:30"}, ExecutionTimezone: "America/New_York"}
	next, err := nextReboot(definition, time.Time{}, rebootAt("2026-10-31T06:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if want := rebootAt("2026-11-01T05:30:00Z"); !next.Equal(want) {
		t.Fatalf("fall fold next = %s, want first %s", next, want)
	}
	if next, err = nextReboot(definition, time.Time{}, rebootAt("2026-11-01T05:45:00Z")); err != nil {
		t.Fatal(err)
	} else if want := rebootAt("2026-11-02T06:30:00Z"); !next.Equal(want) {
		t.Fatalf("cursor between fold copies = %s, want %s", next, want)
	}
	cronFold := rebootDefinition{Mode: rebootModeCron, Cron: "30 1 * * *", ExecutionTimezone: "America/New_York"}
	if next, err := nextReboot(cronFold, time.Time{}, rebootAt("2026-10-31T06:00:00Z")); err != nil {
		t.Fatal(err)
	} else if want := rebootAt("2026-11-01T05:30:00Z"); !next.Equal(want) {
		t.Fatalf("cron fall fold next = %s, want first %s", next, want)
	}
}

func TestRebootIntervalsUseElapsedUTCAnchor(t *testing.T) {
	definition := rebootDefinition{Mode: rebootModeInterval, IntervalValue: 1, IntervalUnit: "days", ExecutionTimezone: "America/New_York"}
	anchor := rebootAt("2027-03-13T12:00:00Z")
	next, err := nextReboot(definition, anchor, anchor)
	if err != nil {
		t.Fatal(err)
	}
	if want := rebootAt("2027-03-14T12:00:00Z"); !next.Equal(want) {
		t.Fatalf("interval across spring transition = %s, want %s", next, want)
	}
	next, err = nextReboot(definition, anchor, rebootAt("2027-03-14T12:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if want := rebootAt("2027-03-15T12:00:00Z"); !next.Equal(want) {
		t.Fatalf("strict interval next = %s, want %s", next, want)
	}
}

func TestRebootTimingValidation(t *testing.T) {
	valid := rebootDefinition{Mode: rebootModeCron, Cron: "0 5 * * SUN", ExecutionTimezone: "UTC"}
	if _, err := parseRebootTiming(valid); err != nil {
		t.Fatal(err)
	}
	for _, expression := range []string{"0 5 * JUL WED", "0 5 * 7 SUN"} {
		definition := valid
		definition.Cron = expression
		if _, err := parseRebootTiming(definition); err != nil {
			t.Errorf("cron name expression %q rejected: %v", expression, err)
		}
	}
	for _, expression := range []string{"0 0 0 * * *", "@daily", "TZ=UTC 0 5 * * *", "0 0 ? * *", "0 0 * * 7"} {
		definition := valid
		definition.Cron = expression
		if _, err := parseRebootTiming(definition); err == nil {
			t.Errorf("cron %q was accepted", expression)
		}
	}
	for _, definition := range []rebootDefinition{
		{Mode: rebootModeInterval, IntervalValue: 0, IntervalUnit: "hours", ExecutionTimezone: "UTC"},
		{Mode: rebootModeInterval, IntervalValue: 366, IntervalUnit: "days", ExecutionTimezone: "UTC"},
		{Mode: rebootModeDaily, DailyTimes: []string{"24:00"}, ExecutionTimezone: "UTC"},
		{Mode: rebootModeDaily, DailyTimes: []string{"05:00:00"}, ExecutionTimezone: "UTC"},
		{Mode: rebootModeDaily, DailyTimes: []string{"05:00"}, ExecutionTimezone: "Not/AZone"},
	} {
		if _, err := parseRebootTiming(definition); err == nil {
			t.Errorf("definition %+v was accepted", definition)
		}
	}
	definition := rebootDefinition{Mode: rebootModeDaily, DailyTimes: []string{"17:00", "05:00", "05:00"}, ExecutionTimezone: "UTC"}
	timing, err := parseRebootTiming(definition)
	if err != nil {
		t.Fatal(err)
	}
	if got := timing.daily; len(got) != 2 || got[0] != 300 || got[1] != 1020 {
		t.Fatalf("daily normalization = %v", got)
	}
	if _, err := rebootLocation("Local"); err == nil || !strings.Contains(err.Error(), "IANA") {
		t.Fatalf("Local timezone error = %v", err)
	}
}

func TestRebootPreviewReturnsStrictlyIncreasingUTCInstants(t *testing.T) {
	definition := rebootDefinition{Mode: rebootModeDaily, DailyTimes: []string{"05:00", "17:00"}, ExecutionTimezone: "UTC"}
	runs, err := rebootPreview(definition, time.Time{}, rebootAt("2026-09-17T06:00:00Z"), 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 5 {
		t.Fatalf("preview length = %d", len(runs))
	}
	for i := 1; i < len(runs); i++ {
		if !runs[i].After(runs[i-1]) || runs[i].Location() != time.UTC {
			t.Fatalf("preview runs are not increasing UTC values: %v", runs)
		}
	}
}
