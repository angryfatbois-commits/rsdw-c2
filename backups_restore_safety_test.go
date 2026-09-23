package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRestoreBackupSuffix(t *testing.T) {
	for _, test := range []struct{ source, want string }{
		{"SaveGames/World.sav.backup", "SaveGames/World.sav"},
		{"SaveGames/World.SAV.BACKUP", "SaveGames/World.SAV"},
		{"SaveGames/Legacy.bak", "SaveGames/Legacy.sav"},
		{"DedicatedServer.ini", "DedicatedServer.ini"},
	} {
		if got := restoreDestination(test.source, BackupSourceRunningBAK); got != test.want {
			t.Fatalf("%s restored to %s, want %s", test.source, got, test.want)
		}
	}
}

func TestRestoreTransactionDoesNotUseGNUFindPrintf(t *testing.T) {
	if strings.Contains(restoreTransactionScript, "-printf") {
		t.Fatal("restore transaction must not depend on GNU find -printf")
	}
}

func TestStoppedRendererParsesAndFailsClosed(t *testing.T) {
	for _, input := range []string{
		"kind: Deployment\nspec:\n  replicas: 1 # chart default\n",
		"kind: StatefulSet\nspec:\n    replicas: 12\n",
		"kind: Deployment\nspec: {template: {}}\n",
	} {
		var out bytes.Buffer
		if err := renderStoppedWorkloads(strings.NewReader(input), &out); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), `"replicas":0`) {
			t.Fatal(out.String())
		}
	}
	for _, input := range []string{
		"kind: Pod\nspec: {}\n", "kind: Job\nspec: {}\n", "kind: ConfigMap\n",
		"kind: Deployment\nspec: {}\n---\nkind: HorizontalPodAutoscaler\nspec: {}\n",
		"kind: Deployment\nspec: {}\nmetadata:\n  annotations:\n    helm.sh/hook: pre-install\n",
		"kind: Deployment\nspec: [invalid\n",
	} {
		var out bytes.Buffer
		if err := renderStoppedWorkloads(strings.NewReader(input), &out); err == nil || out.Len() != 0 {
			t.Fatalf("must fail before emitting resources: %q %v", out.String(), err)
		}
	}
}

func TestConfigOnlyRestorePreservesWorldSave(t *testing.T) {
	app := backupTestApp(t)
	server := app.store.Snapshot().Servers["scuffedtards"]
	server.Status = StatusStopped
	files := []restoreFile{{Path: backupWorldSaveRoot + "/World.sav", Data: []byte("precious world")}}
	if err := app.restoreFiles(context.Background(), server, files, false); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(app.backups.root, ".demo-worlds", server.ID)
	before, err := os.ReadDir(filepath.Join(root, backupWorldSaveRoot))
	if err != nil || len(before) != 1 {
		t.Fatal(before, err)
	}
	if err := app.restoreFiles(context.Background(), server, []restoreFile{{Path: "DedicatedServer.ini", Data: []byte("new configuration")}}, true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, backupWorldSaveRoot, before[0].Name()))
	if err != nil || string(data) != "precious world" {
		t.Fatal(string(data), err)
	}
}

func TestCreateServerFromBackupRejectsUnsafeWorldName(t *testing.T) {
	app := backupTestApp(t)
	response := backupJSONRequest(t, app, "POST", "/api/backups/runs", backupRunRequest{ServerID: "scuffedtards", DefinitionID: "dragonwilds-world-save", BackendID: "local", Acknowledge: true})
	if response.Code != 201 {
		t.Fatal(response.Code, response.Body.String())
	}
	run := decodeBackupResponse[BackupRun](t, response)
	before := len(app.store.Snapshot().Servers)
	response = backupJSONRequest(t, app, "POST", "/api/backups/"+run.ID+"/create-server", map[string]any{"serverName": "../Other", "ownerName": "Owner", "ownerId": "0123456789abcdef0123456789abcdef", "confirmCreate": true})
	if response.Code != 400 || len(app.store.Snapshot().Servers) != before {
		t.Fatal(response.Code, response.Body.String())
	}
}

func TestRestoreRecoversAppliedButUnfinalizedTransaction(t *testing.T) {
	root := t.TempDir()
	relative := backupWorldSaveRoot + "/World.sav"
	write := func(name, data string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	run := func(mode string) {
		t.Helper()
		out, err := exec.Command("sh", "-ec", restoreTransactionScript, "restore", root, mode, "true", "true").CombinedOutput()
		if err != nil {
			t.Fatal(string(out), err)
		}
	}
	write(filepath.Join(root, relative), "original")
	run("prepare")
	stage := filepath.Join(root, ".c2-restore")
	write(filepath.Join(stage, "incoming", relative), "replacement")
	write(filepath.Join(stage, "paths"), relative+"\n")
	write(filepath.Join(stage, "checksums"), fmt.Sprintf("%x  %s\n", sha256.Sum256([]byte("replacement")), relative))
	run("commit")
	data, _ := os.ReadFile(filepath.Join(stage, "original", relative))
	if string(data) != "original" {
		t.Fatal("rollback bytes discarded", string(data))
	}
	// A new stopped attempt recovers an interrupted/unverified commit before staging.
	run("prepare")
	data, _ = os.ReadFile(filepath.Join(root, relative))
	if string(data) != "original" {
		t.Fatal("original not recovered", string(data))
	}
}

func TestRestoreDoesNotFinalizeIfTargetStartsDuringCommit(t *testing.T) {
	app := backupTestApp(t)
	app.demo = false
	server := Server{ID: "world", Namespace: "dragonwilds", Release: "world-release", DesiredImage: "ghcr.io/example/server:latest", Status: StatusStopped}
	committed, finalized := false, false
	var inspector string
	runner := &backupKubeTestRunner{run: func(args []string) ([]byte, error) {
		call := strings.Join(args, " ")
		switch {
		case strings.Contains(call, "get deployment "):
			replicas := 0
			if committed {
				replicas = 1
			}
			return []byte(fmt.Sprintf(`{"metadata":{"name":%q,"namespace":"dragonwilds","uid":"deployment-uid"},"spec":{"replicas":%d}}`, deploymentName(server.Release), replicas)), nil
		case strings.Contains(call, "get replicasets "):
			return []byte(`{"items":[]}`), nil
		case strings.Contains(call, "get persistentvolumeclaims "):
			return []byte(`{"items":[{"metadata":{"name":"world-pvc","namespace":"dragonwilds"}}]}`), nil
		case strings.Contains(call, "get pods "):
			return []byte(`{"items":[]}`), nil
		case strings.Contains(call, " run "):
			inspector = args[4]
		case strings.Contains(call, " get pod "):
			return []byte(fmt.Sprintf(`{"metadata":{"name":%q,"namespace":"dragonwilds","uid":"helper-uid"},"spec":{"containers":[{"name":"backup"}]},"status":{"phase":"Running"}}`, inspector)), nil
		case strings.Contains(call, " exec "):
			switch args[len(args)-3] {
			case "commit":
				committed = true
			case "finalize":
				finalized = true
			}
		case strings.Contains(call, " wait "), strings.Contains(call, " cp "), strings.Contains(call, " delete pod "):
		default:
			t.Fatalf("unexpected command %s", call)
		}
		return nil, nil
	}}
	app.orchestrator = &kubeOrchestrator{runner: runner, kubectl: "unused"}
	err := app.restoreFiles(context.Background(), server, []restoreFile{{Path: backupWorldSaveRoot + "/World.sav", Data: []byte("save")}}, true)
	if err == nil || !strings.Contains(err.Error(), "original files retained") || !committed || finalized {
		t.Fatalf("commit=%v finalize=%v error=%v", committed, finalized, err)
	}
}
