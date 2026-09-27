package main

import (
	"fmt"
	"math"
	"strconv"
	"time"
)

type DiskPressurePolicy struct {
	Enabled          bool
	ThresholdPercent float64
	Duration         time.Duration
}

type DiskPressureState struct {
	WarningOccurrence string    `json:"warningOccurrence,omitempty"`
	Runtime           string    `json:"runtime"`
	Since             time.Time `json:"since"`
	LastAt            time.Time `json:"lastAt"`
	SampleAt          time.Time `json:"sampleAt"`
}

func diskPressurePolicyFromEnv() (DiskPressurePolicy, error) {
	var policy DiskPressurePolicy
	var err error
	policy.Enabled, err = strconv.ParseBool(envOr("RSDW_DISK_PRESSURE_ENABLED", "false"))
	if err != nil {
		return policy, fmt.Errorf("RSDW_DISK_PRESSURE_ENABLED must be a boolean")
	}
	policy.ThresholdPercent, err = strconv.ParseFloat(envOr("RSDW_DISK_PRESSURE_THRESHOLD_PERCENT", "85"), 64)
	if err != nil || math.IsNaN(policy.ThresholdPercent) || math.IsInf(policy.ThresholdPercent, 0) || policy.ThresholdPercent < 0 || policy.ThresholdPercent > 100 {
		return policy, fmt.Errorf("RSDW_DISK_PRESSURE_THRESHOLD_PERCENT must be between 0 and 100")
	}
	policy.Duration, err = time.ParseDuration(envOr("RSDW_DISK_PRESSURE_DURATION", "10m"))
	if err != nil || policy.Duration <= 0 {
		return policy, fmt.Errorf("RSDW_DISK_PRESSURE_DURATION must be a positive Go duration")
	}
	return policy, nil
}

func diskPressureOccurrence(serverID string, pressure DiskPressureState) string {
	return fmt.Sprintf("disk-pressure/%s/%s/%s", serverID, pressure.Runtime, pressure.Since.UTC().Format(time.RFC3339Nano))
}

// observe tracks sustained disk usage above policy.ThresholdPercent and emits a
// single DiskPressureWarning once the pressure has held continuously for
// policy.Duration. Unlike memory pressure, disk pressure never restarts the
// server: a restart does not free disk space, so this is warn-only. The
// warning re-arms the next time a fresh sample drops below threshold and
// crosses it again, mirroring the streak-reset semantics of memory pressure.
func (policy DiskPressurePolicy) observe(state *State, server Server, o observation, now time.Time) error {
	p := state.Producers[server.ID]
	streak := p.DiskPressure
	p.DiskPressure = nil
	state.Producers[server.ID] = p
	if !policy.Enabled || state.deleting(server.ID) || server.Status == StatusStopped || server.Status == StatusDeleting || o.status == StatusStopped {
		return nil
	}
	if o.runtime == "" || o.at.IsZero() || o.at.After(now) || now.Sub(o.at) > telemetryMaxAge {
		return nil
	}
	reading := freshMetrics(o.metrics, now)["diskPercent"]
	if reading.Status != "available" || reading.Value == nil || reading.ObservedAt == nil {
		return nil
	}
	if *reading.Value < policy.ThresholdPercent {
		return nil
	}
	if streak != nil && !o.at.After(streak.LastAt) {
		return nil
	}
	sampleAt := *reading.ObservedAt
	if sampleAt.After(now) {
		return nil
	}
	if streak == nil || streak.Runtime != o.runtime || o.at.Sub(streak.LastAt) > telemetryMaxAge || streak.SampleAt.IsZero() || sampleAt.Before(streak.SampleAt) || sampleAt.Sub(streak.SampleAt) > telemetryMaxAge {
		streak = &DiskPressureState{Runtime: o.runtime, Since: o.at}
	}
	advanced := sampleAt.After(streak.SampleAt)
	streak.SampleAt = sampleAt
	streak.LastAt = o.at
	if streak.WarningOccurrence == "" && advanced && sampleAt.Sub(streak.Since) >= policy.Duration {
		streak.WarningOccurrence = diskPressureOccurrence(server.ID, *streak)
		details := fmt.Sprintf("Disk usage has stayed at or above %.0f%% for at least %s", *reading.Value, policy.Duration)
		emitAlertEvidence(state, server, DiskPressureWarning, "", details, o.at, AlertEvidence{DiskPressure: &DiskPressureEvidence{Percent: *reading.Value, ThresholdPercent: policy.ThresholdPercent}}, "", streak.WarningOccurrence)
	}
	p.DiskPressure = streak
	state.Producers[server.ID] = p
	return nil
}
