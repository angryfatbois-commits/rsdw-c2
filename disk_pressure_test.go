package main

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestDiskPressurePolicyFromEnv(t *testing.T) {
	for _, name := range []string{"RSDW_DISK_PRESSURE_ENABLED", "RSDW_DISK_PRESSURE_THRESHOLD_PERCENT", "RSDW_DISK_PRESSURE_DURATION"} {
		t.Setenv(name, "")
	}
	got, err := diskPressurePolicyFromEnv()
	if err != nil || got != (DiskPressurePolicy{ThresholdPercent: 85, Duration: 10 * time.Minute}) {
		t.Fatalf("default policy = %+v, %v", got, err)
	}
	for _, tc := range []struct {
		name, value string
		valid       bool
	}{
		{"ENABLED", "true", true}, {"ENABLED", "false", true}, {"ENABLED", "yes", false},
		{"THRESHOLD_PERCENT", "0", true}, {"THRESHOLD_PERCENT", "100", true}, {"THRESHOLD_PERCENT", "85.5", true},
		{"THRESHOLD_PERCENT", "-1", false}, {"THRESHOLD_PERCENT", "101", false}, {"THRESHOLD_PERCENT", "NaN", false},
		{"THRESHOLD_PERCENT", "Inf", false}, {"THRESHOLD_PERCENT", "bad", false},
		{"DURATION", "1m30s", true}, {"DURATION", "1ns", true}, {"DURATION", "0s", false},
		{"DURATION", "-1m", false}, {"DURATION", "10", false}, {"DURATION", "bad", false},
		{"DURATION", "99999999999999999999h", false},
	} {
		t.Run(tc.name+"="+tc.value, func(t *testing.T) {
			t.Setenv("RSDW_DISK_PRESSURE_"+tc.name, tc.value)
			_, err := diskPressurePolicyFromEnv()
			if (err == nil) != tc.valid || err != nil && !strings.Contains(err.Error(), "RSDW_DISK_PRESSURE_"+tc.name) {
				t.Fatalf("parse error = %v, valid = %t", err, tc.valid)
			}
		})
	}
	t.Setenv("RSDW_DISK_PRESSURE_ENABLED", "true")
	t.Setenv("RSDW_DISK_PRESSURE_THRESHOLD_PERCENT", "90.5")
	t.Setenv("RSDW_DISK_PRESSURE_DURATION", "1m30s")
	got, err = diskPressurePolicyFromEnv()
	if err != nil || got != (DiskPressurePolicy{Enabled: true, ThresholdPercent: 90.5, Duration: 90 * time.Second}) {
		t.Fatalf("configured policy = %+v, %v", got, err)
	}
}

func diskPressureSample(at time.Time) observation {
	o := alertSample(at, "healthy", "runtime", -1)
	o.status = StatusOnline
	setReading(o.metrics, "diskPercent", 85, at)
	return o
}

func observeDiskPressure(t *testing.T, policy DiskPressurePolicy, state *State, server Server, o observation, now time.Time) {
	t.Helper()
	observeAlerts(state, server, o, now)
	if err := policy.observe(state, server, o, now); err != nil {
		t.Fatal(err)
	}
}

func TestDiskPressureWarningAfterSustainedDuration(t *testing.T) {
	state, server, start := alertFixture(t)
	integration := state.Integrations["bot"]
	integration.Rules[DiskPressureWarning] = true
	state.Integrations["bot"] = integration
	policy := DiskPressurePolicy{Enabled: true, ThresholdPercent: 85, Duration: time.Minute}

	observeDiskPressure(t, policy, state, server, diskPressureSample(start), start)
	if len(state.Events) != 0 {
		t.Fatalf("premature disk pressure warning = %+v", state.Events)
	}
	observeDiskPressure(t, policy, state, server, diskPressureSample(start.Add(30*time.Second)), start.Add(30*time.Second))
	if len(state.Events) != 0 {
		t.Fatalf("premature disk pressure warning at half duration = %+v", state.Events)
	}
	observeDiskPressure(t, policy, state, server, diskPressureSample(start.Add(time.Minute)), start.Add(time.Minute))
	if len(state.Events) != 1 || state.Events[0].Kind != DiskPressureWarning {
		t.Fatalf("disk pressure warning = %+v", state.Events)
	}
	warning := state.Events[0]
	if warning.Evidence.DiskPressure == nil || warning.Evidence.DiskPressure.Percent != 85 || warning.Evidence.DiskPressure.ThresholdPercent != 85 || warning.OccurrenceID == "" || warning.Severity != "warning" || len(state.Deliveries) != 1 {
		t.Fatalf("disk pressure warning evidence = %+v", warning)
	}

	// Sustained pressure past the initial warning must not emit a second one.
	observeDiskPressure(t, policy, state, server, diskPressureSample(start.Add(90*time.Second)), start.Add(90*time.Second))
	if len(state.Events) != 1 {
		t.Fatalf("disk pressure warning re-fired while sustained: %+v", eventKinds(state))
	}
}

func TestDiskPressureRearmsAfterClearing(t *testing.T) {
	state, server, start := alertFixture(t)
	integration := state.Integrations["bot"]
	integration.Rules[DiskPressureWarning] = true
	state.Integrations["bot"] = integration
	policy := DiskPressurePolicy{Enabled: true, ThresholdPercent: 85, Duration: time.Minute}

	observeDiskPressure(t, policy, state, server, diskPressureSample(start), start)
	observeDiskPressure(t, policy, state, server, diskPressureSample(start.Add(time.Minute)), start.Add(time.Minute))
	if len(state.Events) != 1 {
		t.Fatalf("first disk pressure warning = %+v", eventKinds(state))
	}

	cleared := diskPressureSample(start.Add(90 * time.Second))
	setReading(cleared.metrics, "diskPercent", 50, cleared.at)
	observeDiskPressure(t, policy, state, server, cleared, cleared.at)
	if p := state.Producers[server.ID]; p.DiskPressure != nil {
		t.Fatalf("disk pressure streak retained after clearing: %+v", p.DiskPressure)
	}

	resumed := start.Add(90*time.Second + time.Minute)
	observeDiskPressure(t, policy, state, server, diskPressureSample(resumed), resumed)
	if len(state.Events) != 2 || eventKinds(state)[1] != DiskPressureWarning {
		t.Fatalf("disk pressure did not rearm: %+v", eventKinds(state))
	}
}

func TestDiskPressureNeverRestarts(t *testing.T) {
	state, server, start := alertFixture(t)
	policy := DiskPressurePolicy{Enabled: true, ThresholdPercent: 85, Duration: time.Minute}
	for _, offset := range []time.Duration{0, time.Minute, 2 * time.Minute, 3 * time.Minute} {
		o := diskPressureSample(start.Add(offset))
		observeDiskPressure(t, policy, state, server, o, o.at)
	}
	if p := state.Producers[server.ID]; p.Restart != nil {
		t.Fatalf("disk pressure claimed a restart: %+v", p.Restart)
	}
	for _, kind := range eventKinds(state) {
		if kind != DiskPressureWarning {
			t.Fatalf("unexpected event kind from disk pressure: %v", kind)
		}
	}
}

func TestDiskPressureResetsContinuity(t *testing.T) {
	for _, tc := range []string{"low", "negative", "NaN", "infinity", "missing", "unavailable", "stale metric", "missing timestamp", "future metric", "stale observation", "future observation", "zero observation", "gap", "duplicate", "out of order", "runtime change", "missing runtime", "stopped", "observed stopped", "deleting", "disabled"} {
		t.Run(tc, func(t *testing.T) {
			state, server, start := alertFixture(t)
			policy := DiskPressurePolicy{Enabled: true, ThresholdPercent: 85, Duration: time.Minute}
			for _, offset := range []time.Duration{0, 30 * time.Second} {
				o := diskPressureSample(start.Add(offset))
				observeDiskPressure(t, policy, state, server, o, o.at)
			}
			o := diskPressureSample(start.Add(time.Minute))
			now := o.at
			switch tc {
			case "low":
				setReading(o.metrics, "diskPercent", 84.99, o.at)
			case "negative", "NaN", "infinity":
				value := map[string]float64{"negative": -1, "NaN": math.NaN(), "infinity": math.Inf(1)}[tc]
				setReading(o.metrics, "diskPercent", value, o.at)
			case "missing":
				delete(o.metrics, "diskPercent")
			case "unavailable", "stale metric", "missing timestamp", "future metric":
				reading := o.metrics["diskPercent"]
				switch tc {
				case "unavailable":
					reading.Status = "unavailable"
				case "stale metric":
					reading.ObservedAt = cloneTimePtr(&start)
				case "missing timestamp":
					reading.ObservedAt = nil
				case "future metric":
					future := now.Add(6 * time.Second)
					reading.ObservedAt = &future
				}
				o.metrics["diskPercent"] = reading
			case "stale observation":
				now = o.at.Add(telemetryMaxAge + time.Nanosecond)
			case "future observation":
				now = o.at.Add(-time.Nanosecond)
			case "zero observation":
				o.at = time.Time{}
			case "gap":
				o = diskPressureSample(start.Add(76 * time.Second))
				now = o.at
			case "duplicate":
				o.at = start.Add(30 * time.Second)
			case "out of order":
				o.at = start.Add(29 * time.Second)
			case "runtime change":
				o.runtime = "replacement"
			case "missing runtime":
				o.runtime = ""
			case "stopped":
				server.Status = StatusStopped
			case "observed stopped":
				o.status = StatusStopped
			case "deleting":
				server.Status = StatusDeleting
			case "disabled":
				policy.Enabled = false
			}
			observeDiskPressure(t, policy, state, server, o, now)
			if len(state.Events) != 0 {
				t.Fatal("interrupted disk pressure emitted a warning", eventKinds(state))
			}
			p := state.Producers[server.ID]
			if tc != "gap" && tc != "runtime change" && p.DiskPressure != nil {
				t.Fatal("invalid evidence retained the disk pressure streak")
			}
			state.Deletions = nil
			server.Status = StatusOnline
			policy.Enabled = true
			next := now.Add(15 * time.Second)
			o = diskPressureSample(next)
			if tc == "runtime change" {
				o.runtime = "replacement"
			}
			observeDiskPressure(t, policy, state, server, o, next)
			if len(state.Events) != 0 {
				t.Fatal("disk pressure resumed the interrupted streak")
			}
		})
	}
}

func TestDiskPressureUsesSourceTimeAndInclusiveThreshold(t *testing.T) {
	for _, threshold := range []float64{0, 85, 100} {
		state, server, start := alertFixture(t)
		integration := state.Integrations["bot"]
		integration.Rules[DiskPressureWarning] = true
		state.Integrations["bot"] = integration
		policy := DiskPressurePolicy{Enabled: true, ThresholdPercent: threshold, Duration: time.Minute}
		o := diskPressureSample(start)
		setReading(o.metrics, "diskPercent", threshold, start)
		observeDiskPressure(t, policy, state, server, o, start)
		next := diskPressureSample(start.Add(time.Minute))
		setReading(next.metrics, "diskPercent", threshold, start.Add(time.Minute))
		observeDiskPressure(t, policy, state, server, next, start.Add(time.Minute))
		if len(state.Events) != 1 || state.Events[0].Kind != DiskPressureWarning {
			t.Fatalf("threshold %v: events = %v", threshold, eventKinds(state))
		}
	}
}

func TestDiskPressurePersistenceRecoveryAndClone(t *testing.T) {
	app := newTestApp(t, false)
	server := Server{ID: "target", Namespace: "games", Release: "target", Status: StatusOnline, MaxPlayers: 4}
	if err := app.store.Update(func(state *State) error { state.Servers[server.ID] = server; return nil }); err != nil {
		t.Fatal(err)
	}
	policy := DiskPressurePolicy{Enabled: true, ThresholdPercent: 85, Duration: time.Minute}
	start := time.Now().UTC()
	if err := app.store.Update(func(state *State) error {
		observeAlerts(state, server, diskPressureSample(start), start)
		return policy.observe(state, server, diskPressureSample(start), start)
	}); err != nil {
		t.Fatal(err)
	}
	snapshot := app.store.Snapshot()
	if p := snapshot.Producers[server.ID].DiskPressure; p == nil || p.Since.IsZero() {
		t.Fatalf("disk pressure streak not persisted: %+v", p)
	}
	snapshot.Producers[server.ID].DiskPressure.Since = time.Time{}
	if p := app.store.Snapshot().Producers[server.ID].DiskPressure; p == nil || p.Since.IsZero() {
		t.Fatal("Snapshot did not deep-clone DiskPressure state")
	}
	reloaded, err := NewStore(app.store.path, false)
	if err != nil {
		t.Fatal(err)
	}
	if p := reloaded.Snapshot().Producers[server.ID].DiskPressure; p != nil {
		t.Fatalf("disk pressure streak survived process restart: %+v", p)
	}
}
