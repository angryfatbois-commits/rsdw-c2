package main

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestMemoryPressurePolicyFromEnv(t *testing.T) {
	for _, name := range []string{"RSDW_MEMORY_PRESSURE_ENABLED", "RSDW_MEMORY_PRESSURE_THRESHOLD_PERCENT", "RSDW_MEMORY_PRESSURE_DURATION"} {
		t.Setenv(name, "")
	}
	got, err := memoryPressurePolicyFromEnv()
	if err != nil || got != (MemoryPressurePolicy{ThresholdPercent: 85, Duration: 10 * time.Minute}) {
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
			t.Setenv("RSDW_MEMORY_PRESSURE_"+tc.name, tc.value)
			_, err := memoryPressurePolicyFromEnv()
			if (err == nil) != tc.valid || err != nil && !strings.Contains(err.Error(), "RSDW_MEMORY_PRESSURE_"+tc.name) {
				t.Fatalf("parse error = %v, valid = %t", err, tc.valid)
			}
		})
	}
	t.Setenv("RSDW_MEMORY_PRESSURE_ENABLED", "true")
	t.Setenv("RSDW_MEMORY_PRESSURE_THRESHOLD_PERCENT", "90.5")
	t.Setenv("RSDW_MEMORY_PRESSURE_DURATION", "1m30s")
	got, err = memoryPressurePolicyFromEnv()
	if err != nil || got != (MemoryPressurePolicy{Enabled: true, ThresholdPercent: 90.5, Duration: 90 * time.Second}) {
		t.Fatalf("configured policy = %+v, %v", got, err)
	}
}

func pressureSample(at time.Time) observation {
	o := alertSample(at, "healthy", "runtime", -1)
	o.status = StatusOnline
	setReading(o.metrics, "memoryUsedBytes", 85, at)
	setReading(o.metrics, "memoryLimitBytes", 100, at)
	return o
}

func observePressure(t *testing.T, policy MemoryPressurePolicy, state *State, server Server, o observation, now time.Time) rebootDispatch {
	t.Helper()
	observeAlerts(state, server, o, now)
	dispatch, err := policy.observe(state, server, o, now)
	if err != nil {
		t.Fatal(err)
	}
	return dispatch
}

func TestMemoryPressureDurationAndSharedRestartLifecycle(t *testing.T) {
	for _, completed := range []bool{true, false} {
		t.Run(map[bool]string{true: "completed", false: "failed"}[completed], func(t *testing.T) {
			state, server, start := alertFixture(t)
			integration := state.Integrations["bot"]
			integration.Rules[MemoryPressureRestartRequested] = true
			state.Integrations["bot"] = integration
			policy := MemoryPressurePolicy{Enabled: true, ThresholdPercent: 85, Duration: time.Minute}
			for _, offset := range []time.Duration{0, 30 * time.Second, time.Minute - time.Nanosecond} {
				o := pressureSample(start.Add(offset))
				observePressure(t, policy, state, server, o, o.at)
				if len(state.Events) != 0 {
					t.Fatal("pressure restarted before the full duration", eventKinds(state))
				}
			}
			o := pressureSample(start.Add(time.Minute))
			dispatch := observePressure(t, policy, state, server, o, o.at)
			if dispatch.op.ID == "" || dispatch.op.Runtime != "runtime" || !slices.Equal(eventKinds(state), []EventKind{MemoryPressureRestartRequested}) || state.Events[0].Severity != "warning" || len(state.Deliveries) != 1 {
				t.Fatalf("pressure claim = %+v, events = %+v", dispatch, state.Events)
			}
			if state.Producers[server.ID].MemoryPressure != nil || state.Servers[server.ID].Status != StatusStarting {
				t.Fatal("claim did not reset pressure and mark the server starting")
			}
			o = pressureSample(start.Add(75 * time.Second))
			observePressure(t, policy, state, server, o, o.at)
			if len(state.Events) != 1 {
				t.Fatal("pending restart dispatched twice")
			}
			o = pressureSample(dispatch.op.RequestedAt.Add(restartTimeout))
			want := RestartFailed
			if completed {
				want = RestartCompleted
				o.runtime, o.restartOperation, o.runtimeStarted = "replacement", dispatch.op.ID, dispatch.op.RequestedAt.Add(time.Second)
			}
			observePressure(t, policy, state, server, o, o.at)
			if !slices.Equal(eventKinds(state), []EventKind{MemoryPressureRestartRequested, want}) || state.Events[1].OperationID != dispatch.op.ID || state.Producers[server.ID].Restart != nil || len(state.Deliveries) != 2 {
				t.Fatalf("shared reconciliation = %+v", state.Events)
			}
		})
	}
}

func TestMemoryPressureWarningCancelsWhenPressureClears(t *testing.T) {
	state, server, start := alertFixture(t)
	integration := state.Integrations["bot"]
	integration.Rules[RestartWarning] = true
	state.Integrations["bot"] = integration
	policy := MemoryPressurePolicy{Enabled: true, ThresholdPercent: 85, Duration: 10 * time.Minute}

	observePressure(t, policy, state, server, pressureSample(start), start)
	if len(state.Events) != 1 || state.Events[0].Kind != RestartWarning {
		t.Fatalf("pressure warning = %+v", state.Events)
	}
	warning := state.Events[0]
	if warning.Evidence.RestartWarning == nil || warning.Evidence.RestartWarning.Trigger != "memory-pressure" || warning.Evidence.RestartWarning.Minutes != 10 || warning.OccurrenceID == "" || len(state.Deliveries) != 1 {
		t.Fatalf("pressure warning evidence = %+v", warning)
	}

	cleared := pressureSample(start.Add(30 * time.Second))
	setReading(cleared.metrics, "memoryUsedBytes", 50, cleared.at)
	observePressure(t, policy, state, server, cleared, cleared.at)
	cancelObsoleteWarnings(state, cleared.at)
	for _, delivery := range state.Deliveries {
		if delivery.Status != DeliveryFailed || !strings.Contains(delivery.Result, "cancelled") {
			t.Fatalf("pressure warning remained active: %+v", delivery)
		}
	}
}

func TestMemoryPressureResetsContinuity(t *testing.T) {
	for _, tc := range []string{"low", "negative used", "zero limit", "negative limit", "NaN", "infinity", "missing used", "missing limit", "unavailable", "stale metric", "missing timestamp", "future metric", "stale observation", "future observation", "zero observation", "gap", "duplicate", "out of order", "runtime change", "missing runtime", "stopped", "observed stopped", "deleting", "deletion receipt", "pending", "disabled"} {
		t.Run(tc, func(t *testing.T) {
			state, server, start := alertFixture(t)
			policy := MemoryPressurePolicy{Enabled: true, ThresholdPercent: 85, Duration: time.Minute}
			for _, offset := range []time.Duration{0, 30 * time.Second} {
				o := pressureSample(start.Add(offset))
				observePressure(t, policy, state, server, o, o.at)
			}
			o := pressureSample(start.Add(time.Minute))
			now := o.at
			switch tc {
			case "low":
				setReading(o.metrics, "memoryUsedBytes", 84.99, o.at)
			case "negative used", "NaN", "infinity":
				value := map[string]float64{"negative used": -1, "NaN": math.NaN(), "infinity": math.Inf(1)}[tc]
				setReading(o.metrics, "memoryUsedBytes", value, o.at)
			case "zero limit", "negative limit":
				setReading(o.metrics, "memoryLimitBytes", 0, o.at)
				if tc == "negative limit" {
					setReading(o.metrics, "memoryLimitBytes", -1, o.at)
				}
			case "missing used":
				delete(o.metrics, "memoryUsedBytes")
			case "missing limit":
				delete(o.metrics, "memoryLimitBytes")
			case "unavailable", "stale metric", "missing timestamp", "future metric":
				reading := o.metrics["memoryUsedBytes"]
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
				o.metrics["memoryUsedBytes"] = reading
			case "stale observation":
				now = o.at.Add(telemetryMaxAge + time.Nanosecond)
			case "future observation":
				now = o.at.Add(-time.Nanosecond)
			case "zero observation":
				o.at = time.Time{}
			case "gap":
				o = pressureSample(start.Add(76 * time.Second))
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
			case "deletion receipt":
				state.Deletions = map[string]deletionRecord{server.ID: {ServerID: server.ID}}
			case "pending":
				p := state.Producers[server.ID]
				p.Restart = &RestartOperation{ID: "manual", Runtime: "runtime", RequestedAt: start}
				state.Producers[server.ID] = p
			case "disabled":
				policy.Enabled = false
			}
			observePressure(t, policy, state, server, o, now)
			if len(state.Events) != 0 {
				t.Fatal("interrupted pressure caused a restart", eventKinds(state))
			}
			p := state.Producers[server.ID]
			if tc != "gap" && tc != "runtime change" && p.MemoryPressure != nil {
				t.Fatal("invalid evidence retained the pressure streak")
			}
			p.Restart = nil
			state.Producers[server.ID] = p
			state.Deletions = nil
			server.Status = StatusOnline
			policy.Enabled = true
			next := now.Add(15 * time.Second)
			o = pressureSample(next)
			if tc == "runtime change" {
				o.runtime = "replacement"
			}
			observePressure(t, policy, state, server, o, next)
			if len(state.Events) != 0 {
				t.Fatal("pressure resumed the interrupted streak")
			}
		})
	}
}

func TestMemoryPressureUsesSourceTimeAndInclusiveThreshold(t *testing.T) {
	for _, threshold := range []float64{0, 85, 100} {
		state, server, start := alertFixture(t)
		policy := MemoryPressurePolicy{Enabled: true, ThresholdPercent: threshold, Duration: 30 * time.Second}
		for _, offset := range []time.Duration{0, 30 * time.Second} {
			o := pressureSample(start.Add(offset))
			setReading(o.metrics, "memoryUsedBytes", threshold, o.at)
			setReading(o.metrics, "memoryLimitBytes", 100, o.at)
			observePressure(t, policy, state, server, o, o.at)
		}
		if !slices.Equal(eventKinds(state), []EventKind{MemoryPressureRestartRequested}) {
			t.Fatalf("threshold %g with advancing metric timestamps = %v", threshold, eventKinds(state))
		}
	}
}

func TestMemoryPressureCachedSamplesCannotAdvanceOrBridgeSourceGaps(t *testing.T) {
	state, server, start := alertFixture(t)
	policy := MemoryPressurePolicy{Enabled: true, ThresholdPercent: 85, Duration: 30 * time.Second}
	for _, step := range []struct{ collected, sampled int }{
		{0, 0}, {15, 0}, {30, 0}, {45, 0}, {60, 60}, {75, 75}, {90, 90},
	} {
		o := pressureSample(start.Add(time.Duration(step.collected) * time.Second))
		setReading(o.metrics, "memoryUsedBytes", 85, start.Add(time.Duration(step.sampled)*time.Second))
		observePressure(t, policy, state, server, o, o.at)
		if step.collected < 90 && len(state.Events) != 0 {
			t.Fatalf("cached samples or a source gap triggered a restart at %d seconds", step.collected)
		}
	}
	if !slices.Equal(eventKinds(state), []EventKind{MemoryPressureRestartRequested}) {
		t.Fatalf("new continuous source samples did not trigger exactly one restart: %v", eventKinds(state))
	}
}

func TestMemoryPressureSourceClockResets(t *testing.T) {
	for _, offset := range []time.Duration{-time.Second, time.Second} {
		state, server, start := alertFixture(t)
		policy := MemoryPressurePolicy{Enabled: true, ThresholdPercent: 85, Duration: 30 * time.Second}
		observePressure(t, policy, state, server, pressureSample(start), start)
		o := pressureSample(start.Add(30 * time.Second))
		sampleAt := start.Add(offset)
		if offset > 0 {
			sampleAt = o.at.Add(offset)
		}
		setReading(o.metrics, "memoryUsedBytes", 85, sampleAt)
		observePressure(t, policy, state, server, o, o.at)
		o = pressureSample(start.Add(45 * time.Second))
		observePressure(t, policy, state, server, o, o.at)
		if len(state.Events) != 0 {
			t.Fatalf("source clock change retained elapsed pressure: %v", eventKinds(state))
		}
	}
}

func TestMemoryPressurePersistenceRecoveryAndClone(t *testing.T) {
	app := newTestApp(t, true)
	server := app.store.Snapshot().Servers["scuffedtards"]
	start := time.Now().UTC()
	policy := MemoryPressurePolicy{Enabled: true, ThresholdPercent: 85, Duration: 30 * time.Second}
	if err := app.store.Update(func(state *State) error {
		state.Events = nil
		observePressure(t, policy, state, server, pressureSample(start), start)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	snapshot := app.store.Snapshot()
	snapshot.Producers[server.ID].MemoryPressure.Since = start.Add(-time.Hour)
	if got := app.store.Snapshot().Producers[server.ID].MemoryPressure.Since; !got.Equal(start) {
		t.Fatal("snapshot mutated persisted pressure state")
	}
	if err := app.store.Update(func(state *State) error {
		state.Producers[server.ID].MemoryPressure.Since = start.Add(-time.Hour)
		return errors.New("abort")
	}); err == nil {
		t.Fatal("failed transaction was accepted")
	}
	if got := app.store.Snapshot().Producers[server.ID].MemoryPressure.Since; !got.Equal(start) {
		t.Fatal("failed transaction mutated pressure state")
	}
	data, err := os.ReadFile(app.store.path)
	if err != nil {
		t.Fatal(err)
	}
	var disk State
	if err := json.Unmarshal(data, &disk); err != nil || disk.Producers[server.ID].MemoryPressure == nil {
		t.Fatalf("pressure state missing from disk: %v", err)
	}
	reopened, err := NewStore(app.store.path, false)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Snapshot().Producers[server.ID].MemoryPressure != nil {
		t.Fatal("process recovery retained pressure state")
	}
	if err := reopened.Update(func(state *State) error {
		o := pressureSample(start.Add(30 * time.Second))
		observePressure(t, policy, state, server, o, o.at)
		if len(state.Events) != 0 {
			t.Fatal("process recovery caught up on pressure")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCollectorPressureClaimPersistsBeforeDispatch(t *testing.T) {
	for _, failPersistence := range []bool{false, true} {
		t.Run(map[bool]string{false: "dispatch", true: "persistence failure"}[failPersistence], func(t *testing.T) {
			app, runner, server := fixtureApp(t)
			app.memoryPressure = MemoryPressurePolicy{Enabled: true, ThresholdPercent: 3.125, Duration: time.Minute}
			at := time.Now().UTC().Add(-15 * time.Second)
			if err := app.store.Update(func(state *State) error {
				state.Producers[server.ID] = AlertProducer{Runtime: "pod-uid/container-id", LastAt: at, MemoryPressure: &MemoryPressureState{Runtime: "pod-uid/container-id", Since: at.Add(-time.Minute), LastAt: at, SampleAt: at}}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			calls := 0
			runner.override = func(call string) ([]byte, error, bool) {
				if !strings.Contains(call, "patch deployment") {
					return nil, nil, false
				}
				calls++
				if app.lifecycleMu.TryLock() {
					app.lifecycleMu.Unlock()
					t.Error("dispatch released lifecycleMu")
				}
				data, err := os.ReadFile(app.store.path)
				var saved State
				if err != nil || json.Unmarshal(data, &saved) != nil || saved.Producers[server.ID].Restart == nil || !slices.Equal(eventKinds(&saved), []EventKind{MemoryPressureRestartRequested}) {
					t.Errorf("dispatch preceded durable claim: %s, %v", data, err)
				}
				return nil, errors.New("command outcome unknown"), true
			}
			if failPersistence {
				app.store.path = t.TempDir()
			}
			app.collectTelemetry(context.Background())
			state := app.store.Snapshot()
			if failPersistence {
				if calls != 0 || state.Producers[server.ID].Restart != nil || len(state.Events) != 0 || !state.Producers[server.ID].MemoryPressure.LastAt.Equal(at) {
					t.Fatalf("failed persistence dispatched or changed state: %d, %+v", calls, state.Producers[server.ID])
				}
			} else {
				if calls != 1 || state.Producers[server.ID].Restart == nil || !state.Producers[server.ID].Restart.CommandUncertain {
					t.Fatalf("pressure command = %d, producer = %+v", calls, state.Producers[server.ID])
				}
				app.collectTelemetry(context.Background())
				if calls != 1 {
					t.Fatal("pending pressure command was redispatched")
				}
			}
		})
	}
}

func TestStopStartResetsMemoryPressureBeforeNextCollection(t *testing.T) {
	app := newTestApp(t, true)
	app.memoryPressure = MemoryPressurePolicy{Enabled: true, ThresholdPercent: 85, Duration: time.Nanosecond}
	if err := app.store.Update(func(state *State) error {
		server := state.Servers["scuffedtards"]
		server.MemoryUsedBytes, server.MemoryLimitBytes = 85, 100
		state.Servers = map[string]Server{server.ID: server}
		state.Events = nil
		at := time.Now().UTC().Add(-15 * time.Second)
		p := state.Producers[server.ID]
		p.MemoryPressure = &MemoryPressureState{Runtime: "demo/" + server.ID + "/" + server.LastRestart, Since: at, LastAt: at, SampleAt: at}
		state.Producers[server.ID] = p
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"stop", "start"} {
		res := requestJSON(t, app, http.MethodPost, "/api/servers/scuffedtards/actions/"+action, "")
		if res.Code != http.StatusOK {
			t.Fatalf("%s = %d: %s", action, res.Code, res.Body.String())
		}
	}
	app.collectTelemetry(context.Background())
	state := app.store.Snapshot()
	if !slices.Equal(eventKinds(&state), []EventKind{ServerStopped, ServerStarted}) {
		t.Fatalf("first collection reused pre-stop pressure: %v", eventKinds(&state))
	}
	app.collectTelemetry(context.Background())
	state = app.store.Snapshot()
	if !slices.Equal(eventKinds(&state), []EventKind{ServerStopped, ServerStarted, MemoryPressureRestartRequested, RestartCompleted}) {
		t.Fatalf("fresh pressure did not restart: %v", eventKinds(&state))
	}
}

func TestDemoPressureAndManualRestartMakeNoExternalCalls(t *testing.T) {
	app := newTestApp(t, true)
	app.memoryPressure = MemoryPressurePolicy{Enabled: true, ThresholdPercent: 85, Duration: time.Nanosecond}
	app.orchestrator = &kubeOrchestrator{runner: commandFunc(func(context.Context, string, ...string) ([]byte, error) {
		t.Error("demo called Kubernetes")
		return nil, errors.New("unexpected Kubernetes call")
	})}
	app.discordTransport = transportFunc(func(*http.Request) (*http.Response, error) {
		t.Error("demo contacted Discord")
		return nil, errors.New("unexpected Discord call")
	})
	if err := app.store.Update(func(state *State) error {
		server := state.Servers["scuffedtards"]
		server.MemoryUsedBytes, server.MemoryLimitBytes = 85, 100
		state.Servers = map[string]Server{server.ID: server}
		state.Events = nil
		integration := integrationFixture()
		integration.Rules[MemoryPressureRestartRequested] = true
		state.Integrations[integration.ID] = integration
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	app.collectTelemetry(context.Background())
	app.collectTelemetry(context.Background())
	state := app.store.Snapshot()
	if !slices.Equal(eventKinds(&state), []EventKind{MemoryPressureRestartRequested, RestartCompleted}) || state.Producers["scuffedtards"].Restart != nil || state.Producers["scuffedtards"].MemoryPressure != nil || state.Servers["scuffedtards"].Status != StatusOnline {
		t.Fatalf("demo pressure lifecycle = %+v", state.Events)
	}
	if state.Events[0].OperationID == "" || state.Events[0].OperationID != state.Events[1].OperationID {
		t.Fatal("demo completion did not share the pressure operation")
	}
	for _, delivery := range state.Deliveries {
		if result := app.sendDiscord(context.Background(), state.Integrations[delivery.IntegrationID], delivery); result.status != DeliverySent {
			t.Fatalf("demo send = %+v", result)
		}
	}
	res := requestJSON(t, app, http.MethodPost, "/api/servers/scuffedtards/actions/restart", "")
	if res.Code != http.StatusOK {
		t.Fatalf("demo manual restart = %d: %s", res.Code, res.Body.String())
	}
	state = app.store.Snapshot()
	if !slices.Equal(eventKinds(&state), []EventKind{MemoryPressureRestartRequested, RestartCompleted, RestartRequested, RestartCompleted}) {
		t.Fatalf("demo manual event kinds = %v", eventKinds(&state))
	}
}
