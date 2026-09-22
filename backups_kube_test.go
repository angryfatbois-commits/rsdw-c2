package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

type backupKubeTestRunner struct {
	run    func([]string) ([]byte, error)
	stream func([]string, io.Writer) error
	calls  []string
}

func (runner *backupKubeTestRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := append([]string{name}, args...)
	runner.calls = append(runner.calls, strings.Join(call, " "))
	if runner.run == nil {
		return nil, nil
	}
	return runner.run(call)
}

func (runner *backupKubeTestRunner) RunStream(_ context.Context, destination io.Writer, name string, args ...string) error {
	call := append([]string{name}, args...)
	runner.calls = append(runner.calls, strings.Join(call, " "))
	if runner.stream == nil {
		return nil
	}
	return runner.stream(call, destination)
}

func backupKubeTarget() podTarget {
	return podTarget{
		pod:       podTargetPod("backup-pod", "dragonwilds", "pod-uid"),
		container: telemetryContainer{Name: "server"},
	}
}

func podTargetPod(name, namespace, uid string) telemetryPod {
	return telemetryPod{Metadata: kubeMetadata{Name: name, Namespace: namespace, UID: uid}}
}

func TestResolveBackupPathUsesOnlyTheExpectedServerStateExtension(t *testing.T) {
	runner := &backupKubeTestRunner{run: func(call []string) ([]byte, error) {
		if strings.Contains(strings.Join(call, " "), "-printf") {
			return []byte("World.sav.backup\x00"), nil
		}
		if strings.Contains(strings.Join(call, " "), "World.sav.backup") {
			return []byte("file"), nil
		}
		return []byte("directory"), nil
	}}
	kube := &kubeOrchestrator{runner: runner, kubectl: "kubectl"}

	running, manifestPath, err := kube.resolveBackupPath(context.Background(), backupKubeTarget(), backupWorldSaveRoot, BackupItemFile, BackupServerRunning)
	if err != nil {
		t.Fatalf("resolve running source: %v", err)
	}
	if running != backupDataRoot+"/"+backupWorldSaveRoot+"/World.sav.backup" || manifestPath != backupWorldSaveRoot+"/World.sav.backup" {
		t.Fatalf("running source = %q, manifest path = %q", running, manifestPath)
	}

	runner.run = func(call []string) ([]byte, error) {
		if strings.Contains(strings.Join(call, " "), "-printf") {
			return []byte("World.sav\x00"), nil
		}
		if strings.Contains(strings.Join(call, " "), "World.sav") {
			return []byte("file"), nil
		}
		return []byte("directory"), nil
	}
	stopped, manifestPath, err := kube.resolveBackupPath(context.Background(), backupKubeTarget(), backupWorldSaveRoot, BackupItemFile, BackupServerStopped)
	if err != nil {
		t.Fatalf("resolve stopped source: %v", err)
	}
	if stopped != backupDataRoot+"/"+backupWorldSaveRoot+"/World.sav" || manifestPath != backupWorldSaveRoot+"/World.sav" {
		t.Fatalf("stopped source = %q, manifest path = %q", stopped, manifestPath)
	}

	runner.run = func(_ []string) ([]byte, error) { return nil, errors.New("not used for an explicit wrong extension") }
	if _, _, err := kube.resolveBackupPath(context.Background(), backupKubeTarget(), backupWorldSaveRoot+"/World.sav", BackupItemFile, BackupServerRunning); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("running .sav source returned %v", err)
	}
}

func TestResolveBackupPathRejectsMissingAndAmbiguousSources(t *testing.T) {
	for _, test := range []struct {
		name   string
		output string
		want   string
	}{
		{name: "missing", output: "", want: "found none"},
		{name: "ambiguous", output: "A.sav.backup\x00B.sav.backup\x00", want: "found 2"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &backupKubeTestRunner{run: func(call []string) ([]byte, error) {
				if strings.Contains(strings.Join(call, " "), "-printf") {
					return []byte(test.output), nil
				}
				return []byte("directory"), nil
			}}
			kube := &kubeOrchestrator{runner: runner, kubectl: "kubectl"}
			_, _, err := kube.resolveBackupPath(context.Background(), backupKubeTarget(), backupWorldSaveRoot, BackupItemFile, BackupServerRunning)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("resolve error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestResolveBackupPathAllowsExplicitNonSaveFile(t *testing.T) {
	runner := &backupKubeTestRunner{run: func(_ []string) ([]byte, error) {
		return []byte("file"), nil
	}}
	kube := &kubeOrchestrator{runner: runner, kubectl: "kubectl"}

	remote, manifestPath, err := kube.resolveBackupPath(context.Background(), backupKubeTarget(), "RSDragonwilds/Saved/Config/LinuxServer/DedicatedServer.ini", BackupItemFile, BackupServerRunning)
	if err != nil {
		t.Fatalf("resolve explicit config file: %v", err)
	}
	want := backupDataRoot + "/RSDragonwilds/Saved/Config/LinuxServer/DedicatedServer.ini"
	if remote != want || manifestPath != "RSDragonwilds/Saved/Config/LinuxServer/DedicatedServer.ini" {
		t.Fatalf("resolved config file = %q, %q", remote, manifestPath)
	}
	if len(runner.calls) == 0 || !strings.Contains(runner.calls[0], "test -f") {
		t.Fatalf("explicit config file did not use file validation: %v", runner.calls)
	}
}

func TestCaptureBackupItemVerifiesStableFileAndStreamsThroughLimit(t *testing.T) {
	stats := 0
	runner := &backupKubeTestRunner{
		run: func(call []string) ([]byte, error) {
			joined := strings.Join(call, " ")
			if strings.Contains(joined, "stat -c") {
				stats++
				return []byte(fmt.Sprintf("%s\x00f\x0012\x002026-01-01 00:00:00.000000001\x00%x\x00", backupDataRoot+"/"+backupWorldSaveRoot+"/World.sav.backup", sha256.Sum256([]byte("stable-world")))), nil
			}
			return []byte("file"), nil
		},
		stream: func(_ []string, destination io.Writer) error {
			_, err := io.Copy(destination, strings.NewReader("stable-world"))
			return err
		},
	}
	kube := &kubeOrchestrator{runner: runner, kubectl: "kubectl"}
	captureRoot := t.TempDir()
	item, file, err := kube.captureBackupItem(context.Background(), backupKubeTarget(), BackupItemSpec{Name: "world", Kind: BackupItemFile}, BackupSourceRule{Path: backupWorldSaveRoot + "/World.sav.backup"}, BackupServerRunning, captureRoot, 64)
	if err != nil {
		t.Fatalf("capture stable file: %v", err)
	}
	defer file.Close()
	content, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "stable-world" || item.SourcePath != backupWorldSaveRoot+"/World.sav.backup" || item.Consistency != BackupConsistencyStableRead || stats != 3 {
		t.Fatalf("captured item = %+v content=%q stat calls=%d", item, content, stats)
	}

	limited := &backupKubeTestRunner{run: runner.run, stream: func(_ []string, destination io.Writer) error {
		_, err := io.Copy(destination, strings.NewReader("too-large"))
		return err
	}}
	kube.runner = limited
	if _, _, err := kube.captureBackupItem(context.Background(), backupKubeTarget(), BackupItemSpec{Name: "world", Kind: BackupItemFile}, BackupSourceRule{Path: backupWorldSaveRoot + "/World.sav.backup"}, BackupServerRunning, captureRoot, 4); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("limited capture returned %v", err)
	}
}

func TestBackupLimitWriterDoesNotPartiallyWriteOverLimit(t *testing.T) {
	var destination bytes.Buffer
	writer := &backupLimitWriter{destination: &destination, limit: 4}
	if _, err := writer.Write([]byte("12345")); err == nil {
		t.Fatal("over-limit write succeeded")
	}
	if destination.Len() != 0 || writer.written != 0 {
		t.Fatalf("over-limit write partially changed destination=%q written=%d", destination.String(), writer.written)
	}
}

func TestWorldClaimRequiresExactlyOneOwnedPVC(t *testing.T) {
	server := Server{ID: "world", Namespace: "dragonwilds", Release: "world-release"}
	for _, test := range []struct {
		name  string
		items string
		want  string
		err   string
	}{
		{name: "one claim", items: `[{"metadata":{"name":"world-pvc","namespace":"dragonwilds"}}]`, want: "world-pvc"},
		{name: "no claims", items: `[]`, err: "found 0"},
		{name: "two claims", items: `[{"metadata":{"name":"a","namespace":"dragonwilds"}},{"metadata":{"name":"b","namespace":"dragonwilds"}}]`, err: "found 2"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &backupKubeTestRunner{run: func(call []string) ([]byte, error) {
				if !strings.Contains(strings.Join(call, " "), "get persistentvolumeclaims") {
					t.Fatalf("unexpected command %q", call)
				}
				return []byte(`{"items":` + test.items + `}`), nil
			}}
			kube := &kubeOrchestrator{runner: runner, kubectl: "kubectl"}
			claim, err := kube.worldClaim(context.Background(), server)
			if test.err != "" {
				if err == nil || !strings.Contains(err.Error(), test.err) {
					t.Fatalf("world claim error = %v, want %q", err, test.err)
				}
				return
			}
			if err != nil || claim != test.want {
				t.Fatalf("world claim = %q, error = %v", claim, err)
			}
		})
	}
}
