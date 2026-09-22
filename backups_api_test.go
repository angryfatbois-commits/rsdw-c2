package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func backupTestApp(t *testing.T) *App {
	t.Helper()
	app := newTestApp(t, true)
	backups, err := newBackupController(app.store, true)
	if err != nil {
		t.Fatal(err)
	}
	app.backups = backups
	app.store.mu.Lock()
	app.store.state.initBackups()
	app.store.state.StorageBackends[backupLocalBackendID] = app.backupBackend()
	app.store.mu.Unlock()
	return app
}

func backupJSONRequest(t *testing.T, app *App, method, endpoint string, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(method, endpoint, bytes.NewReader(data))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	return response
}

func TestBackupDemoCreatedServerStaysOnlineForRunningSourceSelection(t *testing.T) {
	app := backupTestApp(t)
	response := backupJSONRequest(t, app, http.MethodPost, "/api/servers", map[string]any{
		"name": "Backup browser world", "ownerId": "0123456789abcdef0123456789abcdef", "maxPlayers": 4,
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("create server = %d: %s", response.Code, response.Body.String())
	}
	created := decodeBackupResponse[Server](t, response)
	if created.Status != StatusOnline {
		t.Fatalf("created demo server status = %s, want online", created.Status)
	}

	bootstrap := backupJSONRequest(t, app, http.MethodGet, "/api/bootstrap", nil)
	if bootstrap.Code != http.StatusOK {
		t.Fatalf("bootstrap = %d: %s", bootstrap.Code, bootstrap.Body.String())
	}
	var view struct {
		Servers []Server `json:"servers"`
	}
	if err := json.Unmarshal(bootstrap.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	for _, server := range view.Servers {
		if server.ID == created.ID && server.Status != StatusOnline {
			t.Fatalf("bootstrapped demo server status = %s, want online", server.Status)
		}
	}
	run := backupJSONRequest(t, app, http.MethodPost, "/api/backups/runs", map[string]any{
		"serverId": created.ID, "definitionId": "dragonwilds-world-save", "backendId": "local", "idempotencyKey": "created-running", "acknowledge": true,
	})
	if run.Code != http.StatusCreated {
		t.Fatalf("running backup = %d: %s", run.Code, run.Body.String())
	}
	if got := decodeBackupResponse[BackupRun](t, run); got.Source != BackupSourceRunningBAK {
		t.Fatalf("created server backup source = %s, want running-bak", got.Source)
	}
}

func decodeBackupResponse[T any](t *testing.T, response *httptest.ResponseRecorder) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatalf("decode response %d %s: %v", response.Code, response.Body.String(), err)
	}
	return value
}

func backupDefinitionWithItems() BackupDefinition {
	return BackupDefinition{
		APIVersion: BackupDefinitionAPIVersion,
		Kind:       BackupDefinitionKind,
		ID:         "dragonwilds-world-and-config",
		Revision:   1,
		Name:       "World and config",
		ServerType: "dragonwilds",
		Strategy:   BackupStrategyLogicalFiles,
		Items: []BackupItemSpec{
			{Name: "world-save", Kind: BackupItemFile, Requirement: BackupItemRequired, Sources: []BackupSourceRule{
				{ServerState: BackupServerRunning, Collector: BackupCollectorServerFiles, Path: backupWorldSaveRoot},
				{ServerState: BackupServerStopped, Collector: BackupCollectorServerFiles, Path: backupWorldSaveRoot},
			}},
			{Name: "server-config", Kind: BackupItemFile, Requirement: BackupItemOptional, Sources: []BackupSourceRule{
				{ServerState: BackupServerRunning, Collector: BackupCollectorServerFiles, Path: "RSDragonwilds/Saved/Config/LinuxServer/DedicatedServer.ini"},
				{ServerState: BackupServerStopped, Collector: BackupCollectorServerFiles, Path: "RSDragonwilds/Saved/Config/LinuxServer/DedicatedServer.ini"},
			}},
		},
	}
}

func TestBackupAPIListsBuiltInDefinitionAndConnectedLocalBackend(t *testing.T) {
	app := backupTestApp(t)
	response := backupJSONRequest(t, app, http.MethodGet, "/api/backups", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("list backups status = %d: %s", response.Code, response.Body.String())
	}
	value := decodeBackupResponse[struct {
		Available bool
		Storage   []StorageBackend
		Profiles  []BackupDefinition
	}](t, response)
	if !value.Available || len(value.Storage) != 1 || value.Storage[0].Status != StorageBackendConnected || value.Storage[0].Removable {
		t.Fatalf("local backend state = %+v available=%t", value.Storage, value.Available)
	}
	if len(value.Profiles) != 1 || value.Profiles[0].ID != "dragonwilds-world-save" || value.Profiles[0].Items[0].Kind != BackupItemFile {
		t.Fatalf("built-in profile = %+v", value.Profiles)
	}
}

func TestBackupAPIProtectsBuiltInDefinitionFromCreateAndImport(t *testing.T) {
	app := backupTestApp(t)
	original := app.store.Snapshot().BackupDefinitions["dragonwilds-world-save"]
	replacement := original
	replacement.Name = "Overwritten built-in"

	created := backupJSONRequest(t, app, http.MethodPost, "/api/backups/profiles", replacement)
	if created.Code != http.StatusConflict {
		t.Fatalf("built-in create status = %d: %s", created.Code, created.Body.String())
	}
	if got := app.store.Snapshot().BackupDefinitions["dragonwilds-world-save"]; !reflect.DeepEqual(got, original) {
		t.Fatalf("built-in changed after create: got %+v want %+v", got, original)
	}

	data, err := ExportBackupDefinitionYAML(replacement)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/backups/profiles/import", bytes.NewReader(data))
	request.Header.Set("Content-Type", "application/yaml")
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("built-in import status = %d: %s", response.Code, response.Body.String())
	}
	if got := app.store.Snapshot().BackupDefinitions["dragonwilds-world-save"]; !reflect.DeepEqual(got, original) {
		t.Fatalf("built-in changed after import: got %+v want %+v", got, original)
	}
}

func TestBackupAPIRejectsMultiDocumentProfileImport(t *testing.T) {
	app := backupTestApp(t)
	definition := BuiltinDragonwildsDefinition()
	definition.ID = "multi-document-profile"
	definition.Name = "Multi-document profile"
	data, err := ExportBackupDefinitionYAML(definition)
	if err != nil {
		t.Fatal(err)
	}
	secondDocument := append([]byte(nil), data...)
	data = append(data, []byte("---\n")...)
	data = append(data, secondDocument...)

	request := httptest.NewRequest(http.MethodPost, "/api/backups/profiles/import", bytes.NewReader(data))
	request.Header.Set("Content-Type", "application/yaml")
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "exactly one document") {
		t.Fatalf("multi-document import = %d: %s", response.Code, response.Body.String())
	}
	if _, ok := app.store.Snapshot().BackupDefinitions[definition.ID]; ok {
		t.Fatal("multi-document profile was persisted")
	}
}

func TestBackupAPIUsesRunningBakAndStoppedSavWithoutFallback(t *testing.T) {
	app := backupTestApp(t)
	runningResponse := backupJSONRequest(t, app, http.MethodPost, "/api/backups/runs", map[string]any{
		"serverId": "scuffedtards", "definitionId": "dragonwilds-world-save", "backendId": "local", "idempotencyKey": "running", "acknowledge": true,
	})
	if runningResponse.Code != http.StatusCreated {
		t.Fatalf("running backup status = %d: %s", runningResponse.Code, runningResponse.Body.String())
	}
	running := decodeBackupResponse[BackupRun](t, runningResponse)
	if running.Source != BackupSourceRunningBAK || running.ItemCount != 1 || running.ManifestID == "" {
		t.Fatalf("running backup = %+v", running)
	}
	retry := backupJSONRequest(t, app, http.MethodPost, "/api/backups/runs", map[string]any{
		"serverId": "scuffedtards", "definitionId": "dragonwilds-world-save", "backendId": "local", "idempotencyKey": "running", "acknowledge": true,
	})
	if retry.Code != http.StatusCreated || decodeBackupResponse[BackupRun](t, retry).ID != running.ID {
		t.Fatalf("idempotent retry = %d %s", retry.Code, retry.Body.String())
	}

	if err := app.store.Update(func(state *State) error {
		server := state.Servers["scuffedtards"]
		server.Status = StatusStopped
		state.Servers[server.ID] = server
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	stoppedResponse := backupJSONRequest(t, app, http.MethodPost, "/api/backups/runs", map[string]any{
		"serverId": "scuffedtards", "definitionId": "dragonwilds-world-save", "backendId": "local", "idempotencyKey": "stopped", "acknowledge": true,
	})
	if stoppedResponse.Code != http.StatusCreated {
		t.Fatalf("stopped backup status = %d: %s", stoppedResponse.Code, stoppedResponse.Body.String())
	}
	stopped := decodeBackupResponse[BackupRun](t, stoppedResponse)
	if stopped.Source != BackupSourceStoppedSAV {
		t.Fatalf("stopped backup source = %q", stopped.Source)
	}

	invalid := backupDefinitionWithItems()
	invalid.Items[0].Sources = []BackupSourceRule{{ServerState: BackupServerStopped, Collector: BackupCollectorServerFiles, Path: backupWorldSaveRoot}}
	profile := backupJSONRequest(t, app, http.MethodPost, "/api/backups/profiles", invalid)
	if profile.Code != http.StatusOK {
		t.Fatalf("save invalid-for-running profile = %d: %s", profile.Code, profile.Body.String())
	}
	if err := app.store.Update(func(state *State) error {
		server := state.Servers["scuffedtards"]
		server.Status = StatusOnline
		state.Servers[server.ID] = server
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	noFallback := backupJSONRequest(t, app, http.MethodPost, "/api/backups/runs", map[string]any{
		"serverId": "scuffedtards", "definitionId": invalid.ID, "backendId": "local", "idempotencyKey": "no-fallback", "acknowledge": true,
	})
	if noFallback.Code != http.StatusBadRequest || !strings.Contains(noFallback.Body.String(), "no running source rule") {
		t.Fatalf("missing running source fallback response = %d %s", noFallback.Code, noFallback.Body.String())
	}
}

func TestBackupAPIMultiItemBundleAndDownload(t *testing.T) {
	app := backupTestApp(t)
	definition := backupDefinitionWithItems()
	if response := backupJSONRequest(t, app, http.MethodPost, "/api/backups/profiles", definition); response.Code != http.StatusOK {
		t.Fatalf("save multi-item profile = %d: %s", response.Code, response.Body.String())
	}
	response := backupJSONRequest(t, app, http.MethodPost, "/api/backups/runs", map[string]any{
		"serverId": "scuffedtards", "definitionId": definition.ID, "backendId": "local", "idempotencyKey": "multi", "acknowledge": true,
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("multi-item run = %d: %s", response.Code, response.Body.String())
	}
	run := decodeBackupResponse[BackupRun](t, response)
	if run.ItemCount != 2 {
		t.Fatalf("multi-item count = %d", run.ItemCount)
	}
	download := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/backups/runs/"+run.ID+"/bundle", nil)
	app.ServeHTTP(download, request)
	if download.Code != http.StatusOK || download.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("bundle response = %d %s", download.Code, download.Header())
	}
	archive, err := zip.NewReader(bytes.NewReader(download.Body.Bytes()), int64(download.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool, len(archive.File))
	for _, file := range archive.File {
		names[file.Name] = true
	}
	if !names["manifest.json"] || len(names) != 3 {
		t.Fatalf("bundle entries = %v", names)
	}
}

func TestBackupScheduleRunNowDoesNotAdvanceNextRunAndCreateServerUsesForm(t *testing.T) {
	app := backupTestApp(t)
	response := backupJSONRequest(t, app, http.MethodPost, "/api/backups/schedules", map[string]any{
		"serverId": "scuffedtards", "definitionId": "dragonwilds-world-save", "backendId": "local", "enabled": true,
		"mode": "daily", "dailyTimes": []string{"23:00"}, "executionTimezone": "UTC", "acknowledge": true,
	})
	if response.Code != http.StatusOK {
		t.Fatalf("create schedule = %d: %s", response.Code, response.Body.String())
	}
	schedule := decodeBackupResponse[backupScheduleView](t, response)
	nextBefore := schedule.NextRun
	runResponse := backupJSONRequest(t, app, http.MethodPost, "/api/backups/schedules/"+schedule.ID+"/run", map[string]any{"acknowledge": true})
	if runResponse.Code != http.StatusCreated {
		t.Fatalf("run schedule now = %d: %s", runResponse.Code, runResponse.Body.String())
	}
	after := app.store.Snapshot().BackupSchedules[schedule.ID]
	if nextBefore == nil || after.NextRun == nil || !nextBefore.Equal(*after.NextRun) {
		t.Fatalf("manual schedule run changed next run: before=%v after=%v", nextBefore, after.NextRun)
	}
	run := decodeBackupResponse[BackupRun](t, runResponse)
	createdResponse := backupJSONRequest(t, app, http.MethodPost, "/api/backups/"+run.ID+"/create-server", map[string]any{"serverName": "Copied world", "ownerName": "Copied owner", "ownerId": "0123456789abcdef0123456789abcdef", "confirmCreate": true})
	if createdResponse.Code != http.StatusCreated {
		t.Fatalf("create server from backup = %d: %s", createdResponse.Code, createdResponse.Body.String())
	}
	created := decodeBackupResponse[Server](t, createdResponse)
	if created.WorldName != "Copied world" || created.Name != "Copied owner" || created.Status != StatusStopped {
		t.Fatalf("created server = %+v", created)
	}
}

func TestBackupRestoreTrackedAndCustomSaveRequiresStoppedEmptyConfirmation(t *testing.T) {
	app := backupTestApp(t)
	runResponse := backupJSONRequest(t, app, http.MethodPost, "/api/backups/runs", map[string]any{
		"serverId": "scuffedtards", "definitionId": "dragonwilds-world-save", "backendId": "local", "idempotencyKey": "restore", "acknowledge": true,
	})
	run := decodeBackupResponse[BackupRun](t, runResponse)
	if err := app.store.Update(func(state *State) error {
		server := state.Servers["scuffedtards"]
		server.Status = StatusStopped
		state.Servers[server.ID] = server
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	restored := backupJSONRequest(t, app, http.MethodPost, "/api/servers/scuffedtards/restore", map[string]any{"source": "backup", "manifestId": run.ManifestID, "confirmEmpty": true})
	if restored.Code != http.StatusAccepted {
		t.Fatalf("tracked restore = %d: %s", restored.Code, restored.Body.String())
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	requestPart, err := writer.CreateFormField("request")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = requestPart.Write([]byte("{\"source\":\"custom\",\"confirmEmpty\":true,\"destructiveConfirm\":true}"))
	savePart, err := writer.CreateFormFile("save", "uploaded.sav")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = savePart.Write([]byte("custom-save"))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	customRequest := httptest.NewRequest(http.MethodPost, "/api/servers/scuffedtards/restore", &body)
	customRequest.Header.Set("Content-Type", writer.FormDataContentType())
	customResponse := httptest.NewRecorder()
	app.ServeHTTP(customResponse, customRequest)
	if customResponse.Code != http.StatusAccepted {
		t.Fatalf("custom restore = %d: %s", customResponse.Code, customResponse.Body.String())
	}
}

func TestBackupViewerCannotListOrRun(t *testing.T) {
	app := backupTestApp(t)
	request := httptest.NewRequest(http.MethodGet, "/api/backups", nil)
	request = request.WithContext(context.WithValue(request.Context(), principalKey{}, Principal{Subject: "viewer", Role: RoleViewer}))
	response := httptest.NewRecorder()
	app.handleBackups(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("viewer list = %d: %s", response.Code, response.Body.String())
	}
}

func TestBackupPersistenceDisabled(t *testing.T) {
	t.Setenv("RSDW_BACKUPS_ENABLED", "false")
	app := backupTestApp(t)
	if app.backupAvailable() {
		t.Fatal("backup controller is available when backups are disabled")
	}
	response := backupJSONRequest(t, app, http.MethodPost, "/api/backups/runs", map[string]any{
		"serverId": "scuffedtards", "definitionId": "dragonwilds-world-save", "acknowledge": true,
	})
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled backup run = %d: %s", response.Code, response.Body.String())
	}
}

func TestBackupScheduleRecoverySkipsOverdueWithoutCatchUp(t *testing.T) {
	now := time.Date(2026, time.September, 19, 12, 0, 0, 0, time.UTC)
	schedule := BackupSchedule{ID: "schedule", Enabled: true, Mode: rebootModeInterval, IntervalValue: 1, IntervalUnit: "hours", Timezone: "UTC", IntervalAnchor: cloneTimePtr(&now), NextRun: cloneTimePtr(ptrTime(now.Add(-3 * time.Hour)))}
	state := State{BackupSchedules: map[string]BackupSchedule{schedule.ID: schedule}}
	state.recoverBackupSchedules(now)
	got := state.BackupSchedules[schedule.ID]
	if got.NextRun == nil || !got.NextRun.After(now) || got.LastResult != BackupRunStatusFailed {
		t.Fatalf("recovered schedule = %+v", got)
	}
}

func ptrTime(value time.Time) *time.Time {
	return &value
}
