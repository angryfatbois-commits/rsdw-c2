package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/robfig/cron/v3"
)

type rebootMode string

const (
	rebootModeCron     rebootMode = "cron"
	rebootModeInterval rebootMode = "interval"
	rebootModeDaily    rebootMode = "daily"
)

type rebootDefinition struct {
	ServerID          string     `json:"serverId"`
	Mode              rebootMode `json:"mode"`
	Cron              string     `json:"cron,omitempty"`
	IntervalValue     int        `json:"intervalValue,omitempty"`
	IntervalUnit      string     `json:"intervalUnit,omitempty"`
	DailyTimes        []string   `json:"dailyTimes,omitempty"`
	ExecutionTimezone string     `json:"executionTimezone"`
}

type rebootTiming struct {
	mode     rebootMode
	cron     *cron.SpecSchedule
	interval time.Duration
	daily    []dailyMinute
	location *time.Location
}

type dailyMinute uint16

func parseRebootTiming(def rebootDefinition) (rebootTiming, error) {
	location, err := rebootLocation(def.ExecutionTimezone)
	if err != nil {
		return rebootTiming{}, err
	}
	timing := rebootTiming{mode: def.Mode, location: location}
	switch def.Mode {
	case rebootModeCron:
		expression := strings.TrimSpace(def.Cron)
		fields := strings.Fields(expression)
		if len(fields) != 5 {
			return rebootTiming{}, errors.New("cron must contain exactly five fields: minute hour day-of-month month day-of-week")
		}
		upper := strings.ToUpper(expression)
		if strings.HasPrefix(upper, "TZ=") || strings.HasPrefix(upper, "CRON_TZ=") || strings.HasPrefix(expression, "@") {
			return rebootTiming{}, errors.New("cron must not contain a timezone override or descriptor")
		}
		if strings.Contains(expression, "?") {
			return rebootTiming{}, errors.New("cron uses standard five-field syntax; Quartz descriptors are not supported")
		}
		parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
		parsed, err := parser.Parse(expression)
		if err != nil {
			return rebootTiming{}, fmt.Errorf("invalid cron expression: %w", err)
		}
		schedule, ok := parsed.(*cron.SpecSchedule)
		if !ok {
			return rebootTiming{}, errors.New("invalid cron expression")
		}
		// The parser defaults to time.Local. Run its civil-field search in UTC,
		// then resolve those fields explicitly in the schedule's IANA zone.
		schedule.Location = time.UTC
		timing.cron = schedule
	case rebootModeInterval:
		if def.IntervalUnit != "hours" && def.IntervalUnit != "days" {
			return rebootTiming{}, errors.New("intervalUnit must be hours or days")
		}
		if def.IntervalValue < 1 || (def.IntervalUnit == "hours" && def.IntervalValue > 8760) || (def.IntervalUnit == "days" && def.IntervalValue > 365) {
			return rebootTiming{}, errors.New("intervalValue is outside the supported range")
		}
		unit := time.Hour
		if def.IntervalUnit == "days" {
			unit = 24 * time.Hour
		}
		timing.interval = time.Duration(def.IntervalValue) * unit
	case rebootModeDaily:
		if len(def.DailyTimes) == 0 || len(def.DailyTimes) > 1440 {
			return rebootTiming{}, errors.New("dailyTimes must contain between one and 1440 times")
		}
		seen := map[dailyMinute]bool{}
		for _, raw := range def.DailyTimes {
			if len(raw) != 5 || raw[2] != ':' || raw[0] < '0' || raw[0] > '2' || raw[1] < '0' || raw[1] > '9' || raw[3] < '0' || raw[3] > '5' || raw[4] < '0' || raw[4] > '9' {
				return rebootTiming{}, fmt.Errorf("daily time %q must use HH:mm", raw)
			}
			hour := int(raw[0]-'0')*10 + int(raw[1]-'0')
			minute := int(raw[3]-'0')*10 + int(raw[4]-'0')
			if hour > 23 {
				return rebootTiming{}, fmt.Errorf("daily time %q has an invalid hour", raw)
			}
			value := dailyMinute(hour*60 + minute)
			seen[value] = true
		}
		for value := range seen {
			timing.daily = append(timing.daily, value)
		}
		sort.Slice(timing.daily, func(i, j int) bool { return timing.daily[i] < timing.daily[j] })
	default:
		return rebootTiming{}, errors.New("mode must be cron, interval, or daily")
	}
	return timing, nil
}

func rebootLocation(name string) (*time.Location, error) {
	name = strings.TrimSpace(name)
	if name == "" || name == "Local" || strings.ContainsAny(name, "\x00\r\n") || strings.HasPrefix(name, "+") || strings.HasPrefix(name, "-") {
		return nil, errors.New("executionTimezone must be a supported IANA timezone")
	}
	location, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("executionTimezone %q is not a supported IANA timezone", name)
	}
	return location, nil
}

func nextReboot(def rebootDefinition, anchor, after time.Time) (time.Time, error) {
	timing, err := parseRebootTiming(def)
	if err != nil {
		return time.Time{}, err
	}
	after = after.UTC()
	switch timing.mode {
	case rebootModeInterval:
		anchor = anchor.UTC()
		if anchor.IsZero() {
			return time.Time{}, errors.New("interval schedules require a UTC anchor")
		}
		if !anchor.Before(after) {
			return anchor.Add(timing.interval), nil
		}
		elapsed := after.Sub(anchor)
		cycles := elapsed/timing.interval + 1
		if cycles <= 0 {
			return time.Time{}, errors.New("interval calculation overflow")
		}
		next := anchor.Add(time.Duration(cycles) * timing.interval)
		if !next.After(after) || next.Before(anchor) {
			return time.Time{}, errors.New("interval calculation overflow")
		}
		return next, nil
	case rebootModeDaily:
		return nextCivilDaily(timing, after)
	case rebootModeCron:
		return nextCivilCron(timing, after)
	default:
		return time.Time{}, errors.New("unsupported reboot mode")
	}
}

func rebootPreview(def rebootDefinition, anchor, after time.Time, count int) ([]time.Time, error) {
	if count < 1 || count > 5 {
		return nil, errors.New("preview count must be between one and five")
	}
	if anchor.IsZero() {
		anchor = after.UTC()
	}
	runs := make([]time.Time, 0, count)
	cursor := after.UTC()
	for len(runs) < count {
		next, err := nextReboot(def, anchor, cursor)
		if err != nil {
			return nil, err
		}
		runs = append(runs, next)
		cursor = next
	}
	return runs, nil
}

func nextCivilDaily(timing rebootTiming, after time.Time) (time.Time, error) {
	local := after.In(timing.location)
	for day := 0; day < 366*9; day++ {
		date := local.AddDate(0, 0, day)
		year, month, dayOfMonth := date.Date()
		for _, minute := range timing.daily {
			nominal := time.Date(year, month, dayOfMonth, int(minute)/60, int(minute)%60, 0, 0, time.UTC)
			candidate, ok := resolveCivilMinute(nominal, timing.location)
			if ok && candidate.After(after) {
				return candidate, nil
			}
		}
	}
	return time.Time{}, errors.New("no daily occurrence within the supported horizon")
}

func nextCivilCron(timing rebootTiming, after time.Time) (time.Time, error) {
	localAfter := after.In(timing.location)
	cursor := time.Date(localAfter.Year(), localAfter.Month(), localAfter.Day(), localAfter.Hour(), localAfter.Minute(), localAfter.Second(), localAfter.Nanosecond(), time.UTC)
	horizon := cursor.AddDate(8, 0, 0)
	for cursor.Before(horizon) {
		nominal := timing.cron.Next(cursor)
		if nominal.IsZero() {
			// robfig/cron intentionally bounds its own search to five years. Move
			// the civil cursor forward and retain the product's longer horizon.
			cursor = cursor.AddDate(5, 0, 0).Add(-time.Minute)
			continue
		}
		if nominal.After(horizon) {
			break
		}
		candidate, ok := resolveCivilMinute(nominal, timing.location)
		if ok && candidate.After(after) {
			return candidate, nil
		}
		cursor = nominal
	}
	return time.Time{}, errors.New("cron has no occurrence within the supported eight-year horizon")
}

// resolveCivilMinute maps a wall-clock minute to the earliest matching UTC
// instant. A gap has no exact round trip and is skipped; a fold has two exact
// round trips and deliberately returns the first one.
func resolveCivilMinute(nominal time.Time, location *time.Location) (time.Time, bool) {
	year, month, day, hour, minute := nominal.Year(), nominal.Month(), nominal.Day(), nominal.Hour(), nominal.Minute()
	offsets := zoneOffsetsAround(location, nominal)
	var matches []time.Time
	for offset := range offsets {
		candidate := nominal.Add(-time.Duration(offset) * time.Second)
		local := candidate.In(location)
		if local.Year() == year && local.Month() == month && local.Day() == day && local.Hour() == hour && local.Minute() == minute && local.Second() == 0 {
			matches = append(matches, candidate.UTC())
		}
	}
	if len(matches) == 0 {
		return time.Time{}, false
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].Before(matches[j]) })
	return matches[0], true
}

func zoneOffsetsAround(location *time.Location, nominal time.Time) map[int]struct{} {
	offsets := map[int]struct{}{}
	start := nominal.Add(-72 * time.Hour)
	end := nominal.Add(72 * time.Hour)
	probe := start
	for probe.Before(end) {
		local := probe.In(location)
		_, offset := local.Zone()
		offsets[offset] = struct{}{}
		_, transition := local.ZoneBounds()
		if transition.IsZero() {
			break
		}
		next := transition.UTC()
		if !next.After(probe) {
			probe = probe.Add(time.Hour)
		} else {
			probe = next
		}
	}
	_, offset := end.In(location).Zone()
	offsets[offset] = struct{}{}
	return offsets
}

func cloneTimePtr(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
