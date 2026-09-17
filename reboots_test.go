package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type rebootTestOrchestrator struct {
	calls []Server
	err   error
}

func (o *rebootTestOrchestrator) Deploy(context.Context, Server) error { return nil }
func (o *rebootTestOrchestrator) Restart(_ context.Context, server Server) error {
	o.calls = append(o.calls, server)
	return o.err
}
func (o *rebootTestOrchestrator) Logs(context.Context, Server, int) ([]LogLine, error) {
	return nil, nil
}
func (o *rebootTestOrchestrator) Refresh(_ context.Context, server Server) (Server, error) {
	return server, nil
}
func (o *rebootTestOrchestrator) CheckUpdate(_ context.Context, server Server) (Server, error) {
	return server, nil
}

func TestRebootScheduleAPIValidatesAndMutatesWithoutImmediateRestart(t *testing.T) {
	app := newTestApp(t, true)
	now := rebootAt("2026-09-17T12:00:00Z")
	app.clock = func() time.Time { return now }
	body := `{"serverId":"scuffedtards","enabled":true,"mode":"daily","dailyTimes":["17:00","05:00","05:00"],"executionTimezone":"UTC","acknowledgeDisconnect":true}`
	created := requestJSON(t, app, http.MethodPost, "/api/reboots", body)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", created.Code, created.Body.String())
	}
	var schedule rebootScheduleView
	if err := json.Unmarshal(created.Body.Bytes(), &schedule); err != nil {
		t.Fatal(err)
	}
	if schedule.ID == "" || !schedule.Enabled || schedule.NextRun == nil || schedule.NextRun.Before(now) {
		t.Fatalf("created schedule = %+v", schedule)
	}
	if got := app.store.Snapshot().RebootHistory; len(got) != 0 {
		t.Fatalf("saving a schedule executed work: %+v", got)
	}
	if got := app.store.Snapshot().Events[len(app.store.Snapshot().Events)-1]; got.Actor != "token-admin" || got.ScheduleID != schedule.ID {
		t.Fatalf("schedule audit event = %+v", got)
	}

	preview := requestJSON(t, app, http.MethodPost, "/api/reboots/preview", `{"mode":"cron","cron":"0 5 * * *","executionTimezone":"America/New_York"}`)
	if preview.Code != http.StatusOK || !strings.Contains(preview.Body.String(), `"runs"`) {
		t.Fatalf("preview = %d: %s", preview.Code, preview.Body.String())
	}
	invalid := requestJSON(t, app, http.MethodPost, "/api/reboots/preview", `{"enabled":true,"mode":"daily","dailyTimes":["05:00"],"executionTimezone":"UTC"} trailing`)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("trailing JSON status = %d", invalid.Code)
	}
	impossible := requestJSON(t, app, http.MethodPost, "/api/reboots", `{"serverId":"scuffedtards","enabled":false,"mode":"cron","cron":"0 0 31 2 *","executionTimezone":"UTC"}`)
	if impossible.Code != http.StatusBadRequest {
		t.Fatalf("impossible disabled schedule status = %d: %s", impossible.Code, impossible.Body.String())
	}

	disabled := `{"serverId":"scuffedtards","enabled":false,"mode":"daily","dailyTimes":["17:00"],"executionTimezone":"UTC"}`
	updated := requestJSON(t, app, http.MethodPut, "/api/reboots/"+schedule.ID, disabled)
	if updated.Code != http.StatusOK {
		t.Fatalf("disable status = %d: %s", updated.Code, updated.Body.String())
	}
	var disabledView rebootScheduleView
	if err := json.Unmarshal(updated.Body.Bytes(), &disabledView); err != nil {
		t.Fatal(err)
	}
	if disabledView.Enabled || disabledView.NextRun != nil {
		t.Fatalf("disabled schedule = %+v", disabledView)
	}
	deleted := requestJSON(t, app, http.MethodDelete, "/api/reboots/"+schedule.ID, "")
	if deleted.Code != http.StatusNoContent || app.store.Snapshot().RebootSchedules[schedule.ID].ID != "" {
		t.Fatalf("delete status/state = %d/%+v", deleted.Code, app.store.Snapshot().RebootSchedules[schedule.ID])
	}
}

func TestScheduledRebootsCoalesceAndReconcileThroughExistingRestart(t *testing.T) {
	app := newTestApp(t, false)
	runner := &rebootTestOrchestrator{}
	app.orchestrator = runner
	now := rebootAt("2026-09-17T12:00:00Z")
	app.clock = func() time.Time { return now }
	if err := app.store.Update(func(state *State) error {
		server := Server{ID: "world", Name: "World", Status: StatusOnline}
		state.Servers[server.ID] = server
		anchor := now.Add(-2 * time.Hour)
		due := now.Add(-30 * time.Second)
		for _, id := range []string{"morning", "evening"} {
			state.RebootSchedules[id] = rebootSchedule{ID: id, Definition: rebootDefinition{ServerID: server.ID, Mode: rebootModeInterval, IntervalValue: 1, IntervalUnit: "hours", ExecutionTimezone: "UTC"}, Enabled: true, Revision: 1, CreatedAt: anchor, UpdatedAt: anchor, IntervalAnchor: cloneTimePtr(&anchor), NextRun: cloneTimePtr(&due)}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	app.scanReboots(context.Background(), now)
	if len(runner.calls) != 1 || runner.calls[0].ID != "world" {
		t.Fatalf("restart calls = %+v", runner.calls)
	}
	state := app.store.Snapshot()
	op := state.Producers["world"].Restart
	if op == nil || len(state.RebootHistory) != 2 {
		t.Fatalf("claimed state = %+v", state)
	}
	for _, execution := range state.RebootHistory {
		if execution.OperationID != op.ID || execution.Result != rebootAwaiting || execution.ID == "" || execution.OccurrenceID != execution.ID {
			t.Fatalf("coalesced execution = %+v, operation = %+v", execution, op)
		}
	}
	listed := requestJSON(t, app, http.MethodGet, "/api/reboots", "")
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), `"occurrenceId"`) {
		t.Fatalf("history API did not expose occurrence identity: %d %s", listed.Code, listed.Body.String())
	}

	if err := app.store.Update(func(state *State) error {
		server := state.Servers["world"]
		observationAt := now.Add(15 * time.Second)
		observeAlerts(state, server, observation{at: observationAt, health: "healthy", runtime: "replacement", runtimeStarted: observationAt, restartOperation: op.ID, metrics: emptyMetrics()}, observationAt)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	state = app.store.Snapshot()
	if state.Producers["world"].Restart != nil {
		t.Fatal("replacement observation did not clear the pending operation")
	}
	for _, execution := range state.RebootHistory {
		if execution.Result != rebootCompleted {
			t.Fatalf("execution was not reconciled: %+v", execution)
		}
	}
}

func TestScheduledRebootMissingTargetAndPersistenceFailureDoNotDispatch(t *testing.T) {
	app := newTestApp(t, false)
	runner := &rebootTestOrchestrator{}
	app.orchestrator = runner
	now := rebootAt("2026-09-17T12:00:00Z")
	app.clock = func() time.Time { return now }
	due := now.Add(-15 * time.Second)
	if err := app.store.Update(func(state *State) error {
		state.RebootSchedules["missing"] = rebootSchedule{ID: "missing", Definition: rebootDefinition{ServerID: "gone", Mode: rebootModeDaily, DailyTimes: []string{"05:00"}, ExecutionTimezone: "UTC"}, Enabled: true, Revision: 1, NextRun: cloneTimePtr(&due)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	app.scanReboots(context.Background(), now)
	if len(runner.calls) != 0 {
		t.Fatalf("missing target dispatched = %+v", runner.calls)
	}
	state := app.store.Snapshot()
	if len(state.RebootHistory) != 1 || state.RebootHistory[0].Result != rebootSkipped {
		t.Fatalf("missing target result = %+v", state.RebootHistory)
	}

	badParent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(badParent, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	app = newTestApp(t, false)
	app.orchestrator = runner
	app.clock = func() time.Time { return now }
	app.store.path = filepath.Join(badParent, "state.json")
	app.store.state.Servers["world"] = Server{ID: "world", Name: "World"}
	app.store.state.RebootSchedules["persist"] = rebootSchedule{ID: "persist", Definition: rebootDefinition{ServerID: "world", Mode: rebootModeDaily, DailyTimes: []string{"05:00"}, ExecutionTimezone: "UTC"}, Enabled: true, Revision: 1, NextRun: cloneTimePtr(&due)}
	app.scanReboots(context.Background(), now)
	if len(runner.calls) != 0 {
		t.Fatalf("persistence failure dispatched = %+v", runner.calls)
	}
}
