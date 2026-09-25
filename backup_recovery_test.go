package main

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestBackupRecoveryReconcilesPublishedRunAndInterruptedCaptures(t *testing.T) {
	for _, crash := range []string{"published", "staged", "failed-state-commit", "manifest-present"} {
		t.Run(crash, func(t *testing.T) {
			root := t.TempDir()
			repository := testRepository(t, LocalBackupRepositoryConfig{Root: filepath.Join(root, "backups")})
			request := testPublicationRequest(t, "recover-run", "recover-key", []testItem{{name: "world", path: "world.sav.backup", content: "durable-world", consistency: BackupConsistencyStableRead}})
			store, err := NewStore(filepath.Join(root, "state.json"), false)
			if err != nil {
				t.Fatal(err)
			}
			original := BackupRun{ID: request.Manifest.ID, ServerID: request.Manifest.ServerID, DefinitionID: request.Manifest.DefinitionID, BackendID: "local", ScheduleID: "nightly", Idempotency: request.Idempotency, Phase: BackupRunPublishing, Status: BackupRunStatusRunning, CreatedAt: request.Manifest.CreatedAt}
			if crash == "failed-state-commit" {
				original.Status, original.Error = BackupRunStatusFailed, "state commit failed"
			}
			if err := store.Update(func(state *State) error {
				state.BackupRuns = []BackupRun{original, {ID: "interrupted", Status: BackupRunStatusRunning, Phase: BackupRunCapturing, ScheduleID: "other"}}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			var publication PublishedBackup
			if crash == "staged" {
				publication, err = repository.createStage(context.Background(), request)
			} else {
				publication, err = repository.Publish(context.Background(), request)
			}
			if err != nil {
				t.Fatal(err)
			}
			if crash == "manifest-present" {
				if err := store.Update(func(state *State) error {
					state.BackupManifests[publication.Manifest.ID] = publication.Manifest
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			capture := filepath.Join(repository.root, ".capture", "capture-abandoned")
			if err := os.MkdirAll(capture, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(capture, "item"), []byte("partial capture"), 0600); err != nil {
				t.Fatal(err)
			}
			store, err = NewStore(filepath.Join(root, "state.json"), false)
			if err != nil {
				t.Fatal(err)
			}
			restarted := testRepository(t, LocalBackupRepositoryConfig{Root: repository.root})
			app := &App{store: store, backups: &backupController{available: true, root: repository.root, local: restarted, repositories: map[string]BackupRepository{backupLocalBackendID: restarted}}, clock: func() time.Time { return request.Manifest.CreatedAt.Add(time.Hour) }}
			if err := app.recoverBackupRepository(context.Background()); err != nil {
				t.Fatal(err)
			}
			state := store.Snapshot()
			if len(state.BackupRuns) != 2 {
				t.Fatalf("recovery duplicated runs: %+v", state.BackupRuns)
			}
			run := state.BackupRuns[0]
			if run.Status != BackupRunStatusSucceeded || run.ScheduleID != "nightly" || run.Idempotency != original.Idempotency || run.CreatedAt != original.CreatedAt || run.Bundle != publication.Bundle || run.ManifestID != original.ID {
				t.Fatalf("recovered run lost metadata or publication: %+v", run)
			}
			if state.BackupRuns[1].Status != BackupRunStatusInterrupted || state.BackupRuns[1].Error == "" || state.BackupRuns[1].ScheduleID != "other" {
				t.Fatalf("capture run not interrupted: %+v", state.BackupRuns[1])
			}
			if _, err := os.Stat(capture); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("capture remains: %v", err)
			}
			entries := readBundle(t, restarted, run.Bundle.Key)
			if string(entries[publication.Manifest.Items[0].ObjectKey]) != "durable-world" {
				t.Fatal("recovered bundle changed")
			}
			app.clock = func() time.Time { return request.Manifest.CreatedAt.Add(2 * time.Hour) }
			if err := app.recoverBackupRepository(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(state.BackupRuns, store.Snapshot().BackupRuns) {
				t.Fatal("repeated recovery changed runs")
			}
		})
	}
}

func TestBackupRecoveryDiscoversOrphanPublication(t *testing.T) {
	repository := testRepository(t, LocalBackupRepositoryConfig{Root: t.TempDir()})
	request := testPublicationRequest(t, "orphan", "orphan-key", []testItem{{name: "world", path: "world.sav.backup", content: "world", consistency: BackupConsistencyStableRead}})
	publication, err := repository.Publish(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore("", false)
	if err != nil {
		t.Fatal(err)
	}
	app := &App{store: store, backups: &backupController{available: true, local: repository, repositories: map[string]BackupRepository{backupLocalBackendID: repository}, root: repository.root}}
	for range 2 {
		if err := app.recoverBackupRepository(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	state := store.Snapshot()
	if len(state.BackupRuns) != 1 || state.BackupRuns[0].Idempotency != request.Idempotency || state.BackupRuns[0].Bundle != publication.Bundle || len(state.BackupManifests) != 1 {
		t.Fatalf("orphan recovery = %+v", state.BackupRuns)
	}
}

func TestBackupRecoveryInterruptsRunWithEmptyRepository(t *testing.T) {
	repository := testRepository(t, LocalBackupRepositoryConfig{Root: t.TempDir()})
	store, err := NewStore("", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(state *State) error {
		state.BackupRuns = []BackupRun{{ID: "queued", Phase: BackupRunQueued, Status: BackupRunStatusRunning}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	app := &App{store: store, backups: &backupController{available: true, local: repository, repositories: map[string]BackupRepository{backupLocalBackendID: repository}}}
	if err := app.recoverBackupRepository(context.Background()); err != nil {
		t.Fatal(err)
	}
	runs := store.Snapshot().BackupRuns
	if len(runs) != 1 || runs[0].Status != BackupRunStatusInterrupted || runs[0].Error == "" {
		t.Fatalf("empty repository left a running backup: %+v", runs)
	}
}

func TestBackupUnavailableScheduleRecordsFailureWithoutCatchUp(t *testing.T) {
	for _, unavailable := range []string{"disabled", "offline"} {
		t.Run(unavailable, func(t *testing.T) {
			app := backupTestApp(t)
			now := time.Date(2026, time.September, 19, 12, 0, 0, 0, time.UTC)
			app.clock = func() time.Time { return now }
			schedule := BackupSchedule{ID: "unavailable", ServerID: "scuffedtards", DefinitionID: "dragonwilds-world-save", BackendID: "local", Enabled: true, Mode: rebootModeInterval, IntervalValue: 1, IntervalUnit: "hours", Timezone: "UTC", IntervalAnchor: cloneTimePtr(&now), NextRun: ptrTime(now.Add(-3 * time.Hour))}
			if err := app.store.Update(func(state *State) error {
				state.BackupSchedules[schedule.ID] = schedule
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if unavailable == "disabled" {
				app.backups.available = false
			} else if err := os.Rename(app.backups.root, app.backups.root+"-offline"); err != nil {
				t.Fatal(err)
			}
			app.runDueBackups(context.Background())
			failed := app.store.Snapshot().BackupSchedules[schedule.ID]
			if failed.LastRun == nil || !failed.LastRun.Equal(now) || failed.LastResult != BackupRunStatusFailed || failed.LastError == "" || failed.NextRun == nil || !failed.NextRun.Equal(now.Add(time.Hour)) {
				t.Fatalf("unavailable due schedule did not record failure and advance: %+v", failed)
			}
			app.backups.available = true
			if unavailable == "offline" {
				if err := os.Rename(app.backups.root+"-offline", app.backups.root); err != nil {
					t.Fatal(err)
				}
			}
			app.runDueBackups(context.Background())
			snapshot := app.store.Snapshot()
			if !reflect.DeepEqual(failed, snapshot.BackupSchedules[schedule.ID]) || len(snapshot.BackupRuns) != 0 {
				t.Fatal("storage recovery caught up a failed occurrence")
			}
		})
	}
}

func TestScheduledBackupPruningRetainsOnlyConfiguredCount(t *testing.T) {
	app := backupTestApp(t)
	response := backupJSONRequest(t, app, http.MethodPost, "/api/servers", map[string]any{
		"name": "Pruning world", "ownerId": "0123456789abcdef0123456789abcdef", "maxPlayers": 4,
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("create server = %d: %s", response.Code, response.Body.String())
	}
	server := decodeBackupResponse[Server](t, response)
	schedule := BackupSchedule{ID: "prune-sched", ServerID: server.ID, DefinitionID: "dragonwilds-world-save", BackendID: "local", Enabled: true, RetainCount: 2, Mode: rebootModeInterval, IntervalValue: 1, IntervalUnit: "hours", Timezone: "UTC"}
	if err := app.store.Update(func(state *State) error {
		state.BackupSchedules[schedule.ID] = schedule
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var manifestIDs []string
	for i := 0; i < 4; i++ {
		run, err := app.executeBackup(context.Background(), backupRunRequest{
			ServerID: server.ID, DefinitionID: "dragonwilds-world-save", BackendID: "local", ScheduleID: schedule.ID,
			Actor: "scheduler", IdempotencyKey: fmt.Sprintf("prune-run-%d", i), Acknowledge: true, Scheduled: true,
		})
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		manifestIDs = append(manifestIDs, run.ManifestID)
	}
	snapshot := app.store.Snapshot()
	var kept []BackupRun
	for _, run := range snapshot.BackupRuns {
		if run.ScheduleID == schedule.ID {
			kept = append(kept, run)
		}
	}
	if len(kept) != 2 {
		t.Fatalf("expected 2 retained runs after pruning, got %d: %+v", len(kept), kept)
	}
	for _, run := range kept {
		if run.ManifestID != manifestIDs[2] && run.ManifestID != manifestIDs[3] {
			t.Fatalf("retained run %s is not one of the newest two", run.ManifestID)
		}
	}
	for _, id := range manifestIDs[:2] {
		if _, ok := snapshot.BackupManifests[id]; ok {
			t.Fatalf("pruned manifest %s still present in state", id)
		}
		if _, err := app.backups.local.OpenBundle(context.Background(), "objects/"+id+"/bundle.zip"); err == nil {
			t.Fatalf("pruned manifest %s bundle still readable from repository", id)
		}
	}
	for _, id := range manifestIDs[2:] {
		if _, err := app.backups.local.OpenBundle(context.Background(), "objects/"+id+"/bundle.zip"); err != nil {
			t.Fatalf("retained manifest %s bundle missing from repository: %v", id, err)
		}
	}
}

func TestManualBackupRunIsNotPrunedByScheduleRetention(t *testing.T) {
	app := backupTestApp(t)
	response := backupJSONRequest(t, app, http.MethodPost, "/api/servers", map[string]any{
		"name": "Manual run world", "ownerId": "0123456789abcdef0123456789abcdef", "maxPlayers": 4,
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("create server = %d: %s", response.Code, response.Body.String())
	}
	server := decodeBackupResponse[Server](t, response)
	manual := backupJSONRequest(t, app, http.MethodPost, "/api/backups/runs", map[string]any{
		"serverId": server.ID, "definitionId": "dragonwilds-world-save", "backendId": "local", "idempotencyKey": "manual-untouched", "acknowledge": true,
	})
	if manual.Code != http.StatusCreated {
		t.Fatalf("manual run = %d: %s", manual.Code, manual.Body.String())
	}
	schedule := BackupSchedule{ID: "unrelated-sched", ServerID: server.ID, DefinitionID: "dragonwilds-world-save", BackendID: "local", Enabled: true, RetainCount: 1, Mode: rebootModeInterval, IntervalValue: 1, IntervalUnit: "hours", Timezone: "UTC"}
	if err := app.store.Update(func(state *State) error {
		state.BackupSchedules[schedule.ID] = schedule
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := app.executeBackup(context.Background(), backupRunRequest{
			ServerID: server.ID, DefinitionID: "dragonwilds-world-save", BackendID: "local", ScheduleID: schedule.ID,
			Actor: "scheduler", IdempotencyKey: fmt.Sprintf("unrelated-run-%d", i), Acknowledge: true, Scheduled: true,
		}); err != nil {
			t.Fatalf("scheduled run %d: %v", i, err)
		}
	}
	snapshot := app.store.Snapshot()
	found := false
	for _, run := range snapshot.BackupRuns {
		if run.Idempotency.Key == "manual-untouched" {
			found = true
		}
	}
	if !found {
		t.Fatal("manual backup run was pruned by an unrelated schedule's retention")
	}
}

func demoCollectionDefinition(kind BackupItemKind, sourcePath string) BackupDefinition {
	definition := BuiltinDragonwildsDefinition()
	definition.Items[0].Kind = kind
	for index := range definition.Items[0].Sources {
		definition.Items[0].Sources[index].Path = sourcePath
	}
	return definition
}

func TestDemoBackupExplicitPathsAndDirectoryTar(t *testing.T) {
	app := &App{demo: true}
	for _, kind := range []BackupItemKind{BackupItemFile, BackupItemDirectory} {
		definition := demoCollectionDefinition(kind, "Saved/Config/settings.ini")
		items, err := app.collectDemoBackup(context.Background(), Server{ID: "demo"}, definition, BackupSourceStoppedSAV)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 1 || items[0].SourcePath != "Saved/Config/settings.ini" || items[0].Kind != kind {
			t.Fatalf("demo changed explicit path or kind: %+v", items)
		}
		if kind == BackupItemDirectory {
			archive := tar.NewReader(items[0].Content)
			header, err := archive.Next()
			if err != nil || header.Name != "Saved/Config/settings.ini/demo.sav" || header.Typeflag != tar.TypeReg {
				t.Fatalf("synthetic directory is not a restorable tar: %+v, %v", header, err)
			}
			content, err := io.ReadAll(archive)
			if err != nil || string(content) != "demo world world-save\nserver=demo\nsource=stopped-sav\n" {
				t.Fatalf("synthetic tar contents = %q, %v", content, err)
			}
			if _, err := archive.Next(); err != io.EOF {
				t.Fatalf("tar termination = %v", err)
			}
		}
	}
}

func TestDemoBackupReadsRestoredFilesAndDirectories(t *testing.T) {
	root := t.TempDir()
	world := filepath.Join(root, ".demo-worlds", "demo")
	files := map[string][]byte{
		"Saved/ServerConfig":                []byte("extensionless=true\n"),
		"Saved/SaveGames/restored-name.sav": {0, 1, 2, 255},
		"Saved/Config/settings.ini":         []byte("restored=true\n"),
		"Saved/Config/nested/extra.ini":     []byte("nested=true\n"),
	}
	for name, content := range files {
		path := filepath.Join(world, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	app := &App{demo: true, backups: &backupController{root: root}}
	for _, source := range []string{"Saved/ServerConfig", "Saved/SaveGames", "Saved/Config/settings.ini", "Saved/Config"} {
		kind := BackupItemFile
		if source == "Saved/Config" {
			kind = BackupItemDirectory
		}
		items, err := app.collectDemoBackup(context.Background(), Server{ID: "demo"}, demoCollectionDefinition(kind, source), BackupSourceStoppedSAV)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 1 {
			t.Fatalf("items = %+v", items)
		}
		if kind == BackupItemFile {
			content, err := io.ReadAll(items[0].Content)
			if err != nil || !bytes.Equal(content, files[items[0].SourcePath]) || len(content) == 0 {
				t.Fatalf("restored bytes changed at %s: %q, %v", items[0].SourcePath, content, err)
			}
		} else {
			archive := tar.NewReader(items[0].Content)
			seen := map[string][]byte{}
			for {
				header, err := archive.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if header.Typeflag == tar.TypeDir {
					continue
				}
				content, err := io.ReadAll(archive)
				if err != nil {
					t.Fatal(err)
				}
				seen[header.Name] = content
			}
			want := map[string][]byte{"Saved/Config/settings.ini": files["Saved/Config/settings.ini"], "Saved/Config/nested/extra.ini": files["Saved/Config/nested/extra.ini"]}
			if !reflect.DeepEqual(seen, want) {
				t.Fatalf("restored tar contents = %v, want %v", seen, want)
			}
		}
	}
	if _, err := app.collectDemoBackup(context.Background(), Server{ID: "demo"}, demoCollectionDefinition(BackupItemFile, "missing.ini"), BackupSourceStoppedSAV); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing restored file synthesized: %v", err)
	}
}

func TestBackupRecoveryRemovesRestoreTempsPreservingDemoWorlds(t *testing.T) {
	repository := testRepository(t, LocalBackupRepositoryConfig{Root: t.TempDir()})
	for _, name := range []string{".restore-abandoned", ".demo-worlds", "unrelated"} {
		if err := os.Mkdir(filepath.Join(repository.root, name), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repository.root, name, "data"), []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	store, err := NewStore("", false)
	if err != nil {
		t.Fatal(err)
	}
	app := &App{store: store, backups: &backupController{root: repository.root, local: repository, repositories: map[string]BackupRepository{backupLocalBackendID: repository}, available: true}}
	if err := app.recoverBackupRepository(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repository.root, ".restore-abandoned")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restore temp remains: %v", err)
	}
	for _, name := range []string{".demo-worlds", "unrelated"} {
		data, err := os.ReadFile(filepath.Join(repository.root, name, "data"))
		if err != nil || string(data) != "keep" {
			t.Fatalf("recovery changed %s: %q, %v", name, data, err)
		}
	}
}
