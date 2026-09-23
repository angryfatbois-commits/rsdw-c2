package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestVerifyBackupStoppedChecksActualPods(t *testing.T) {
	server := Server{ID: "world", Namespace: "dragonwilds", Release: "world-release"}
	for _, test := range []struct {
		name, pods string
		allowed    bool
	}{
		{name: "empty", pods: `[]`, allowed: true},
		{name: "terminating writer", pods: `[{"metadata":{"name":"old","deletionTimestamp":"2026-01-01T00:00:00Z"},"spec":{"volumes":[{"persistentVolumeClaim":{"claimName":"world-pvc"}}]}}]`},
		{name: "unlabelled writer", pods: `[{"metadata":{"name":"other"},"spec":{"volumes":[{"persistentVolumeClaim":{"claimName":"world-pvc"}}]}}]`},
		{name: "owned pod", pods: `[{"metadata":{"name":"old","ownerReferences":[{"kind":"ReplicaSet","uid":"rs-uid","controller":true}]}}]`},
		{name: "labelled pod", pods: `[{"metadata":{"name":"old","labels":{"app.kubernetes.io/instance":"world-release"}}}]`},
		{name: "inspector", pods: `[{"metadata":{"name":"helper","labels":{"rsdw-c2/backup-inspector":"world"}},"spec":{"volumes":[{"persistentVolumeClaim":{"claimName":"world-pvc"}}]}}]`, allowed: true},
		{name: "other inspector", pods: `[{"metadata":{"name":"helper","labels":{"rsdw-c2/backup-inspector":"other"}},"spec":{"volumes":[{"persistentVolumeClaim":{"claimName":"world-pvc"}}]}}]`},
		{name: "missing pod list", pods: `null`},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &backupKubeTestRunner{run: func(args []string) ([]byte, error) {
				call := strings.Join(args, " ")
				switch {
				case strings.Contains(call, "get deployment "):
					return []byte(fmt.Sprintf(`{"metadata":{"name":%q,"namespace":"dragonwilds","uid":"deployment-uid"},"spec":{"replicas":0}}`, deploymentName(server.Release))), nil
				case strings.Contains(call, "get replicasets "):
					return []byte(`{"items":[{"metadata":{"uid":"rs-uid","ownerReferences":[{"kind":"Deployment","uid":"deployment-uid","controller":true}]}}]}`), nil
				case strings.Contains(call, "get persistentvolumeclaims "):
					return []byte(`{"items":[{"metadata":{"name":"world-pvc","namespace":"dragonwilds"}}]}`), nil
				case strings.Contains(call, "get pods "):
					return []byte(`{"items":` + test.pods + `}`), nil
				default:
					t.Fatalf("unexpected command %s", call)
					return nil, nil
				}
			}}
			k := &kubeOrchestrator{runner: runner, kubectl: "unused"}
			if err := k.verifyBackupStopped(context.Background(), server); (err == nil) != test.allowed {
				t.Fatalf("stopped result = %v", err)
			}
		})
	}
}

func TestBackupInspectorDoesNotRequireDeployment(t *testing.T) {
	server := Server{ID: "world", Namespace: "dragonwilds", Release: "world-release", DesiredImage: "ghcr.io/example/server:latest"}
	var name string
	deleted := false
	runner := &backupKubeTestRunner{run: func(args []string) ([]byte, error) {
		call := strings.Join(args, " ")
		switch {
		case strings.Contains(call, " run "):
			name = args[4]
			var override map[string]any
			if err := json.Unmarshal([]byte(args[len(args)-1]), &override); err != nil {
				t.Fatal(err)
			}
			assertBackupInspectorIsReadOnlyAndRestricted(t, override)
			if !strings.Contains(call, "sleep 600") || !strings.Contains(call, backupInspectorLabel) || !strings.Contains(call, "new-world-pvc") {
				t.Fatalf("wrong inspector command %s", call)
			}
		case strings.Contains(call, " wait "):
		case strings.Contains(call, " get pod "):
			return []byte(fmt.Sprintf(`{"metadata":{"name":%q,"namespace":"dragonwilds","uid":"helper-uid"},"spec":{"containers":[{"name":"backup"}]},"status":{"phase":"Running"}}`, name)), nil
		case strings.Contains(call, " delete pod "):
			deleted = true
		default:
			t.Fatalf("unexpected inspector command %s", call)
		}
		return nil, nil
	}}
	k := &kubeOrchestrator{runner: runner, kubectl: "unused"}
	target, cleanup, err := k.backupInspector(context.Background(), server, "new-world-pvc")
	if err != nil {
		t.Fatal(err)
	}
	if target.container.Name != "backup" || deleted {
		t.Fatal("inspector not available to restore")
	}
	cleanup()
	if !deleted {
		t.Fatal("cleanup did not delete inspector")
	}
}

func assertBackupInspectorIsReadOnlyAndRestricted(t *testing.T, override map[string]any) {
	t.Helper()
	spec, ok := override["spec"].(map[string]any)
	if !ok {
		t.Fatalf("inspector spec = %#v", override["spec"])
	}
	if spec["automountServiceAccountToken"] != false {
		t.Fatal("inspector must not mount a service account token")
	}
	podSecurity, ok := spec["securityContext"].(map[string]any)
	if !ok || podSecurity["runAsNonRoot"] != true || podSecurity["runAsUser"] != float64(1000) || podSecurity["runAsGroup"] != float64(1000) {
		t.Fatalf("inspector pod security context = %#v", spec["securityContext"])
	}
	seccomp, ok := podSecurity["seccompProfile"].(map[string]any)
	if !ok || seccomp["type"] != "RuntimeDefault" {
		t.Fatalf("inspector seccomp profile = %#v", podSecurity["seccompProfile"])
	}
	containers, ok := spec["containers"].([]any)
	if !ok || len(containers) != 1 {
		t.Fatalf("inspector containers = %#v", spec["containers"])
	}
	container, ok := containers[0].(map[string]any)
	if !ok {
		t.Fatalf("inspector container = %#v", containers[0])
	}
	containerSecurity, ok := container["securityContext"].(map[string]any)
	if !ok || containerSecurity["allowPrivilegeEscalation"] != false || containerSecurity["readOnlyRootFilesystem"] != true {
		t.Fatalf("inspector container security context = %#v", container["securityContext"])
	}
	capabilities, ok := containerSecurity["capabilities"].(map[string]any)
	dropped, okDrop := capabilities["drop"].([]any)
	if !ok || !okDrop || len(dropped) != 1 || dropped[0] != "ALL" {
		t.Fatalf("inspector capabilities = %#v", containerSecurity["capabilities"])
	}
	resources, ok := container["resources"].(map[string]any)
	if !ok || resources["requests"] == nil || resources["limits"] == nil {
		t.Fatalf("inspector resource bounds = %#v", container["resources"])
	}
	mounts, ok := container["volumeMounts"].([]any)
	if !ok || len(mounts) != 1 {
		t.Fatalf("inspector volume mounts = %#v", container["volumeMounts"])
	}
	volumeMount, ok := mounts[0].(map[string]any)
	if !ok || volumeMount["readOnly"] != true {
		t.Fatalf("inspector volume mount = %#v", mounts[0])
	}
	volumes, ok := spec["volumes"].([]any)
	if !ok || len(volumes) != 1 {
		t.Fatalf("inspector volumes = %#v", spec["volumes"])
	}
	volume, ok := volumes[0].(map[string]any)
	if !ok {
		t.Fatalf("inspector volume = %#v", volumes[0])
	}
	claim, ok := volume["persistentVolumeClaim"].(map[string]any)
	if !ok || claim["readOnly"] != true {
		t.Fatalf("inspector PVC = %#v", volume["persistentVolumeClaim"])
	}
}

func TestBackupPodExecAllowsLongerThanTelemetry(t *testing.T) {
	k, _ := backupFilesystem(t)
	output, err := k.backupPodExec(context.Background(), backupKubeTarget(), "sh", "-ec", "sleep 1.6; printf done")
	if err != nil || string(output) != "done" {
		t.Fatalf("backup command = %q, %v", output, err)
	}
}
