package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/yaml"
)

const ownershipAnnotation = "rsdw-c2.petzko.sh/owner"

type worldDataMode string

const (
	keepWorld  worldDataMode = "keep"
	purgeWorld worldDataMode = "purge"
)

type resourceIdentity struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

type deletionPlan struct {
	Storage         resourceIdentity   `json:"storage"`
	Revision        int                `json:"revision"`
	Digest          string             `json:"digest"`
	Resources       []resourceIdentity `json:"resources"`
	World           []resourceIdentity `json:"world"`
	Secrets         []resourceIdentity `json:"secrets"`
	RetainedSecrets []resourceIdentity `json:"retainedSecrets,omitempty"`
	Seeds           []resourceIdentity `json:"seeds"`
}

type deletionRecord struct {
	ServerID   string        `json:"serverId"`
	WorldLabel string        `json:"worldLabel"`
	Namespace  string        `json:"namespace"`
	Release    string        `json:"release"`
	Mode       worldDataMode `json:"mode"`
	Plan       deletionPlan  `json:"plan"`
	Completed  bool          `json:"completed"`
	LastError  string        `json:"lastError,omitempty"`
}

func worldLabel(server Server) string {
	return defaultValue(server.WorldName, defaultValue(server.Name, server.ID))
}

func (s State) deleting(id string) bool {
	_, ok := s.Deletions[id]
	return ok
}

func (a *App) lockServer(w http.ResponseWriter, id string) (Server, bool) {
	if !a.lifecycleMu.TryLock() {
		writeError(w, http.StatusConflict, "another server lifecycle operation is in progress; try again shortly")
		return Server{}, false
	}
	if err := a.store.writable(); err != nil {
		a.lifecycleMu.Unlock()
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return Server{}, false
	}
	snapshot := a.store.Snapshot()
	server, ok := snapshot.Servers[id]
	if !ok || snapshot.deleting(id) {
		a.lifecycleMu.Unlock()
		writeError(w, http.StatusConflict, "server is absent or has a deletion receipt; refresh Maintenance")
		return Server{}, false
	}
	return server, true
}

func (a *App) handleDelete(w http.ResponseWriter, r *http.Request, id string) {
	var input struct {
		Confirm      string        `json:"confirm"`
		Mode         worldDataMode `json:"mode"`
		PurgeConfirm string        `json:"purgeConfirm"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF || input.Confirm != id {
		writeError(w, http.StatusBadRequest, "confirm must identify this server's stable ID")
		return
	}
	if input.Mode == "" {
		input.Mode = keepWorld
	}
	if input.Mode != keepWorld && input.Mode != purgeWorld || input.Mode == purgeWorld && input.PurgeConfirm != "DELETE WORLD "+id {
		writeError(w, http.StatusBadRequest, "purge requires the additional confirmation DELETE WORLD followed by the stable server ID")
		return
	}
	if !a.lifecycleMu.TryLock() {
		writeError(w, http.StatusConflict, "another server lifecycle operation is in progress; retry shortly")
		return
	}
	defer a.lifecycleMu.Unlock()
	if err := a.store.writable(); err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	snapshot := a.store.Snapshot()
	record, exists := snapshot.Deletions[id]
	if exists && record.Mode != input.Mode {
		writeError(w, http.StatusConflict, "the recorded world-data choice cannot change on retry")
		return
	}
	if exists && record.Completed {
		writeJSON(w, http.StatusOK, record)
		return
	}
	server, ok := snapshot.Servers[id]
	if !ok {
		writeError(w, http.StatusNotFound, "server not found")
		return
	}
	if !a.demo && a.store.path == "" {
		writeError(w, http.StatusServiceUnavailable, "deletion requires persistent C2 state storage")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 100*time.Second)
	defer cancel()
	k, kube := a.orchestrator.(*kubeOrchestrator)
	if !a.demo && !kube {
		writeError(w, http.StatusServiceUnavailable, "deletion requires Kubernetes access")
		return
	}
	if !exists {
		record = deletionRecord{ServerID: id, WorldLabel: worldLabel(server), Namespace: server.Namespace, Release: server.Release, Mode: input.Mode}
		if !a.demo {
			plan, err := k.inspectDeletion(ctx, server, snapshot, input.Mode)
			if err != nil {
				writeError(w, http.StatusConflict, err.Error())
				return
			}
			record.Plan = plan
		}
		if err := a.store.Update(func(state *State) error {
			if state.Deletions == nil {
				state.Deletions = map[string]deletionRecord{}
			}
			state.Deletions[id] = record
			cancelServerDeliveries(state, id)
			return nil
		}); err != nil {
			writeError(w, http.StatusInternalServerError, "could not persist deletion intent; no deletion started")
			return
		}
	}
	if !a.demo {
		if err := k.removeServer(ctx, record, a.store.Snapshot()); err != nil {
			record.LastError = err.Error()
			persistErr := a.store.Update(func(state *State) error { state.Deletions[id] = record; return nil })
			message := record.LastError
			if persistErr != nil {
				message += "; could not persist the latest error"
			}
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": message + "; deletion is pending, retry with the same choice", "deletion": record})
			return
		}
	}
	record.Completed, record.LastError = true, ""
	if err := a.store.Update(func(state *State) error {
		delete(state.Servers, id)
		delete(state.Producers, id)
		for key, integration := range state.Integrations {
			integration.ServerIDs = slices.DeleteFunc(integration.ServerIDs, func(value string) bool { return value == id })
			state.Integrations[key] = integration
		}
		cancelServerDeliveries(state, id)
		for _, seed := range record.Plan.Seeds {
			delete(state.PendingSeeds, seed.Name)
		}
		state.Deletions[id] = record
		appendEvent(state, server, "system", "success", "Server deleted", "World data choice: "+string(record.Mode))
		return nil
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "resource cleanup finished; retry deletion to persist its receipt")
		return
	}
	cache := a.observations()
	cache.mu.Lock()
	delete(cache.history, id)
	cache.mu.Unlock()
	if server.SaveSeed != nil {
		a.cleanupSeed(server.SaveSeed.Claim)
	}
	writeJSON(w, http.StatusOK, record)
}

func cancelServerDeliveries(state *State, id string) {
	for key, delivery := range state.Deliveries {
		if delivery.Event.ServerID == id && (delivery.Status == DeliveryPending || delivery.Status == DeliveryRetry || delivery.Status == DeliverySending) {
			delivery.Status, delivery.Result, delivery.UpdatedAt = DeliveryFailed, "Cancelled by server deletion; an in-flight message may already have been sent", time.Now().UTC()
			state.Deliveries[key] = delivery
		}
	}
}

type deletionResource struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   kubeMetadata      `json:"metadata"`
	Spec       json.RawMessage   `json:"spec"`
	Data       map[string][]byte `json:"data"`
	Status     struct {
		Phase string `json:"phase"`
	} `json:"status"`
}

func (r deletionResource) identity() resourceIdentity {
	return resourceIdentity{Kind: r.Kind, Namespace: r.Metadata.Namespace, Name: r.Metadata.Name, UID: r.Metadata.UID}
}

func (k *kubeOrchestrator) deletionGet(ctx context.Context, kind, namespace, name string) (deletionResource, error) {
	data, err := k.runner.Run(ctx, k.kubectl, "-n", namespace, "get", kind, name, "--ignore-not-found", "-o", "json")
	var resource deletionResource
	if err != nil {
		return resource, fmt.Errorf("cannot read %s %s/%s; check Kubernetes access", kind, namespace, name)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return resource, nil
	}
	if json.Unmarshal(data, &resource) != nil || resource.Metadata.UID == "" || resource.Kind != kind || resource.Metadata.Name != name || resource.Metadata.Namespace != namespace {
		return resource, fmt.Errorf("invalid identity for %s %s/%s", kind, namespace, name)
	}
	return resource, nil
}

func (k *kubeOrchestrator) deletionList(ctx context.Context, namespace, kinds string, extra ...string) ([]deletionResource, error) {
	args := append([]string{"-n", namespace, "get", kinds, "-o", "json"}, extra...)
	data, err := k.runner.Run(ctx, k.kubectl, args...)
	var list struct {
		Items []deletionResource `json:"items"`
	}
	if err != nil || json.Unmarshal(data, &list) != nil || list.Items == nil {
		return nil, fmt.Errorf("cannot inspect %s in %s; check Kubernetes list permissions", kinds, namespace)
	}
	return list.Items, nil
}

type storedRelease struct {
	Name      string            `json:"name"`
	Namespace string            `json:"namespace"`
	Version   int               `json:"version"`
	Manifest  string            `json:"manifest"`
	Hooks     []json.RawMessage `json:"hooks"`
}

func (k *kubeOrchestrator) deletionRelease(ctx context.Context, namespace, name string) (storedRelease, resourceIdentity, error) {
	var release storedRelease
	var storage resourceIdentity
	items, err := k.deletionList(ctx, namespace, "secrets", "-l", "owner=helm,name="+name)
	if err != nil {
		return release, storage, err
	}
	for _, item := range items {
		encoded, err := base64.StdEncoding.DecodeString(string(item.Data["release"]))
		if err != nil {
			return release, storage, errors.New("cannot decode Helm release storage; repair it before deletion")
		}
		reader, err := gzip.NewReader(bytes.NewReader(encoded))
		if err != nil {
			return release, storage, errors.New("cannot decompress Helm release storage")
		}
		data, err := io.ReadAll(io.LimitReader(reader, 16<<20))
		reader.Close()
		var candidate storedRelease
		if err != nil || json.Unmarshal(data, &candidate) != nil || candidate.Name != name || candidate.Namespace != namespace || candidate.Version < 1 || item.Metadata.UID == "" || item.Metadata.Namespace != namespace || item.Metadata.Name != "sh.helm.release.v1."+name+".v"+strconv.Itoa(candidate.Version) {
			return release, storage, errors.New("Helm release storage identity is invalid")
		}
		if candidate.Version > release.Version {
			release, storage = candidate, item.identity()
		}
	}
	return release, storage, nil
}

func releaseDigest(release storedRelease) string {
	sum := sha256.Sum256([]byte(release.Manifest))
	return hex.EncodeToString(sum[:])
}

func manifestResources(release storedRelease) ([]deletionResource, error) {
	if len(release.Hooks) != 0 {
		return nil, errors.New("release has hooks; remove or audit them outside C2 before deletion")
	}
	decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(release.Manifest), 4096)
	var resources []deletionResource
	seen := map[string]bool{}
	for {
		var r deletionResource
		err := decoder.Decode(&r)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errors.New("stored Helm manifest is unreadable")
		}
		if r.Kind == "" {
			continue
		}
		// Only the game chart's namespaced resource kinds have audited uninstall semantics.
		version := map[string]string{"Deployment": "apps/v1", "Service": "v1", "ConfigMap": "v1", "Secret": "v1", "PersistentVolumeClaim": "v1"}[r.Kind]
		if version == "" || r.APIVersion != version || r.Metadata.Name == "" || r.Metadata.Annotations["helm.sh/hook"] != "" {
			return nil, fmt.Errorf("unsupported uninstall resource %s; audit this release outside C2", r.Kind)
		}
		if r.Metadata.Namespace == "" {
			r.Metadata.Namespace = release.Namespace
		}
		key := r.Kind + "/" + r.Metadata.Name
		if r.Metadata.Namespace != release.Namespace || seen[key] {
			return nil, errors.New("release contains cross-namespace or duplicate resources")
		}
		seen[key] = true
		if r.Kind == "PersistentVolumeClaim" && r.Metadata.Annotations["helm.sh/resource-policy"] != "keep" {
			return nil, fmt.Errorf("world PVC %s lacks keep in the stored Helm manifest; an operator must repair retention before either deletion mode", r.Metadata.Name)
		}
		if r.Kind != "PersistentVolumeClaim" && r.Metadata.Annotations["helm.sh/resource-policy"] != "" {
			return nil, errors.New("release retains non-world resources; reconcile it outside C2")
		}
		resources = append(resources, r)
	}
	if len(resources) == 0 {
		return nil, errors.New("stored Helm manifest is empty")
	}
	return resources, nil
}

func helmOwned(r deletionResource, release string) bool {
	return r.Metadata.Annotations["meta.helm.sh/release-name"] == release && r.Metadata.Annotations["meta.helm.sh/release-namespace"] == r.Metadata.Namespace
}

func podSpec(r deletionResource) map[string]any {
	var spec map[string]any
	_ = json.Unmarshal(r.Spec, &spec)
	if r.Kind == "Pod" {
		return spec
	}
	if r.Kind == "CronJob" {
		job, _ := spec["jobTemplate"].(map[string]any)
		spec, _ = job["spec"].(map[string]any)
	}
	template, _ := spec["template"].(map[string]any)
	result, _ := template["spec"].(map[string]any)
	return result
}

func referencedNames(value any, key string) []string {
	var names []string
	switch v := value.(type) {
	case map[string]any:
		for field, child := range v {
			if field == key {
				object, _ := child.(map[string]any)
				for _, nameKey := range []string{"name", "claimName", "secretName"} {
					if name, ok := object[nameKey].(string); ok && name != "" {
						names = append(names, name)
					}
				}
			}
			names = append(names, referencedNames(child, key)...)
		}
	case []any:
		for _, child := range v {
			names = append(names, referencedNames(child, key)...)
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

func secretNames(spec map[string]any) []string {
	var names []string
	for _, key := range []string{"secret", "secretKeyRef", "secretRef"} {
		names = append(names, referencedNames(spec, key)...)
	}
	slices.Sort(names)
	return slices.Compact(names)
}

func (k *kubeOrchestrator) inspectDeletion(ctx context.Context, server Server, state State, mode worldDataMode) (deletionPlan, error) {
	var plan deletionPlan
	for _, other := range state.Servers {
		if other.ID != server.ID && other.Namespace == server.Namespace && other.Release == server.Release {
			return plan, errors.New("another inventory entry uses this release")
		}
	}
	release, storage, err := k.deletionRelease(ctx, server.Namespace, server.Release)
	if err != nil {
		return plan, err
	}
	resources, err := manifestResources(release)
	if err != nil {
		return plan, err
	}
	plan.Storage, plan.Revision, plan.Digest = storage, release.Version, releaseDigest(release)
	var deployment deletionResource
	for _, manifest := range resources {
		live, err := k.deletionGet(ctx, manifest.Kind, server.Namespace, manifest.Metadata.Name)
		if err != nil {
			return plan, err
		}
		if live.Metadata.UID == "" || !helmOwned(live, server.Release) || live.Metadata.DeletionTimestamp != nil {
			return plan, fmt.Errorf("cannot prove release ownership of %s %s; reconcile the release before deletion", manifest.Kind, manifest.Metadata.Name)
		}
		if manifest.Kind == "PersistentVolumeClaim" {
			plan.World = append(plan.World, live.identity())
		} else {
			plan.Resources = append(plan.Resources, live.identity())
		}
		if manifest.Kind == "Deployment" {
			if deployment.Kind != "" || manifest.Metadata.Name != deploymentName(server.Release) {
				return plan, errors.New("expected exactly the managed game deployment")
			}
			deployment = live
			if !slices.Equal(referencedNames(podSpec(live), "persistentVolumeClaim"), referencedNames(podSpec(manifest), "persistentVolumeClaim")) || !slices.Equal(secretNames(podSpec(live)), secretNames(podSpec(manifest))) {
				return plan, errors.New("live deployment storage or Secret references differ from the stored release")
			}
		}
	}
	if deployment.Kind == "" {
		return plan, errors.New("managed deployment is absent from the release")
	}
	claims := referencedNames(podSpec(deployment), "persistentVolumeClaim")
	if len(claims) == 0 {
		return plan, errors.New("cannot identify world storage from the deployment")
	}
	for _, claim := range claims {
		if server.SaveSeed != nil && claim == server.SaveSeed.Claim {
			seed, err := k.deletionGet(ctx, "PersistentVolumeClaim", server.Namespace, claim)
			if err != nil {
				return plan, err
			}
			if seed.Metadata.UID != "" {
				if seed.Metadata.Labels[seedLabel] != claim {
					return plan, errors.New("temporary seed ownership cannot be proven")
				}
				plan.Seeds = append(plan.Seeds, seed.identity())
			}
			continue
		}
		if !slices.ContainsFunc(plan.World, func(r resourceIdentity) bool { return r.Name == claim }) {
			if mode == purgeWorld {
				return plan, fmt.Errorf("PVC %s is external to this release; use keep or have its owner handle disposal", claim)
			}
			world, err := k.deletionGet(ctx, "PersistentVolumeClaim", server.Namespace, claim)
			if err != nil {
				return plan, err
			}
			if world.Metadata.UID == "" {
				return plan, fmt.Errorf("world PVC %s is missing", claim)
			}
			plan.World = append(plan.World, world.identity())
		}
	}
	if len(plan.World) == 0 {
		return plan, errors.New("no world PVC identified")
	}
	for _, ref := range plan.World {
		if !slices.Contains(claims, ref.Name) {
			return plan, errors.New("release contains an unexplained PVC")
		}
		if err := worldProtected(state, server.ID, ref); err != nil {
			return plan, err
		}
		world, err := k.deletionGet(ctx, ref.Kind, ref.Namespace, ref.Name)
		if err != nil {
			return plan, err
		}
		if err := retainedWorld(world, ref); err != nil {
			return plan, err
		}
	}
	for _, name := range []string{server.Release + "-api", server.PasswordSecret} {
		if name == "" {
			continue
		}
		secret, err := k.deletionGet(ctx, "Secret", server.Namespace, name)
		if err != nil {
			return plan, err
		}
		if secret.Metadata.UID == "" {
			continue
		}
		if slices.ContainsFunc(plan.Resources, func(r resourceIdentity) bool { return r.Kind == "Secret" && r.Name == name }) {
			continue
		}
		if !slices.Contains(secretNames(podSpec(deployment)), name) || server.OwnershipToken == "" || secret.Metadata.Annotations[ownershipAnnotation] != server.OwnershipToken {
			plan.RetainedSecrets = append(plan.RetainedSecrets, secret.identity())
			continue
		}
		plan.Secrets = append(plan.Secrets, secret.identity())
	}
	if err := k.checkDeletionReferences(ctx, server.Namespace, plan, true, state, server.ID); err != nil {
		return plan, err
	}
	return plan, nil
}

func worldProtected(state State, deleting string, ref resourceIdentity) error {
	for id, record := range state.Deletions {
		if id != deleting && slices.ContainsFunc(record.Plan.World, func(r resourceIdentity) bool { return r.Namespace == ref.Namespace && r.Name == ref.Name }) {
			return errors.New("world storage is protected by another deletion receipt")
		}
	}
	for id, other := range state.Servers {
		if id != deleting && other.Namespace == ref.Namespace && (deploymentName(other.Release) == ref.Name || other.SaveSeed != nil && other.SaveSeed.Claim == ref.Name) {
			return errors.New("world storage is referenced by another inventory entry")
		}
	}
	return nil
}

func retainedWorld(live deletionResource, ref resourceIdentity) error {
	if live.Metadata.UID != ref.UID || live.Metadata.DeletionTimestamp != nil || len(live.Metadata.OwnerReferences) != 0 || live.Status.Phase != "Bound" {
		return fmt.Errorf("world PVC %s/%s must have its recorded UID, be Bound, not terminating, and have no owner references; ask the storage operator to reconcile it", ref.Namespace, ref.Name)
	}
	return nil
}

func (k *kubeOrchestrator) checkDeletionReferences(ctx context.Context, namespace string, plan deletionPlan, before bool, state State, id string) error {
	items, err := k.deletionList(ctx, namespace, "pods,deployments,replicasets,statefulsets,daemonsets,jobs,cronjobs")
	if err != nil {
		return err
	}
	owned := map[string]bool{}
	for _, ref := range plan.Resources {
		if ref.Kind == "Deployment" {
			owned[ref.UID] = true
		}
	}
	for _, item := range items {
		if item.Kind == "ReplicaSet" {
			for _, owner := range item.Metadata.OwnerReferences {
				if owner.Kind == "Deployment" && owned[owner.UID] {
					owned[item.Metadata.UID] = true
				}
			}
		}
	}
	for _, item := range items {
		isOwned := owned[item.Metadata.UID]
		for _, owner := range item.Metadata.OwnerReferences {
			if owned[owner.UID] {
				isOwned = true
			}
		}
		if isOwned {
			if !before {
				return errors.New("owned workloads are still terminating; retry when their Pods and ReplicaSets are gone")
			}
			continue
		}
		spec := podSpec(item)
		claims, secrets := referencedNames(spec, "persistentVolumeClaim"), secretNames(spec)
		for _, ref := range append(append(append(slices.Clone(plan.World), plan.Seeds...), plan.Secrets...), plan.Resources...) {
			if ref.Kind == "PersistentVolumeClaim" && slices.Contains(claims, ref.Name) || ref.Kind == "Secret" && slices.Contains(secrets, ref.Name) {
				return fmt.Errorf("%s %s still references %s; stop or detach that consumer before retrying", item.Kind, item.Metadata.Name, ref.Name)
			}
		}
	}
	for _, ref := range append(slices.Clone(plan.World), plan.Seeds...) {
		if err := worldProtected(state, id, ref); err != nil {
			return err
		}
	}
	for _, integration := range state.Integrations {
		if namespace == envOr("RSDW_NAMESPACE", "rsdw-system") && slices.ContainsFunc(append(slices.Clone(plan.Secrets), plan.Resources...), func(r resourceIdentity) bool { return r.Kind == "Secret" && r.Name == integration.SecretRef.Name }) {
			return errors.New("a game Secret is also used by a Discord integration")
		}
	}
	return nil
}

func (k *kubeOrchestrator) removeServer(ctx context.Context, record deletionRecord, state State) error {
	plan := record.Plan
	release, storage, err := k.deletionRelease(ctx, record.Namespace, record.Release)
	if err != nil {
		return err
	}
	if storage.UID != "" {
		if storage != plan.Storage || release.Version != plan.Revision || releaseDigest(release) != plan.Digest {
			return errors.New("Helm release identity or manifest changed; refusing to uninstall a replacement")
		}
		if _, err := manifestResources(release); err != nil {
			return err
		}
	}
	for _, ref := range append(append(append(slices.Clone(plan.Resources), plan.Secrets...), plan.Seeds...), plan.World...) {
		live, err := k.deletionGet(ctx, ref.Kind, ref.Namespace, ref.Name)
		if err != nil {
			return err
		}
		if live.Metadata.UID != "" && live.Metadata.UID != ref.UID {
			return fmt.Errorf("%s %s has a replacement UID; refusing deletion", ref.Kind, ref.Name)
		}
		if slices.Contains(plan.World, ref) && (storage.UID != "" || record.Mode == keepWorld || live.Metadata.UID != "") {
			if err := retainedWorld(live, ref); err != nil {
				return err
			}
		}
		if storage.UID != "" && slices.Contains(plan.Resources, ref) && live.Metadata.UID != "" && !helmOwned(live, record.Release) {
			return errors.New("release resource ownership changed")
		}
	}
	if err := k.checkDeletionReferences(ctx, record.Namespace, plan, storage.UID != "", state, record.ServerID); err != nil {
		return err
	}
	if storage.UID != "" {
		if _, err := k.runner.Run(ctx, k.helm, "uninstall", record.Release, "--namespace", record.Namespace, "--no-hooks", "--wait", "--timeout", "70s", "--cascade=foreground"); err != nil {
			return errors.New("Helm uninstall did not confirm completion; inspect the release and retry")
		}
	}
	for _, ref := range plan.Resources {
		live, err := k.deletionGet(ctx, ref.Kind, ref.Namespace, ref.Name)
		if err != nil {
			return err
		}
		if live.Metadata.UID != "" {
			if err := k.deleteIdentity(ctx, ref); err != nil {
				return err
			}
		}
	}
	if err := k.checkDeletionReferences(ctx, record.Namespace, plan, false, state, record.ServerID); err != nil {
		return err
	}
	for _, ref := range plan.World {
		live, err := k.deletionGet(ctx, ref.Kind, ref.Namespace, ref.Name)
		if err != nil {
			return err
		}
		if record.Mode == keepWorld {
			if err := retainedWorld(live, ref); err != nil {
				return err
			}
		} else if err := k.deleteIdentity(ctx, ref); err != nil {
			return err
		}
	}
	for _, ref := range append(slices.Clone(plan.Secrets), plan.Seeds...) {
		if err := k.deleteIdentity(ctx, ref); err != nil {
			return err
		}
	}
	_, remaining, err := k.deletionRelease(ctx, record.Namespace, record.Release)
	if err != nil {
		return err
	}
	if remaining.UID != "" {
		return errors.New("release storage remains; retry after reconciling Helm")
	}
	return nil
}

func (k *kubeOrchestrator) deleteIdentity(ctx context.Context, ref resourceIdentity) error {
	live, err := k.deletionGet(ctx, ref.Kind, ref.Namespace, ref.Name)
	if err != nil || live.Metadata.UID == "" {
		return err
	}
	if live.Metadata.UID != ref.UID {
		return fmt.Errorf("%s %s has a replacement UID", ref.Kind, ref.Name)
	}
	resource := map[string]string{"Secret": "secrets", "PersistentVolumeClaim": "persistentvolumeclaims", "Deployment": "deployments", "Service": "services", "ConfigMap": "configmaps"}[ref.Kind]
	if resource == "" {
		return errors.New("unsupported explicit deletion kind")
	}
	options := map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": map[string]string{"uid": ref.UID}, "propagationPolicy": "Foreground"}
	api := "/api/v1"
	if ref.Kind == "Deployment" {
		api = "/apis/apps/v1"
	}
	if err := k.runJSONFile(ctx, options, "delete", "--raw", api+"/namespaces/"+ref.Namespace+"/"+resource+"/"+ref.Name); err != nil {
		return fmt.Errorf("could not delete recorded %s %s; retry after checking access and UID", ref.Kind, ref.Name)
	}
	live, err = k.deletionGet(ctx, ref.Kind, ref.Namespace, ref.Name)
	if err != nil {
		return err
	}
	if live.Metadata.UID != "" {
		return fmt.Errorf("%s %s is still terminating; retry when deletion finishes", ref.Kind, ref.Name)
	}
	return nil
}

func (k *kubeOrchestrator) runJSONFile(ctx context.Context, value any, args ...string) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp("", "rsdw-resource-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	_, err = k.runner.Run(ctx, k.kubectl, append(args, "-f", file.Name())...)
	if err != nil {
		return errors.New("Kubernetes resource command failed; inspect access and resource ownership")
	}
	return nil
}

func (k *kubeOrchestrator) ensureOwnedSecret(ctx context.Context, server Server, name string, values map[string]string) error {
	live, err := k.deletionGet(ctx, "Secret", server.Namespace, name)
	if err != nil {
		return err
	}
	if live.Metadata.UID != "" {
		if live.Metadata.Annotations[ownershipAnnotation] != server.OwnershipToken {
			return errors.New("Secret already exists without this server's ownership marker")
		}
		return nil
	}
	return k.runJSONFile(ctx, map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": name, "namespace": server.Namespace, "annotations": map[string]string{ownershipAnnotation: server.OwnershipToken}}, "stringData": values}, "create")
}

func (a *App) newServerID(ctx context.Context, request CreateServerRequest) (string, error) {
	base := slugify(defaultValue(request.WorldName, request.Name))
	if base == "" {
		base = "world"
	}
	if len(base) > 28 {
		base = strings.TrimRight(base[:28], "-")
	}
	snapshot := a.store.Snapshot()
	for range 5 {
		id := base + "-" + randomID()
		if snapshot.Servers[id].ID != "" || snapshot.deleting(id) {
			continue
		}
		reserved := false
		for _, pending := range snapshot.PendingSeeds {
			reserved = reserved || pending.Release == id
		}
		if reserved {
			continue
		}
		if k, ok := a.orchestrator.(*kubeOrchestrator); ok && !a.demo {
			items, err := k.deletionList(ctx, defaultValue(request.Namespace, "dragonwilds"), "secrets,deployments,services,configmaps,persistentvolumeclaims")
			if err != nil {
				return "", err
			}
			for _, item := range items {
				reserved = reserved || item.Metadata.Name == deploymentName(id) || item.Metadata.Name == id+"-api" || item.Metadata.Name == id+"-settings" || strings.HasPrefix(item.Metadata.Name, "sh.helm.release.v1."+id+".") || item.Metadata.Annotations["meta.helm.sh/release-name"] == id
			}
		}
		if !reserved {
			return id, nil
		}
	}
	return "", errors.New("could not allocate an unused server identity; retry creation")
}
