package main

import (
	"fmt"
	"math"
	"strconv"
	"time"
)

type MemoryPressurePolicy struct {
	Enabled          bool
	ThresholdPercent float64
	Duration         time.Duration
}

type MemoryPressureState struct {
	Runtime string    `json:"runtime"`
	Since   time.Time `json:"since"`
	LastAt  time.Time `json:"lastAt"`
}

func memoryPressurePolicyFromEnv() (MemoryPressurePolicy, error) {
	var policy MemoryPressurePolicy
	var err error
	policy.Enabled, err = strconv.ParseBool(envOr("RSDW_MEMORY_PRESSURE_ENABLED", "false"))
	if err != nil {
		return policy, fmt.Errorf("RSDW_MEMORY_PRESSURE_ENABLED must be a boolean")
	}
	policy.ThresholdPercent, err = strconv.ParseFloat(envOr("RSDW_MEMORY_PRESSURE_THRESHOLD_PERCENT", "85"), 64)
	if err != nil || math.IsNaN(policy.ThresholdPercent) || math.IsInf(policy.ThresholdPercent, 0) || policy.ThresholdPercent < 0 || policy.ThresholdPercent > 100 {
		return policy, fmt.Errorf("RSDW_MEMORY_PRESSURE_THRESHOLD_PERCENT must be between 0 and 100")
	}
	policy.Duration, err = time.ParseDuration(envOr("RSDW_MEMORY_PRESSURE_DURATION", "10m"))
	if err != nil || policy.Duration <= 0 {
		return policy, fmt.Errorf("RSDW_MEMORY_PRESSURE_DURATION must be a positive Go duration")
	}
	return policy, nil
}

func (policy MemoryPressurePolicy) observe(state *State, server Server, o observation, now time.Time) (rebootDispatch, error) {
	p := state.Producers[server.ID]
	streak := p.MemoryPressure
	p.MemoryPressure = nil
	state.Producers[server.ID] = p
	if !policy.Enabled || state.deleting(server.ID) || server.Status == StatusStopped || server.Status == StatusDeleting || o.status == StatusStopped || p.Restart != nil {
		return rebootDispatch{}, nil
	}
	if o.runtime == "" || o.at.IsZero() || o.at.After(now) || now.Sub(o.at) > telemetryMaxAge {
		return rebootDispatch{}, nil
	}
	metrics := freshMetrics(o.metrics, now)
	used, limit := metrics["memoryUsedBytes"], metrics["memoryLimitBytes"]
	if used.Status != "available" || limit.Status != "available" || used.Value == nil || limit.Value == nil || *limit.Value <= 0 {
		return rebootDispatch{}, nil
	}
	if *used.Value / *limit.Value * 100 < policy.ThresholdPercent {
		return rebootDispatch{}, nil
	}
	if streak != nil && !o.at.After(streak.LastAt) {
		return rebootDispatch{}, nil
	}
	if streak == nil || streak.Runtime != o.runtime || o.at.Sub(streak.LastAt) > telemetryMaxAge {
		streak = &MemoryPressureState{Runtime: o.runtime, Since: o.at}
	}
	streak.LastAt = o.at
	p.MemoryPressure = streak
	state.Producers[server.ID] = p
	if o.at.Sub(streak.Since) < policy.Duration {
		return rebootDispatch{}, nil
	}
	return reserveRestart(state, server.ID, now, MemoryPressureRestartRequested)
}
