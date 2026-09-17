package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

const maxSaveBytes = 32 << 20
const maxCreateBytes = maxSaveBytes + (64 << 10)
const seedLabel = "rsdw-c2.petzko.sh/seed"

var errSaveTooLarge = errors.New("save must be at most 32 MiB; the complete upload request must be at most 32 MiB plus 64 KiB")

type SaveSeed struct {
	Claim string `json:"claim"`
	Path  string `json:"path"`
}

type PendingSeed struct {
	Namespace string `json:"namespace"`
	Release   string `json:"release"`
}

type saveUpload struct {
	name string
	data []byte
}

func decodeCreate(w http.ResponseWriter, r *http.Request) (CreateServerRequest, *saveUpload, error) {
	var request CreateServerRequest
	kind, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || kind != "multipart/form-data" {
		if r.Header.Get("Content-Type") != "" && kind != "application/json" {
			return request, nil, errors.New("use application/json or multipart/form-data with request and save parts")
		}
		err := decodeJSON(r, &request)
		return request, nil, err
	}
	if r.ContentLength > maxCreateBytes {
		return request, nil, errSaveTooLarge
	}
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(2 * time.Minute))
	_ = controller.SetWriteDeadline(time.Now().Add(5 * time.Minute))
	r.Body = http.MaxBytesReader(w, r.Body, maxCreateBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return request, nil, errSaveTooLarge
		}
		return request, nil, errors.New("could not read save upload")
	}
	reader := multipart.NewReader(bytes.NewReader(data), params["boundary"])
	var upload *saveUpload
	seenRequest := false
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return request, nil, errors.New("invalid multipart upload")
		}
		_, disposition, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
		if err != nil {
			return request, nil, errors.New("invalid upload part")
		}
		switch part.FormName() {
		case "request":
			if seenRequest || disposition["filename"] != "" {
				return request, nil, errors.New("provide exactly one request part")
			}
			metadata, err := io.ReadAll(io.LimitReader(part, (32<<10)+1))
			if err != nil || len(metadata) > 32<<10 {
				return request, nil, errors.New("server settings must be at most 32 KiB")
			}
			decoder := json.NewDecoder(bytes.NewReader(metadata))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&request); err != nil {
				return request, nil, errors.New("invalid server settings JSON")
			}
			if decoder.Decode(new(any)) != io.EOF {
				return request, nil, errors.New("provide exactly one settings object")
			}
			seenRequest = true
		case "save":
			name := disposition["filename"]
			if upload != nil {
				return request, nil, errors.New("provide exactly one .sav file")
			}
			if !validSaveName(name) {
				return request, nil, errors.New("select a .sav file with a plain filename of at most 128 bytes, without paths or control characters")
			}
			content, err := io.ReadAll(part)
			if err != nil {
				return request, nil, errors.New("could not read save file")
			}
			if len(content) > maxSaveBytes {
				return request, nil, errSaveTooLarge
			}
			if len(content) == 0 {
				return request, nil, errors.New("save file must not be empty")
			}
			upload = &saveUpload{name: name, data: content}
		default:
			return request, nil, errors.New("upload accepts only request and save parts")
		}
	}
	if !seenRequest || upload == nil {
		return request, nil, errors.New("upload requires one request part and one .sav file")
	}
	return request, upload, nil
}

func validSaveName(name string) bool {
	return len(name) > 4 && len(name) <= 128 && !strings.HasPrefix(name, ".") &&
		strings.HasSuffix(name, ".sav") && !strings.ContainsAny(name, `/\`) &&
		strings.IndexFunc(name, unicode.IsControl) < 0
}

func (a *App) seedRoot() (*os.Root, error) {
	if a.store.path == "" {
		return nil, errors.New("save staging requires persistent state")
	}
	parent, err := os.OpenRoot(filepath.Dir(a.store.path))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	if err := parent.Mkdir("save-staging", 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	info, err := parent.Lstat("save-staging")
	if err != nil || !info.IsDir() {
		return nil, errors.New("save staging must be a directory, not a symlink")
	}
	return parent.OpenRoot("save-staging")
}

func writeSeedFile(root *os.Root, name string, data []byte) error {
	file, err := root.OpenFile(name+".tmp", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	defer root.Remove(name + ".tmp")
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if _, err := root.Lstat(name); !errors.Is(err, os.ErrNotExist) {
		return errors.New("staged save already exists")
	}
	return root.Rename(name+".tmp", name)
}

func (a *App) prepareSeed(ctx context.Context, k *kubeOrchestrator, server Server, upload *saveUpload) (SaveSeed, error) {
	token, err := randomToken()
	if err != nil {
		return SaveSeed{}, err
	}
	seed := SaveSeed{Claim: "rsdw-seed-" + token[:20], Path: upload.name}
	if err := a.store.Update(func(state *State) error {
		if state.PendingSeeds == nil {
			state.PendingSeeds = make(map[string]PendingSeed)
		}
		state.PendingSeeds[seed.Claim] = PendingSeed{Namespace: server.Namespace, Release: server.Release}
		return nil
	}); err != nil {
		return SaveSeed{}, err
	}
	root, err := a.seedRoot()
	if err != nil {
		return seed, err
	}
	defer root.Close()
	if err := writeSeedFile(root, seed.Claim+".sav", upload.data); err != nil {
		return seed, err
	}
	return seed, k.stageSeed(ctx, server.Namespace, seed, root)
}

func (k *kubeOrchestrator) stageSeed(ctx context.Context, namespace string, seed SaveSeed, root *os.Root) error {
	if _, err := k.runner.Run(ctx, k.kubectl, "get", "namespace", namespace); err != nil {
		if _, err := k.runner.Run(ctx, k.kubectl, "create", "namespace", namespace); err != nil {
			return err
		}
	}
	metadata := map[string]any{"name": seed.Claim, "namespace": namespace, "labels": map[string]string{seedLabel: seed.Claim}}
	manifest := map[string]any{"apiVersion": "v1", "kind": "List", "items": []any{
		map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": metadata, "spec": map[string]any{
			"accessModes": []string{"ReadWriteOnce"}, "resources": map[string]any{"requests": map[string]string{"storage": "64Mi"}},
		}},
		map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": metadata, "spec": map[string]any{
			"automountServiceAccountToken": false, "restartPolicy": "Never", "activeDeadlineSeconds": 180,
			"securityContext": map[string]any{"runAsUser": 1000, "runAsGroup": 1000, "runAsNonRoot": true, "fsGroup": 1000, "seccompProfile": map[string]string{"type": "RuntimeDefault"}},
			"containers": []any{map[string]any{
				"name": "writer", "image": envOr("RSDW_SEED_WRITER_IMAGE", "busybox:1.37.0"), "command": []string{"sleep", "180"},
				"securityContext": map[string]any{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": map[string]any{"drop": []string{"ALL"}}},
				"resources":       map[string]any{"requests": map[string]string{"cpu": "10m", "memory": "8Mi"}, "limits": map[string]string{"cpu": "100m", "memory": "32Mi"}},
				"volumeMounts":    []any{map[string]any{"name": "seed", "mountPath": "/seed"}},
			}},
			"volumes": []any{map[string]any{"name": "seed", "persistentVolumeClaim": map[string]string{"claimName": seed.Claim}}},
		}},
	}}
	data, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if err := writeSeedFile(root, seed.Claim+".json", data); err != nil {
		return err
	}
	if _, err := k.runner.Run(ctx, k.kubectl, "create", "-f", filepath.Join(root.Name(), seed.Claim+".json")); err != nil {
		return err
	}
	if _, err := k.runner.Run(ctx, k.kubectl, "-n", namespace, "wait", "--for=condition=Ready", "pod/"+seed.Claim, "--timeout=90s"); err != nil {
		return err
	}
	info, err := root.Lstat(seed.Claim + ".sav")
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxSaveBytes {
		return errors.New("staged save must be a bounded regular file")
	}
	if _, err := k.runner.Run(ctx, k.kubectl, "-n", namespace, "exec", seed.Claim, "-c", "writer", "--", "sh", "-ec", `test ! -e /seed/.incoming; test ! -L /seed/.incoming; test ! -e "/seed/$1"; test ! -L "/seed/$1"`, "seed", seed.Path); err != nil {
		return err
	}
	if _, err := k.runner.Run(ctx, k.kubectl, "-n", namespace, "cp", "-c", "writer", filepath.Join(root.Name(), seed.Claim+".sav"), seed.Claim+":/seed/.incoming"); err != nil {
		return err
	}
	if _, err := k.runner.Run(ctx, k.kubectl, "-n", namespace, "exec", seed.Claim, "-c", "writer", "--", "sh", "-ec", `test -f /seed/.incoming; test ! -L /seed/.incoming; chmod 600 /seed/.incoming; sync; mv -T /seed/.incoming "/seed/$1"; sync`, "seed", seed.Path); err != nil {
		return err
	}
	_, err = k.runner.Run(ctx, k.kubectl, "-n", namespace, "delete", "pod", seed.Claim, "--ignore-not-found", "--wait=true", "--timeout=30s")
	return err
}

type seedVolume struct {
	PersistentVolumeClaim *struct {
		ClaimName string `json:"claimName"`
	} `json:"persistentVolumeClaim"`
}

func seedReferenced(volumes []seedVolume, claim string) bool {
	for _, volume := range volumes {
		if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == claim {
			return true
		}
	}
	return false
}

func (k *kubeOrchestrator) cleanupSeed(ctx context.Context, claim string, pending PendingSeed) error {
	var pods struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Spec struct {
				Volumes []seedVolume `json:"volumes"`
			} `json:"spec"`
		} `json:"items"`
	}
	var deployments struct {
		Items []struct {
			Spec struct {
				Template struct {
					Spec struct {
						Volumes []seedVolume `json:"volumes"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
		} `json:"items"`
	}
	for _, target := range []struct {
		resource string
		output   any
	}{{"pods", &pods}, {"deployments", &deployments}} {
		data, err := k.runner.Run(ctx, k.kubectl, "-n", pending.Namespace, "get", target.resource, "-o", "json")
		if err != nil {
			return err
		}
		if err := json.Unmarshal(data, target.output); err != nil {
			return err
		}
	}
	if pods.Items == nil || deployments.Items == nil {
		return errors.New("could not confirm seed references")
	}
	referenced := false
	for _, pod := range pods.Items {
		if pod.Metadata.Name == claim && pod.Metadata.Labels[seedLabel] == claim {
			if _, err := k.runner.Run(ctx, k.kubectl, "-n", pending.Namespace, "delete", "pod", claim, "--ignore-not-found", "--wait=true", "--timeout=30s"); err != nil {
				return err
			}
		} else if seedReferenced(pod.Spec.Volumes, claim) {
			referenced = true
		}
	}
	for _, deployment := range deployments.Items {
		referenced = referenced || seedReferenced(deployment.Spec.Template.Spec.Volumes, claim)
	}
	if referenced {
		return errors.New("seed still referenced")
	}
	data, err := k.runner.Run(ctx, k.kubectl, "-n", pending.Namespace, "get", "pvc", claim, "--ignore-not-found", "-o", "json")
	if err != nil || len(bytes.TrimSpace(data)) == 0 {
		return err
	}
	var pvc struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(data, &pvc); err != nil {
		return err
	}
	if pvc.Metadata.Labels[seedLabel] != claim {
		return errors.New("seed claim is not owned by this request")
	}
	_, err = k.runner.Run(ctx, k.kubectl, "-n", pending.Namespace, "delete", "pvc", claim, "--ignore-not-found", "--wait=true", "--timeout=30s")
	return err
}

func (a *App) cleanupSeed(claim string) {
	root, err := a.seedRoot()
	if err != nil {
		return
	}
	defer root.Close()
	for _, suffix := range []string{".sav", ".sav.tmp", ".json", ".json.tmp"} {
		if err := root.Remove(claim + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return
		}
	}
	pending, ok := a.store.Snapshot().PendingSeeds[claim]
	if !ok {
		return
	}
	k, ok := a.orchestrator.(*kubeOrchestrator)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := k.cleanupSeed(ctx, claim, pending); err != nil {
		return
	}
	_ = a.store.Update(func(state *State) error { delete(state.PendingSeeds, claim); return nil })
}

func (a *App) runSeedCleanup(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if a.createMu.TryLock() {
			snapshot := a.store.Snapshot()
			for claim := range snapshot.PendingSeeds {
				a.cleanupSeed(claim)
			}
			for _, server := range snapshot.Servers {
				if server.SaveSeed != nil {
					a.cleanupSeed(server.SaveSeed.Claim)
				}
			}
			a.createMu.Unlock()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
