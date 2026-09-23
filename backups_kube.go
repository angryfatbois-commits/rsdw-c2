package main

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	backupDataRoot       = "/home/steam/rsdw-dedicated"
	backupCommandTimeout = 2 * time.Minute
	backupStableInterval = time.Second
)

type backupRemoteStat struct {
	snapshot string
}

var errBackupSourceMissing = errors.New("backup source is missing")

func (k *kubeOrchestrator) backupPodExec(ctx context.Context, target podTarget, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, backupCommandTimeout)
	defer cancel()
	container := defaultValue(target.container.Name, "server")
	base := []string{"-n", target.pod.Metadata.Namespace, "exec", "pod/" + target.pod.Metadata.Name, "-c", container, "--"}
	return k.runner.Run(ctx, k.kubectl, append(base, args...)...)
}

type backupLimitWriter struct {
	destination io.Writer
	written     int64
	limit       int64
}

func (writer *backupLimitWriter) Write(data []byte) (int, error) {
	if writer.limit > 0 && writer.written+int64(len(data)) > writer.limit {
		return 0, fmt.Errorf("backup item exceeds the configured %d byte limit", writer.limit)
	}
	count, err := writer.destination.Write(data)
	writer.written += int64(count)
	return count, err
}

func (k *kubeOrchestrator) collectBackup(ctx context.Context, server Server, definition BackupDefinition, source BackupSource, repositoryRoot string, maxItem int64) ([]BackupPublicationItem, func(), error) {
	state := BackupServerStopped
	if source == BackupSourceRunningBAK {
		state = BackupServerRunning
	} else if source != BackupSourceStoppedSAV {
		return nil, func() {}, errors.New("unsupported backup source")
	}
	target, targetCleanup, err := k.backupTarget(ctx, server, state)
	if err != nil {
		return nil, func() {}, err
	}
	captureParent := filepath.Join(repositoryRoot, ".capture")
	if err := os.MkdirAll(captureParent, 0o700); err != nil {
		targetCleanup()
		return nil, func() {}, fmt.Errorf("create backup capture directory: %w", err)
	}
	captureRoot, err := os.MkdirTemp(captureParent, "capture-")
	if err != nil {
		targetCleanup()
		return nil, func() {}, fmt.Errorf("create backup capture directory: %w", err)
	}
	var files []*os.File
	cleanup := func() {
		for _, file := range files {
			_ = file.Close()
		}
		_ = os.RemoveAll(captureRoot)
		targetCleanup()
	}

	items := make([]BackupPublicationItem, 0, len(definition.Items))
	for _, spec := range definition.Items {
		rule, ok := backupSourceRule(spec, state)
		if !ok {
			if spec.Requirement == BackupItemOptional {
				continue
			}
			cleanup()
			return nil, func() {}, fmt.Errorf("profile has no %s source rule for %s", state, spec.Name)
		}
		item, file, err := k.captureBackupItem(ctx, target, spec, *rule, state, captureRoot, maxItem)
		if err != nil {
			if spec.Requirement == BackupItemOptional && errors.Is(err, errBackupSourceMissing) {
				continue
			}
			cleanup()
			return nil, func() {}, fmt.Errorf("capture item %q: %w", spec.Name, err)
		}
		files = append(files, file)
		items = append(items, item)
	}
	if len(items) == 0 {
		cleanup()
		return nil, func() {}, errors.New("profile resolved no items")
	}
	if state == BackupServerRunning {
		after, status, resolveErr := k.resolvePod(ctx, server)
		if resolveErr != nil || status != StatusOnline || !sameContainer(target, after.pod) {
			cleanup()
			if resolveErr != nil {
				return nil, func() {}, fmt.Errorf("server changed during backup: %w", resolveErr)
			}
			return nil, func() {}, errors.New("server changed during backup")
		}
	} else {
		if resolveErr := k.verifyBackupStopped(ctx, server); resolveErr != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("verify server stopped after backup: %w", resolveErr)
		}
	}
	return items, cleanup, nil
}

func backupSourceRule(spec BackupItemSpec, state BackupServerState) (*BackupSourceRule, bool) {
	for index := range spec.Sources {
		if spec.Sources[index].ServerState == state {
			return &spec.Sources[index], true
		}
	}
	return nil, false
}

func (k *kubeOrchestrator) backupTarget(ctx context.Context, server Server, state BackupServerState) (podTarget, func(), error) {
	if state == BackupServerRunning {
		target, status, err := k.resolvePod(ctx, server)
		if err != nil {
			return podTarget{}, func() {}, fmt.Errorf("verify running server: %w", err)
		}
		if status != StatusOnline {
			return podTarget{}, func() {}, fmt.Errorf("server is not online (%s)", status)
		}
		return target, func() {}, nil
	}

	if state != BackupServerStopped {
		return podTarget{}, func() {}, errors.New("invalid backup server state")
	}
	if err := k.verifyBackupStopped(ctx, server); err != nil {
		return podTarget{}, func() {}, fmt.Errorf("verify stopped server: %w", err)
	}
	claim, err := k.worldClaim(ctx, server)
	if err != nil {
		return podTarget{}, func() {}, err
	}
	target, cleanup, err := k.backupInspector(ctx, server, claim)
	if err != nil {
		return podTarget{}, func() {}, err
	}
	if err := k.verifyBackupStopped(ctx, server); err != nil {
		cleanup()
		return podTarget{}, func() {}, err
	}
	return target, cleanup, nil
}

const backupInspectorLabel = "rsdw-c2/backup-inspector"

func (k *kubeOrchestrator) backupInspector(ctx context.Context, server Server, claim string) (podTarget, func(), error) {
	if claim == "" {
		return podTarget{}, func() {}, errors.New("world PVC is required")
	}
	image := defaultValue(server.CurrentImage, server.DesiredImage)
	if image == "" {
		return podTarget{}, func() {}, errors.New("server image is unavailable for stopped-world inspection")
	}
	if _, _, _, err := imageReferenceValues(image); err != nil {
		return podTarget{}, func() {}, fmt.Errorf("server image is invalid: %w", err)
	}
	podName := newBackupID("rsdw-c2-backup")
	overrides := map[string]any{"metadata": map[string]any{"labels": map[string]string{backupInspectorLabel: server.ID}}, "spec": map[string]any{
		"automountServiceAccountToken":  false,
		"activeDeadlineSeconds":         600,
		"terminationGracePeriodSeconds": 1,
		"restartPolicy":                 "Never",
		"securityContext": map[string]any{
			"runAsUser": 1000, "runAsGroup": 1000, "runAsNonRoot": true, "fsGroup": 1000,
			"seccompProfile": map[string]string{"type": "RuntimeDefault"},
		},
		"containers": []any{map[string]any{
			"name": "backup", "image": image, "command": []string{"sh", "-ec", "sleep 600"},
			"securityContext": map[string]any{
				"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
				"capabilities": map[string]any{"drop": []string{"ALL"}},
			},
			"resources": map[string]any{
				"requests": map[string]string{"cpu": "10m", "memory": "8Mi"},
				"limits":   map[string]string{"cpu": "100m", "memory": "64Mi"},
			},
			"volumeMounts": []any{map[string]any{"name": "data", "mountPath": backupDataRoot, "readOnly": true}},
		}},
		"volumes": []any{map[string]any{"name": "data", "persistentVolumeClaim": map[string]any{"claimName": claim, "readOnly": true}}},
	}}
	overrideJSON, err := json.Marshal(overrides)
	if err != nil {
		return podTarget{}, func() {}, fmt.Errorf("encode stopped-world inspection pod: %w", err)
	}
	createContext, cancel := context.WithTimeout(ctx, backupCommandTimeout)
	defer cancel()
	if _, err := k.runner.Run(createContext, k.kubectl, "-n", server.Namespace, "run", podName, "--restart=Never", "--image", image, "--overrides", string(overrideJSON)); err != nil {
		return podTarget{}, func() {}, fmt.Errorf("create stopped-world inspection pod: %w", err)
	}
	cleanup := func() {
		deleteContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = k.runner.Run(deleteContext, k.kubectl, "-n", server.Namespace, "delete", "pod", podName, "--ignore-not-found=true", "--wait=true", "--timeout=25s")
	}
	waitContext, cancelWait := context.WithTimeout(ctx, backupCommandTimeout)
	defer cancelWait()
	if _, err := k.runner.Run(waitContext, k.kubectl, "-n", server.Namespace, "wait", "--for=jsonpath={.status.phase}=Running", "pod/"+podName, "--timeout=90s"); err != nil {
		cleanup()
		return podTarget{}, func() {}, fmt.Errorf("wait for stopped-world inspection pod: %w", err)
	}
	var pod telemetryPod
	if err := k.kubeJSON(waitContext, &pod, "-n", server.Namespace, "get", "pod", podName, "-o", "json"); err != nil {
		cleanup()
		return podTarget{}, func() {}, fmt.Errorf("verify stopped-world inspection pod: %w", err)
	}
	if pod.Metadata.Name != podName || pod.Metadata.Namespace != server.Namespace || pod.Metadata.UID == "" || pod.Status.Phase != "Running" {
		cleanup()
		return podTarget{}, func() {}, errors.New("stopped-world inspection pod identity is invalid")
	}
	var container telemetryContainer
	count := 0
	for _, candidate := range pod.Spec.Containers {
		if candidate.Name == "backup" {
			container = candidate
			count++
		}
	}
	if count != 1 {
		cleanup()
		return podTarget{}, func() {}, errors.New("stopped-world inspection pod container is invalid")
	}
	return podTarget{pod: pod, container: container}, cleanup, nil
}

func (k *kubeOrchestrator) verifyBackupStopped(ctx context.Context, server Server) error {
	var deployment struct {
		Metadata kubeMetadata `json:"metadata"`
		Spec     struct {
			Replicas *int `json:"replicas"`
		} `json:"spec"`
	}
	if err := k.kubeJSON(ctx, &deployment, "-n", server.Namespace, "get", "deployment", deploymentName(server.Release), "-o", "json"); err != nil {
		return fmt.Errorf("verify stopped deployment: %w", err)
	}
	if deployment.Metadata.UID == "" || deployment.Metadata.Namespace != server.Namespace || deployment.Metadata.Name != deploymentName(server.Release) || deployment.Metadata.DeletionTimestamp != nil || deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 0 {
		return errors.New("server deployment is not verified stopped")
	}
	var replicas struct {
		Items []struct {
			Metadata kubeMetadata `json:"metadata"`
		} `json:"items"`
	}
	if err := k.kubeJSON(ctx, &replicas, "-n", server.Namespace, "get", "replicasets", "-o", "json"); err != nil {
		return err
	}
	if replicas.Items == nil {
		return errors.New("ReplicaSet list is missing items")
	}
	owned := map[string]bool{}
	for _, rs := range replicas.Items {
		if rs.Metadata.ownedBy("Deployment", deployment.Metadata.UID) && rs.Metadata.UID != "" {
			owned[rs.Metadata.UID] = true
		}
	}
	claim, err := k.worldClaim(ctx, server)
	if err != nil {
		return err
	}
	var listing struct {
		Items []struct {
			Metadata kubeMetadata `json:"metadata"`
			Spec     struct {
				Volumes []struct {
					PVC *struct {
						ClaimName string `json:"claimName"`
					} `json:"persistentVolumeClaim"`
				} `json:"volumes"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := k.kubeJSON(ctx, &listing, "-n", server.Namespace, "get", "pods", "-o", "json"); err != nil {
		return fmt.Errorf("verify stopped world pods: %w", err)
	}
	if listing.Items == nil {
		return errors.New("Pod list is missing items")
	}
	for _, pod := range listing.Items {
		if pod.Metadata.Labels[backupInspectorLabel] == server.ID && server.ID != "" {
			continue
		}
		for uid := range owned {
			if pod.Metadata.ownedBy("ReplicaSet", uid) {
				return fmt.Errorf("server pod %s still exists", pod.Metadata.Name)
			}
		}
		if pod.Metadata.Labels["app.kubernetes.io/instance"] == server.Release {
			return fmt.Errorf("server pod %s still exists", pod.Metadata.Name)
		}
		for _, volume := range pod.Spec.Volumes {
			if volume.PVC != nil && volume.PVC.ClaimName == claim {
				return fmt.Errorf("pod %s still references world PVC %s", pod.Metadata.Name, claim)
			}
		}
	}
	return nil
}

func (k *kubeOrchestrator) worldClaim(ctx context.Context, server Server) (string, error) {
	var listing struct {
		Items []struct {
			Metadata kubeMetadata
		}
	}
	if err := k.kubeJSON(ctx, &listing, "-n", server.Namespace, "get", "persistentvolumeclaims", "-l", "app.kubernetes.io/instance="+server.Release+",app.kubernetes.io/name=rsdragonwilds", "-o", "json"); err != nil {
		return "", fmt.Errorf("find server world PVC: %w", err)
	}
	claims := make([]string, 0, len(listing.Items))
	for _, item := range listing.Items {
		if item.Metadata.Namespace == server.Namespace && item.Metadata.Name != "" && item.Metadata.DeletionTimestamp == nil {
			claims = append(claims, item.Metadata.Name)
		}
	}
	sort.Strings(claims)
	if len(claims) != 1 {
		return "", fmt.Errorf("expected one world PVC for %s, found %d", server.ID, len(claims))
	}
	return claims[0], nil
}

func (k *kubeOrchestrator) captureBackupItem(ctx context.Context, target podTarget, spec BackupItemSpec, rule BackupSourceRule, state BackupServerState, captureRoot string, maxItem int64) (BackupPublicationItem, *os.File, error) {
	kind := spec.Kind
	if kind == "" {
		kind = BackupItemFile
	}
	if err := ValidateBackupRelativePath(rule.Path); err != nil {
		return BackupPublicationItem{}, nil, err
	}
	if strings.ContainsAny(rule.Path, "*?[]") {
		return BackupPublicationItem{}, nil, errors.New("wildcard source paths are not supported")
	}
	remotePath, manifestPath, err := k.resolveBackupPath(ctx, target, rule.Path, kind, state)
	if err != nil {
		return BackupPublicationItem{}, nil, err
	}
	destination, err := os.CreateTemp(captureRoot, "item-")
	if err != nil {
		return BackupPublicationItem{}, nil, fmt.Errorf("create capture file: %w", err)
	}
	cleanupFile := func() {
		name := destination.Name()
		_ = destination.Close()
		_ = os.Remove(name)
	}
	before, err := k.remoteFingerprint(ctx, target, remotePath, kind, state)
	if err != nil {
		cleanupFile()
		return BackupPublicationItem{}, nil, fmt.Errorf("read source before copy: %w", err)
	}
	timer := time.NewTimer(backupStableInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		cleanupFile()
		return BackupPublicationItem{}, nil, ctx.Err()
	case <-timer.C:
	}
	stable, err := k.remoteFingerprint(ctx, target, remotePath, kind, state)
	if err != nil || stable != before {
		cleanupFile()
		return BackupPublicationItem{}, nil, errors.New("source changed before copy or could not be verified")
	}
	writer := &backupLimitWriter{destination: destination, limit: maxItem}
	guard := backupPathGuard(remotePath) + backupTreeGuard(remotePath, kind, state)
	if kind == BackupItemDirectory {
		exclude := ""
		if state == BackupServerRunning {
			exclude = " --exclude='*.[sS][aA][v]'"
		}
		if err := k.podExecStream(ctx, target, writer, "sh", "-ec", guard+"exec tar -cf - -C "+shellQuote(backupDataRoot)+exclude+" -- "+shellQuote(manifestPath)); err != nil {
			cleanupFile()
			return BackupPublicationItem{}, nil, fmt.Errorf("copy directory: %w", err)
		}
	} else if err := k.podExecStream(ctx, target, writer, "sh", "-ec", guard+"exec cat -- "+shellQuote(remotePath)); err != nil {
		cleanupFile()
		return BackupPublicationItem{}, nil, fmt.Errorf("copy file: %w", err)
	}
	if err := destination.Sync(); err != nil {
		cleanupFile()
		return BackupPublicationItem{}, nil, fmt.Errorf("sync capture file: %w", err)
	}
	after, err := k.remoteFingerprint(ctx, target, remotePath, kind, state)
	if err != nil {
		cleanupFile()
		return BackupPublicationItem{}, nil, fmt.Errorf("read source after copy: %w", err)
	}
	if before != after {
		cleanupFile()
		return BackupPublicationItem{}, nil, errors.New("source changed during copy")
	}
	if err := verifyBackupCapture(destination, before, manifestPath, kind, state); err != nil {
		cleanupFile()
		return BackupPublicationItem{}, nil, err
	}
	if _, err := destination.Seek(0, io.SeekStart); err != nil {
		cleanupFile()
		return BackupPublicationItem{}, nil, fmt.Errorf("rewind capture file: %w", err)
	}
	consistency := BackupConsistencyStableRead
	if state == BackupServerStopped {
		consistency = BackupConsistencyStoppedWorld
	}
	return BackupPublicationItem{Name: spec.Name, Kind: kind, SourcePath: manifestPath, Consistency: consistency, Content: destination}, destination, nil
}

func (k *kubeOrchestrator) resolveBackupPath(ctx context.Context, target podTarget, relative string, kind BackupItemKind, state BackupServerState) (string, string, error) {
	if err := ValidateBackupRelativePath(relative); err != nil {
		return "", "", err
	}
	if strings.ContainsAny(relative, "*?[]") {
		return "", "", errors.New("wildcard source paths are not supported")
	}
	if kind != BackupItemFile && kind != BackupItemDirectory {
		return "", "", errors.New("invalid backup item kind")
	}
	remote := path.Join(backupDataRoot, relative)
	extension := ".sav"
	if state == BackupServerRunning {
		extension = ".sav.backup"
	}
	declared := backupSaveSuffix(relative)
	if kind == BackupItemFile && declared != "" && declared != extension {
		return "", "", errors.New("source file extension does not match the verified server state")
	}
	output, err := k.backupPodExec(ctx, target, "sh", "-ec", backupPathGuard(remote)+
		"if test -f "+shellQuote(remote)+"; then printf file; elif test -d "+shellQuote(remote)+"; then printf directory; else exit 1; fi")
	if err != nil {
		return "", "", fmt.Errorf("inspect source path: %w", err)
	}
	if string(output) == "MISSING" {
		return "", "", errBackupSourceMissing
	}
	if kind == BackupItemDirectory {
		if string(output) != "directory" {
			return "", "", errors.New("source is not a directory")
		}
		if _, err := k.backupPodExec(ctx, target, "sh", "-ec", backupPathGuard(remote)+backupTreeGuard(remote, kind, state)); err != nil {
			return "", "", fmt.Errorf("unsafe source directory: %w", err)
		}
		return remote, relative, nil
	}
	if string(output) == "file" {
		return remote, relative, nil
	}
	if string(output) != "directory" {
		return "", "", errors.New("invalid source path type")
	}
	script := backupPathGuard(remote) + "find " + shellQuote(remote) + " -mindepth 1 -maxdepth 1 -type f -iname " + shellQuote("*"+extension) + " -print0"
	output, err = k.backupPodExec(ctx, target, "sh", "-ec", script)
	if err != nil {
		return "", "", fmt.Errorf("discover source file: %w", err)
	}
	if len(output) == 0 || string(output) == "MISSING" {
		return "", "", fmt.Errorf("expected one %s source in %s, found none: %w", extension, relative, errBackupSourceMissing)
	}
	if output[len(output)-1] != 0 {
		return "", "", errors.New("source file listing is not NUL-terminated")
	}
	names := strings.Split(string(output[:len(output)-1]), "\x00")
	prefix := remote + "/"
	for index, name := range names {
		if !strings.HasPrefix(name, prefix) {
			return "", "", errors.New("source file listing contains an unexpected path")
		}
		names[index] = strings.TrimPrefix(name, prefix)
	}
	if len(names) != 1 {
		return "", "", fmt.Errorf("expected one %s source in %s, found %d", extension, relative, len(names))
	}
	if strings.ContainsAny(names[0], "/\\\x00") || backupSaveSuffix(names[0]) != extension {
		return "", "", errors.New("resolved source filename is invalid")
	}
	resolved := path.Join(relative, names[0])
	return k.resolveBackupPath(ctx, target, resolved, BackupItemFile, state)
}

// The root can be a volume mount, but no component may be a symlink.
func backupPathGuard(remote string) string {
	script := "set -eu; "
	current := "/"
	for _, part := range strings.Split(strings.TrimPrefix(remote, "/"), "/") {
		current = path.Join(current, part)
		quoted := shellQuote(current)
		script += "test ! -L " + quoted + "; "
		if current != remote {
			script += "if test -e " + quoted + "; then test -d " + quoted + "; test -x " + quoted + "; else printf MISSING; exit 0; fi; "
		} else {
			script += "if ! test -e " + quoted + "; then printf MISSING; exit 0; fi; "
		}
	}
	return script
}

func backupTreeGuard(remote string, kind BackupItemKind, state BackupServerState) string {
	if kind != BackupItemDirectory {
		return "test -f " + shellQuote(remote) + "; "
	}
	script := "test -d " + shellQuote(remote) + "; bad=$(find " + shellQuote(remote) + " ! -type f ! -type d -print -quit); test -z \"$bad\"; "
	if state == BackupServerRunning {
		script += "bad=$(find " + shellQuote(remote) + " -type f -iname '*.sav' -print -quit); test -z \"$bad\"; "
	}
	return script
}

func (k *kubeOrchestrator) assertRemoteDirectory(ctx context.Context, target podTarget, remote string) error {
	output, err := k.backupPodExec(ctx, target, "sh", "-ec", backupPathGuard(remote)+backupTreeGuard(remote, BackupItemDirectory, BackupServerStopped))
	if err != nil {
		return fmt.Errorf("unsafe source directory: %w", err)
	}
	if string(output) == "MISSING" {
		return errBackupSourceMissing
	}
	return nil
}

func (k *kubeOrchestrator) assertRemoteFile(ctx context.Context, target podTarget, remote, extension string) error {
	if extension != "" && backupSaveSuffix(remote) != extension {
		return errors.New("source file extension does not match the verified server state")
	}
	output, err := k.backupPodExec(ctx, target, "sh", "-ec", backupPathGuard(remote)+backupTreeGuard(remote, BackupItemFile, BackupServerStopped))
	if err != nil {
		return fmt.Errorf("unsafe source file: %w", err)
	}
	if string(output) == "MISSING" {
		return errBackupSourceMissing
	}
	return nil
}

func (k *kubeOrchestrator) remoteFingerprint(ctx context.Context, target podTarget, remote string, kind BackupItemKind, state BackupServerState) (backupRemoteStat, error) {
	// NUL records preserve spaces and newlines in filenames. stat includes nanoseconds.
	record := `set -eu
for file do
 test ! -L "$file"
 if test -f "$file"; then
 type=f
 size=$(stat -c %s -- "$file")
 digest=$(sha256sum < "$file")
 digest=${digest%% *}
 elif test -d "$file"; then
 type=d
 size=0
 digest=-
 else
 exit 1
 fi
 metadata=$(stat -c '%y %z %i %d %a' -- "$file")
 printf '%s\000%s\000%s\000%s\000%s\000' "$file" "$type" "$size" "$metadata" "$digest"
done`
	script := backupPathGuard(remote)
	if kind == BackupItemDirectory {
		script += backupTreeGuard(remote, kind, state)
		exclude := ""
		if state == BackupServerRunning {
			exclude = " ! -iname '*.sav'"
		}
		script += "find " + shellQuote(remote) + exclude + " -exec sh -ec " + shellQuote(record) + " sh {} +"
	} else {
		script += "sh -ec " + shellQuote(record) + " sh " + shellQuote(remote)
	}
	output, err := k.backupPodExec(ctx, target, "sh", "-ec", script)
	if err != nil {
		return backupRemoteStat{}, err
	}
	if string(output) == "MISSING" {
		return backupRemoteStat{}, errors.New("source disappeared during capture")
	}
	records, err := backupFingerprintRecords(string(output))
	if err != nil {
		return backupRemoteStat{}, err
	}
	keys := make([]string, 0, len(records))
	for key := range records {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var canonical strings.Builder
	for _, key := range keys {
		canonical.WriteString(strings.Join(records[key], "\x00") + "\x00")
	}
	return backupRemoteStat{snapshot: canonical.String()}, nil
}

func backupFingerprintRecords(snapshot string) (map[string][]string, error) {
	fields := strings.Split(snapshot, "\x00")
	if len(fields) < 6 || fields[len(fields)-1] != "" || (len(fields)-1)%5 != 0 {
		return nil, errors.New("invalid source fingerprint")
	}
	records := make(map[string][]string)
	for i := 0; i < len(fields)-1; i += 5 {
		record := fields[i : i+5]
		if _, exists := records[record[0]]; exists {
			return nil, errors.New("duplicate source fingerprint")
		}
		size, err := strconv.ParseInt(record[2], 10, 64)
		if err != nil || size < 0 || (record[1] != "f" && record[1] != "d") || (record[1] == "f" && len(record[4]) != 64) {
			return nil, errors.New("invalid source fingerprint")
		}
		records[record[0]] = record
	}
	return records, nil
}

func verifyBackupCapture(file *os.File, fingerprint backupRemoteStat, relative string, kind BackupItemKind, state BackupServerState) error {
	records, err := backupFingerprintRecords(fingerprint.snapshot)
	if err != nil {
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	check := func(name string, content io.Reader, directory bool) error {
		record, ok := records[path.Join(backupDataRoot, name)]
		if !ok || (record[1] == "d") != directory {
			return errors.New("captured content does not match source")
		}
		delete(records, record[0])
		if directory {
			return nil
		}
		hash := sha256.New()
		count, err := io.Copy(hash, content)
		if err != nil {
			return err
		}
		if strconv.FormatInt(count, 10) != record[2] || fmt.Sprintf("%x", hash.Sum(nil)) != record[4] {
			return errors.New("captured byte count or checksum does not match source")
		}
		return nil
	}
	if kind == BackupItemFile {
		if err := check(relative, file, false); err != nil {
			return err
		}
	} else {
		archive := tar.NewReader(file)
		for {
			header, err := archive.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("invalid captured archive: %w", err)
			}
			name := strings.TrimSuffix(header.Name, "/")
			if err := ValidateBackupRelativePath(name); err != nil || (name != relative && !strings.HasPrefix(name, relative+"/")) {
				return errors.New("unsafe captured archive path")
			}
			if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeDir {
				return errors.New("unsafe captured archive entry")
			}
			if state == BackupServerRunning && strings.EqualFold(path.Ext(name), ".sav") {
				return errors.New("running archive contains a .sav file")
			}
			if err := check(name, archive, header.Typeflag == tar.TypeDir); err != nil {
				return err
			}
		}
	}
	if len(records) != 0 {
		return errors.New("captured content is incomplete")
	}
	return nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
