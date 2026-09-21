package main

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// backupRunner answers the kubectl calls the collector makes and records every argv.
type backupRunner struct {
	mu       sync.Mutex
	calls    []string
	replicas int
	listing  []string
	listings int
	copyErr  error
	copyBody string
	listErr  error
}

func (r *backupRunner) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// listedSuffix reports whether a listing was requested with the given suffix argument.
func (r *backupRunner) listedSuffix(suffix string) bool {
	for _, call := range r.recorded() {
		if strings.Contains(call, "stat -c") && strings.HasSuffix(call, " "+suffix) {
			return true
		}
	}
	return false
}

func (r *backupRunner) issued(fragment string) bool {
	for _, call := range r.recorded() {
		if strings.Contains(call, fragment) {
			return true
		}
	}
	return false
}

// nextListing returns each configured listing in order and repeats the last one afterwards.
func (r *backupRunner) nextListing() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	line := r.listing[r.listings]
	if r.listings < len(r.listing)-1 {
		r.listings++
	}
	return line
}

func (r *backupRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	call := strings.Join(append([]string{name}, args...), " ")
	r.calls = append(r.calls, call)
	r.mu.Unlock()
	switch {
	case strings.Contains(call, "get deployment"):
		return []byte(fmt.Sprintf(`{"metadata":{"name":"world-rsdragonwilds","namespace":"games","uid":"deployment-uid"},"spec":{"replicas":%d,"template":{"spec":{"volumes":[{"name":"data","persistentVolumeClaim":{"claimName":"world-data"}}]}}}}`, r.replicas)), nil
	case strings.Contains(call, "get replicasets"):
		return []byte(`{"items":[{"metadata":{"namespace":"games","uid":"rs-uid","ownerReferences":[{"kind":"Deployment","uid":"deployment-uid","controller":true}]}}]}`), nil
	case strings.Contains(call, "get pods"):
		return []byte(`{"items":[` + fixturePod() + `]}`), nil
	case strings.Contains(call, "stat -c"):
		if r.listErr != nil {
			return nil, r.listErr
		}
		return []byte(r.nextListing()), nil
	case strings.Contains(call, " cp "):
		if r.copyErr != nil {
			return nil, r.copyErr
		}
		destination := args[len(args)-1]
		// A pull writes a local file; a push targets pod:path and touches nothing here.
		if !strings.Contains(destination, ":/") {
			if err := os.WriteFile(destination, []byte(r.copyBody), 0o600); err != nil {
				return nil, err
			}
		}
		return nil, nil
	case strings.Contains(call, "create -f"), strings.Contains(call, "wait --for=condition=Ready"),
		strings.Contains(call, "delete pod"), strings.Contains(call, "mkdir -p"), strings.Contains(call, "mv -T"),
		strings.Contains(call, "test ! -e"):
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected command %s", call)
}

func backupApp(t *testing.T, runner *backupRunner) (*App, Server) {
	t.Helper()
	dir := t.TempDir()
	store, err := NewStore(filepath.Join(dir, "state.json"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("RSDW_BACKUP_DIR", filepath.Join(dir, "backups"))
	server := Server{ID: "world", Name: "World", Release: "world", Namespace: "games", Status: StatusOnline, ServerSettings: ServerSettings{WorldName: "World"}}
	if err := store.Update(func(state *State) error {
		state.Servers[server.ID] = server
		return nil
	}); err != nil {
		t.Skipf("state persistence is unavailable on this platform: %v", err)
	}
	app := &App{store: store, orchestrator: &kubeOrchestrator{runner: runner, kubectl: "kubectl"}, auth: &Auth{demo: true}}
	return app, server
}

func stableListing(name string, size int) []string {
	return []string{fmt.Sprintf("%d 1700000000 %s\n", size, name)}
}

func runBackupToCompletion(t *testing.T, app *App, server Server) backupRun {
	t.Helper()
	run, resolved, err := app.startBackupRun(server.ID, dragonwildsWorldSave.ID)
	if err != nil {
		t.Fatalf("start backup: %v", err)
	}
	app.executeBackupRun(run, resolved)
	for _, recorded := range app.store.Snapshot().BackupRuns {
		if recorded.ID == run.ID {
			return recorded
		}
	}
	t.Fatal("backup run vanished from state")
	return backupRun{}
}

// waitForTerminalBackupRun polls for the async goroutine dispatched by an
// HTTP-triggered run to reach a terminal state, since that path never blocks
// the request on collection the way runBackupToCompletion does.
func waitForTerminalBackupRun(t *testing.T, app *App) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		runs := app.store.Snapshot().BackupRuns
		if len(runs) > 0 && !backupRunActive(runs[len(runs)-1].Result) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("backup run never reached a terminal state")
}

func TestBackupRunningServerCollectsOnlySavBackup(t *testing.T) {
	backupStabilityDelay = 0
	runner := &backupRunner{replicas: 1, listing: stableListing("World.sav.backup", 5), copyBody: "world"}
	app, server := backupApp(t, runner)
	run := runBackupToCompletion(t, app, server)
	if run.Result != backupCompleted {
		t.Fatalf("run = %+v", run)
	}
	if run.SourceMode != backupSourceRunningBak {
		t.Fatalf("source mode = %q", run.SourceMode)
	}
	if !runner.listedSuffix(".sav.backup") {
		t.Fatalf("collector never listed .sav.backup files: %v", runner.recorded())
	}
	if runner.listedSuffix(".sav") {
		t.Fatal("running server was asked for the plain .sav")
	}
	if runner.issued("create -f") {
		t.Fatal("running server spawned an inspector Pod")
	}
	if !runner.issued("cp -c server world-pod:/home/steam/rsdw-dedicated/RSDragonwilds/Saved/SaveGames/World.sav.backup") {
		t.Fatalf("collector did not copy the .sav.backup: %v", runner.recorded())
	}
}

func TestBackupStoppedServerUsesInspectorPodAndAlwaysDeletesIt(t *testing.T) {
	backupStabilityDelay = 0
	for _, tc := range []struct {
		name    string
		copyErr error
		want    backupResult
	}{
		{"copy succeeds", nil, backupCompleted},
		{"copy fails", errors.New("fixture copy denied"), backupFailedRes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &backupRunner{replicas: 0, listing: stableListing("TEST-01.sav", 5), copyBody: "world", copyErr: tc.copyErr}
			app, server := backupApp(t, runner)
			run := runBackupToCompletion(t, app, server)
			if run.Result != tc.want {
				t.Fatalf("result = %q, reason %q", run.Result, run.Reason)
			}
			if !runner.listedSuffix(".sav") || runner.listedSuffix(".sav.backup") {
				t.Fatalf("stopped server used the wrong suffix: %v", runner.recorded())
			}
			if !runner.issued("create -f") {
				t.Fatal("stopped server did not spawn an inspector Pod")
			}
			if !runner.issued("delete pod rsdw-backup-") {
				t.Fatalf("inspector Pod was not deleted: %v", runner.recorded())
			}
		})
	}
}

func TestBackupTransitionalServerIsSkippedWithoutCopy(t *testing.T) {
	backupStabilityDelay = 0
	runner := &backupRunner{replicas: 1, listing: stableListing("World.sav.backup", 5), copyBody: "world"}
	app, server := backupApp(t, runner)
	// A Pod that is not ready leaves resolvePod in StatusStarting.
	runner.mu.Lock()
	runner.mu.Unlock()
	app.orchestrator = &kubeOrchestrator{runner: &startingRunner{backupRunner: runner}, kubectl: "kubectl"}
	run := runBackupToCompletion(t, app, server)
	if run.Result != backupSkipped || run.Reason != errBackupTransitional.Error() {
		t.Fatalf("run = %+v", run)
	}
	if runner.issued(" cp ") {
		t.Fatal("transitional server issued a copy")
	}
}

// startingRunner reports a Pod whose server container is not ready.
type startingRunner struct{ *backupRunner }

func (r *startingRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	call := strings.Join(append([]string{name}, args...), " ")
	if strings.Contains(call, "get pods") {
		r.backupRunner.mu.Lock()
		r.backupRunner.calls = append(r.backupRunner.calls, call)
		r.backupRunner.mu.Unlock()
		return []byte(`{"items":[` + strings.Replace(fixturePod(), `"ready":true`, `"ready":false`, 1) + `]}`), nil
	}
	return r.backupRunner.Run(ctx, name, args...)
}

func TestBackupUnstableSourceFailsWithoutPublishing(t *testing.T) {
	backupStabilityDelay = 0
	for _, tc := range []struct{ name, second string }{
		{"size changed", "9 1700000000 World.sav.backup\n"},
		{"mtime changed", "5 1700000099 World.sav.backup\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &backupRunner{replicas: 1, listing: []string{"5 1700000000 World.sav.backup\n", tc.second}, copyBody: "world"}
			app, server := backupApp(t, runner)
			run := runBackupToCompletion(t, app, server)
			if run.Result != backupFailedRes || run.Reason != errBackupSourceMoved.Error() {
				t.Fatalf("run = %+v", run)
			}
			if runner.issued(" cp ") {
				t.Fatal("an unstable source was copied anyway")
			}
			repo, err := app.backupRepo()
			if err != nil {
				t.Fatal(err)
			}
			defer repo.close()
			if repo.exists(run.ID) {
				t.Fatal("an unstable source published a bundle")
			}
		})
	}
}

func TestBackupQuotaPreservesExistingBundles(t *testing.T) {
	backupStabilityDelay = 0
	runner := &backupRunner{replicas: 1, listing: stableListing("World.sav.backup", 5), copyBody: "world"}
	app, server := backupApp(t, runner)
	first := runBackupToCompletion(t, app, server)
	if first.Result != backupCompleted {
		t.Fatalf("first run = %+v", first)
	}
	t.Setenv("RSDW_BACKUP_MAX_BYTES", "1")
	second := runBackupToCompletion(t, app, server)
	if second.Result != backupFailedRes || second.Reason != errBackupQuota.Error() {
		t.Fatalf("second run = %+v", second)
	}
	t.Setenv("RSDW_BACKUP_MAX_BYTES", "")
	repo, err := app.backupRepo()
	if err != nil {
		t.Fatal(err)
	}
	defer repo.close()
	if !repo.exists(first.ID) {
		t.Fatal("quota failure deleted an existing bundle")
	}
}

func TestBackupInterruptedPublishIsNotOfferedForDownload(t *testing.T) {
	backupStabilityDelay = 0
	runner := &backupRunner{replicas: 1, listing: stableListing("World.sav.backup", 5), copyBody: "world"}
	app, server := backupApp(t, runner)
	run := runBackupToCompletion(t, app, server)
	repo, err := app.backupRepo()
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a process death between staging and rename.
	if err := repo.root.Rename(repo.bundleName(run.ID), run.ID+".tar.tmp"); err != nil {
		t.Fatal(err)
	}
	repo.close()
	res := requestJSON(t, app, "GET", "/api/backups", "")
	var listed struct {
		Runs []struct {
			ID           string `json:"id"`
			Downloadable bool   `json:"downloadable"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Runs) != 1 || listed.Runs[0].ID != run.ID || listed.Runs[0].Downloadable {
		t.Fatalf("interrupted publish was offered for download: %s", res.Body.String())
	}
	app.recoverBackupRepository()
	if res := requestJSON(t, app, "GET", "/api/backups/runs/"+run.ID+"/bundle", ""); res.Code != 404 {
		t.Fatalf("bundle download = %d", res.Code)
	}
}

func TestBackupStaleRunningRunIsFailedByRecovery(t *testing.T) {
	state := &State{Servers: map[string]Server{"world": {ID: "world", Name: "World"}}}
	state.initBackups()
	state.BackupRuns = []backupRun{{ID: "run", ServerID: "world", Result: backupRunning}}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	state.recoverBackups(now)
	if state.BackupRuns[0].Result != backupFailedRes || state.BackupRuns[0].Reason != "C2 restarted while this backup was running" {
		t.Fatalf("recovered run = %+v", state.BackupRuns[0])
	}
	if state.BackupRuns[0].FinishedAt == nil {
		t.Fatal("recovered run has no finish time")
	}
}

func TestBackupLateOccurrenceIsMissedAndCursorAdvances(t *testing.T) {
	runner := &backupRunner{replicas: 1, listing: stableListing("World.sav.backup", 5), copyBody: "world"}
	app, server := backupApp(t, runner)
	now := time.Now().UTC().Truncate(time.Second)
	app.clock = func() time.Time { return now }
	late := now.Add(-5 * time.Minute)
	schedule := backupSchedule{
		ID: "schedule", DefinitionID: dragonwildsWorldSave.ID, Enabled: true,
		Definition:     rebootDefinition{ServerID: server.ID, Mode: rebootModeInterval, IntervalValue: 1, IntervalUnit: "hours", ExecutionTimezone: "UTC"},
		IntervalAnchor: &late, NextRun: &late,
	}
	if err := app.store.Update(func(state *State) error {
		state.initBackups()
		state.BackupSchedules[schedule.ID] = schedule
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	claimed, _, err := app.claimScheduledBackups(server.ID, []string{schedule.ID})
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != "" {
		t.Fatal("a late occurrence dispatched a run")
	}
	if runner.issued(" cp ") {
		t.Fatal("a late occurrence copied a save")
	}
	snapshot := app.store.Snapshot()
	if len(snapshot.BackupRuns) != 1 || snapshot.BackupRuns[0].Result != backupMissed {
		t.Fatalf("runs = %+v", snapshot.BackupRuns)
	}
	next := snapshot.BackupSchedules[schedule.ID].NextRun
	if next == nil || !next.After(now) {
		t.Fatalf("cursor did not advance to a future time: %v", next)
	}
}

func TestBackupRunNowLeavesScheduleCursorUntouched(t *testing.T) {
	backupStabilityDelay = 0
	runner := &backupRunner{replicas: 1, listing: stableListing("World.sav.backup", 5), copyBody: "world"}
	app, _ := backupApp(t, runner)
	created := requestJSON(t, app, "POST", "/api/backups/schedules", `{"serverId":"world","enabled":true,"mode":"daily","dailyTimes":["05:00"],"executionTimezone":"UTC","warningMinutes":0,"acknowledgeDisconnect":false,"definitionId":"dragonwilds-world-save"}`)
	if created.Code != 201 {
		t.Fatalf("create schedule = %d: %s", created.Code, created.Body.String())
	}
	var schedule backupScheduleView
	if err := json.Unmarshal(created.Body.Bytes(), &schedule); err != nil {
		t.Fatal(err)
	}
	before := app.store.Snapshot().BackupSchedules[schedule.ID]
	beforeJSON, _ := json.Marshal(before)
	if res := requestJSON(t, app, "POST", "/api/backups/schedules/"+schedule.ID+"/run", ""); res.Code != 202 {
		t.Fatalf("run now = %d: %s", res.Code, res.Body.String())
	}
	waitForTerminalBackupRun(t, app)
	afterJSON, _ := json.Marshal(app.store.Snapshot().BackupSchedules[schedule.ID])
	if string(beforeJSON) != string(afterJSON) {
		t.Fatalf("run now mutated the schedule:\n%s\n%s", beforeJSON, afterJSON)
	}
	if len(app.store.Snapshot().BackupRuns) != 1 {
		t.Fatalf("run now recorded %d runs", len(app.store.Snapshot().BackupRuns))
	}
}

func TestBackupWarningMinutesRejected(t *testing.T) {
	runner := &backupRunner{replicas: 1, listing: stableListing("World.sav.backup", 5), copyBody: "world"}
	app, _ := backupApp(t, runner)
	res := requestJSON(t, app, "POST", "/api/backups/schedules", `{"serverId":"world","enabled":true,"mode":"daily","dailyTimes":["05:00"],"executionTimezone":"UTC","warningMinutes":5,"acknowledgeDisconnect":false,"definitionId":"dragonwilds-world-save"}`)
	if res.Code != 400 || !strings.Contains(res.Body.String(), "warningMinutes does not apply to backup schedules") {
		t.Fatalf("response = %d: %s", res.Code, res.Body.String())
	}
}

func TestBackupRoutesRejectViewers(t *testing.T) {
	app, issuer := oidcTestApp(t)
	cookies, csrf := loginAs(t, app, issuer, "viewer")
	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/backups"},
		{"POST", "/api/backups/runs"},
		{"GET", "/api/backups/runs/any/bundle"},
		{"DELETE", "/api/backups/runs/any"},
		{"POST", "/api/backups/schedules"},
		{"PUT", "/api/backups/schedules/any"},
		{"DELETE", "/api/backups/schedules/any"},
		{"POST", "/api/backups/schedules/any/run"},
		{"POST", "/api/servers/scuffedtards/actions/restore"},
	} {
		res := authRequest(app, tc.method, tc.path, cookies, "https://console.example", csrf, "")
		if res.Code != 403 || !strings.Contains(res.Body.String(), "permission denied") {
			t.Fatalf("%s %s = %d, want 403 permission denied: %s", tc.method, tc.path, res.Code, res.Body.String())
		}
	}
	if capabilities(RoleViewer)["backups"] {
		t.Fatal("viewers were granted the backups capability")
	}
}

func TestRestorePreconditions(t *testing.T) {
	backupStabilityDelay = 0
	runner := &backupRunner{replicas: 1, listing: stableListing("World.sav.backup", 5), copyBody: "world"}
	app, server := backupApp(t, runner)
	run := runBackupToCompletion(t, app, server)
	if run.Result != backupCompleted {
		t.Fatalf("seed run = %+v", run)
	}
	body := fmt.Sprintf(`{"runId":%q,"confirm":"world"}`, run.ID)
	if res := requestJSON(t, app, "POST", "/api/servers/world/actions/restore", body); res.Code != 409 || !strings.Contains(res.Body.String(), "stop the server before restoring") {
		t.Fatalf("running restore = %d: %s", res.Code, res.Body.String())
	}
	if err := app.store.Update(func(state *State) error {
		current := state.Servers["world"]
		current.Status = StatusStopped
		state.Servers["world"] = current
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// A stopped world that already holds a .sav needs the typed confirmation.
	runner.mu.Lock()
	runner.listing = stableListing("World.sav", 5)
	runner.mu.Unlock()
	res := requestJSON(t, app, "POST", "/api/servers/world/actions/restore", body)
	if res.Code != 409 || !strings.Contains(res.Body.String(), errWorldNotEmpty.Error()) {
		t.Fatalf("non-empty restore = %d: %s", res.Code, res.Body.String())
	}
	confirmed := fmt.Sprintf(`{"runId":%q,"confirm":"world","purgeConfirm":%q}`, run.ID, purgeWorldToken("world"))
	if res := requestJSON(t, app, "POST", "/api/servers/world/actions/restore", confirmed); res.Code != 200 {
		t.Fatalf("confirmed restore = %d: %s", res.Code, res.Body.String())
	}
	if !runner.issued("cp -c reader") {
		t.Fatalf("restore never copied into the world volume: %v", runner.recorded())
	}
	if !runner.issued("delete pod rsdw-restore-") {
		t.Fatal("restore inspector Pod was not deleted")
	}
}

func TestBackupBundleContainsManifestAndItem(t *testing.T) {
	backupStabilityDelay = 0
	runner := &backupRunner{replicas: 1, listing: stableListing("World.sav.backup", 11), copyBody: "world-bytes"}
	app, server := backupApp(t, runner)
	run := runBackupToCompletion(t, app, server)
	if run.Result != backupCompleted {
		t.Fatalf("run = %+v", run)
	}
	res := requestJSON(t, app, "GET", "/api/backups/runs/"+run.ID+"/bundle", "")
	if res.Code != 200 || res.Header().Get("Content-Type") != "application/x-tar" {
		t.Fatalf("bundle = %d %q", res.Code, res.Header().Get("Content-Type"))
	}
	reader := tar.NewReader(strings.NewReader(res.Body.String()))
	names := []string{}
	var payload string
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, header.Name)
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		if header.Name != backupManifestEntry {
			payload = string(data)
		}
	}
	if len(names) != 2 || names[0] != backupManifestEntry || names[1] != "items/world-running.sav.backup" {
		t.Fatalf("bundle entries = %v", names)
	}
	if payload != "world-bytes" {
		t.Fatalf("bundle payload = %q", payload)
	}
	if run.Manifest == nil || len(run.Manifest.Items) != 1 || !strings.HasPrefix(run.Manifest.Items[0].Checksum, "sha256:") {
		t.Fatalf("manifest = %+v", run.Manifest)
	}
}

func TestBackupSecondRunPerServerIsRejected(t *testing.T) {
	runner := &backupRunner{replicas: 1, listing: stableListing("World.sav.backup", 5), copyBody: "world"}
	app, server := backupApp(t, runner)
	if _, _, err := app.startBackupRun(server.ID, dragonwildsWorldSave.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := app.startBackupRun(server.ID, dragonwildsWorldSave.ID); !errors.Is(err, errBackupConflict) {
		t.Fatalf("second run error = %v", err)
	}
}

func TestBackupAmbiguousAndMissingSourcesFail(t *testing.T) {
	backupStabilityDelay = 0
	for _, tc := range []struct {
		name    string
		listing []string
		want    string
	}{
		{"no candidate", []string{"\n"}, "Running server has no game-generated .sav.backup to collect"},
		{"two candidates", []string{"5 1700000000 A.sav.backup\n5 1700000000 B.sav.backup\n"}, "C2 never guesses which world to back up"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &backupRunner{replicas: 1, listing: tc.listing, copyBody: "world"}
			app, server := backupApp(t, runner)
			run := runBackupToCompletion(t, app, server)
			if run.Result != backupFailedRes || !strings.Contains(run.Reason, tc.want) {
				t.Fatalf("run = %+v", run)
			}
			if runner.issued(" cp ") {
				t.Fatal("an unresolved source was copied")
			}
		})
	}
}
