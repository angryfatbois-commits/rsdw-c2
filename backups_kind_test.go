package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type backupKINDRunner struct{ context string }

func (r backupKINDRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if name == "env" {
		args = append(args[:2], append([]string{"--kube-context", r.context}, args[2:]...)...)
		return (shellRunner{}).Run(ctx, name, args...)
	}
	flag := "--context"
	if name == "helm" {
		flag = "--kube-context"
	}
	return (shellRunner{}).Run(ctx, name, append([]string{flag, r.context}, args...)...)
}
func (r backupKINDRunner) RunStream(ctx context.Context, w io.Writer, name string, args ...string) error {
	return (shellRunner{}).RunStream(ctx, w, name, append([]string{"--context", r.context}, args...)...)
}

func TestBackupKINDCaptureRestoreAndSafety(t *testing.T) {
	contextName := os.Getenv("RSDW_BACKUP_KIND_CONTEXT")
	if contextName == "" {
		t.Skip("set RSDW_BACKUP_KIND_CONTEXT to run the isolated real Kubernetes test")
	}
	if !strings.HasPrefix(contextName, "kind-") {
		t.Fatal("integration test requires explicit kind context")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	runner := backupKINDRunner{contextName}
	ns := fmt.Sprintf("rsdw-backup-test-%d", time.Now().UnixNano())
	if out, err := runner.Run(ctx, "kubectl", "create", "namespace", ns); err != nil {
		t.Fatal(string(out), err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if out, err := runner.Run(cleanup, "kubectl", "delete", "namespace", ns, "--wait=false"); err != nil {
			t.Log(string(out), err)
		}
	})
	chart, _ := filepath.Abs("tests/fixtures/backup-chart")
	image := envOr("RSDW_BACKUP_KIND_IMAGE", "ghcr.io/angryfatbois-commits/rsdragonwilds-server:0.2.2")
	k := &kubeOrchestrator{runner: runner, kubectl: "kubectl", helm: "helm", chart: chart}
	server := Server{ID: "fixture", Release: "fixture", Namespace: ns, Name: "Fixture", OwnerID: "0123456789abcdef0123456789abcdef", CurrentImage: image, DesiredImage: image, Status: StatusOnline, MaxPlayers: 4, ServerSettings: ServerSettings{WorldName: "World", ServiceType: "ClusterIP"}}
	if err := k.Deploy(ctx, server); err != nil {
		t.Fatal(err)
	}
	if out, err := runner.Run(ctx, "kubectl", "-n", ns, "rollout", "status", "deployment/fixture-rsdragonwilds", "--timeout=120s"); err != nil {
		t.Fatal(string(out), err)
	}
	target, status, err := k.resolvePod(ctx, server)
	if err != nil || status != StatusOnline {
		t.Fatal(status, err)
	}
	_, err = k.backupPodExec(ctx, target, "sh", "-ec", `mkdir -p /home/steam/rsdw-dedicated/RSDragonwilds/Saved/SaveGames; printf 'authoritative backup' > /home/steam/rsdw-dedicated/RSDragonwilds/Saved/SaveGames/World.sav.backup; printf 'stopped save' > /home/steam/rsdw-dedicated/RSDragonwilds/Saved/SaveGames/World.sav; printf 'configuration' > /home/steam/rsdw-dedicated/DedicatedServer.ini`)
	if err != nil {
		t.Fatal(err)
	}
	app := newTestApp(t, false)
	app.orchestrator = k
	app.backups, err = newBackupController(app.store, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.updateBackupBackend(); err != nil {
		t.Fatal(err)
	}
	_ = app.store.Update(func(s *State) error { s.Servers[server.ID] = server; return nil })
	run := func(id string, definition BackupDefinition) BackupRun {
		_ = app.store.Update(func(s *State) error { s.BackupDefinitions[definition.ID] = definition; return nil })
		r, err := app.executeBackup(ctx, backupRunRequest{ServerID: server.ID, DefinitionID: definition.ID, BackendID: "local", Acknowledge: true, IdempotencyKey: id})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	profile := BuiltinDragonwildsDefinition()
	profile.Items = append(profile.Items, BackupItemSpec{Name: "config", Kind: BackupItemFile, Requirement: BackupItemRequired, Sources: []BackupSourceRule{{ServerState: BackupServerRunning, Collector: BackupCollectorServerFiles, Path: "DedicatedServer.ini"}, {ServerState: BackupServerStopped, Collector: BackupCollectorServerFiles, Path: "DedicatedServer.ini"}}})
	running := run("running", profile)
	files, err := app.backupRestoreFiles(ctx, running.ManifestID)
	if err != nil {
		t.Fatal(err)
	}
	contents := map[string]string{}
	defer cleanupRestoreFiles(files)
	for _, f := range files {
		data, readErr := restoreFileBytes(f)
		if readErr != nil {
			t.Fatal(readErr)
		}
		contents[f.Path] = string(data)
	}
	if contents[backupWorldSaveRoot+"/World.sav"] != "authoritative backup" || contents["DedicatedServer.ini"] != "configuration" {
		t.Fatal(contents)
	}
	unsafe := profile
	unsafe.Items = []BackupItemSpec{{Name: "directory", Kind: BackupItemDirectory, Requirement: BackupItemRequired, Sources: []BackupSourceRule{{ServerState: BackupServerRunning, Collector: BackupCollectorServerFiles, Path: backupWorldSaveRoot}}}}
	if _, cleanup, err := k.collectBackup(ctx, server, unsafe, BackupSourceRunningBAK, app.backups.root, app.backups.maxItem); err == nil {
		cleanup()
		t.Fatal("running directory .sav accepted")
	}
	if _, err := k.backupPodExec(ctx, target, "sh", "-ec", `ln -s /etc /home/steam/rsdw-dedicated/escape`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := k.resolveBackupPath(ctx, target, "escape/passwd", BackupItemFile, BackupServerRunning); err == nil {
		t.Fatal("ancestor symlink accepted")
	}
	if err := k.Scale(ctx, server, 0); err != nil {
		t.Fatal(err)
	}
	if err := k.verifyBackupStopped(ctx, server); err == nil {
		t.Fatal("terminating game pod treated as stopped")
	}
	if out, err := runner.Run(ctx, "kubectl", "-n", ns, "wait", "--for=delete", "pod", "-l", "app.kubernetes.io/instance=fixture", "--timeout=60s"); err != nil {
		t.Fatal(string(out), err)
	}
	server.Status = StatusStopped
	_ = app.store.Update(func(s *State) error { s.Servers[server.ID] = server; return nil })
	stopped := run("stopped", profile)
	stoppedFiles, err := app.backupRestoreFiles(ctx, stopped.ManifestID)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupRestoreFiles(stoppedFiles)
	for _, f := range stoppedFiles {
		data, readErr := restoreFileBytes(f)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if strings.HasSuffix(f.Path, ".sav") && string(data) != "stopped save" {
			t.Fatal("incorrect stopped content")
		}
	}
	if err := app.restoreFiles(ctx, server, files, false); err != errRestoreConfirmation {
		t.Fatalf("nonempty target = %v", err)
	}
	if err := app.restoreFiles(ctx, server, files, true); err != nil {
		t.Fatal(err)
	}
	custom := []restoreFile{{Path: backupWorldSaveRoot + "/Custom.SAV", Data: []byte("custom restored bytes")}}
	if err := app.restoreFiles(ctx, server, custom, true); err != nil {
		t.Fatal(err)
	}
	verifyTarget, cleanup, err := k.backupTarget(ctx, server, BackupServerStopped, false)
	if err != nil {
		t.Fatal(err)
	}
	out, err := k.backupPodExec(ctx, verifyTarget, "cat", backupDataRoot+"/"+backupWorldSaveRoot+"/World.sav")
	cleanup()
	if err != nil || string(out) != "custom restored bytes" {
		t.Fatal(string(out), err)
	}
	response := backupJSONRequest(t, app, "POST", "/api/backups/"+running.ID+"/create-server", map[string]any{"serverName": "Restored", "ownerName": "New owner", "ownerId": "0123456789abcdef0123456789abcdef", "confirmCreate": true})
	if response.Code != 201 {
		t.Fatal(response.Code, response.Body.String())
	}
	created := decodeBackupResponse[Server](t, response)
	if err := k.verifyBackupStopped(ctx, created); err != nil {
		t.Fatal("new server was not stopped", err)
	}
	newTarget, newCleanup, err := k.backupTarget(ctx, created, BackupServerStopped, false)
	if err != nil {
		t.Fatal(err)
	}
	out, err = k.backupPodExec(ctx, newTarget, "cat", backupDataRoot+"/"+backupWorldSaveRoot+"/Restored.sav")
	newCleanup()
	if err != nil || string(out) != "authoritative backup" {
		t.Fatal("new server content", string(out), err)
	}
	t.Log("verified running/stopped capture, config item, symlink and shutdown refusal, destructive confirmation, tracked/custom restore bytes, and new stopped server/PVC restore")
}
