package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
)

// Commands model one Helm release and an unrelated neighboring release.
type deletionRunner struct {
	t              *testing.T
	objects        map[string]deletionResource
	storage        deletionResource
	calls, effects []string
	failDelete     string
	storePath      string
}

func deletionFixture(t *testing.T, storedKeep, owned, withSeed bool) (*App, *deletionRunner) {
	t.Helper()
	r := &deletionRunner{t: t, objects: map[string]deletionResource{}}
	for _, entry := range []struct{ kind, name string }{
		{"Deployment", "world-rsdragonwilds"}, {"PersistentVolumeClaim", "world-rsdragonwilds"}, {"ConfigMap", "world-config"},
		{"Secret", "world-api"}, {"Secret", "world-settings"}, {"Secret", "neighbor-api"},
		{"Deployment", "neighbor-rsdragonwilds"}, {"PersistentVolumeClaim", "neighbor-rsdragonwilds"},
	} {
		obj := deletionResource{APIVersion: "v1", Kind: entry.kind}
		obj.Metadata.Name, obj.Metadata.Namespace, obj.Metadata.UID = entry.name, "games", "uid-"+entry.name
		obj.Metadata.Annotations = map[string]string{"meta.helm.sh/release-name": "world", "meta.helm.sh/release-namespace": "games"}
		if strings.HasPrefix(entry.name, "neighbor") {
			obj.Metadata.Annotations["meta.helm.sh/release-name"] = "neighbor"
		}
		if entry.kind == "Deployment" {
			obj.APIVersion = "apps/v1"
		}
		if entry.kind == "PersistentVolumeClaim" {
			obj.Metadata.Annotations["helm.sh/resource-policy"] = "keep"
			obj.Status.Phase = "Bound"
		}
		if owned && entry.kind == "Secret" && !strings.HasPrefix(entry.name, "neighbor") {
			obj.Metadata.Annotations[ownershipAnnotation] = "owner-world"
		}
		if entry.kind == "ConfigMap" {
			obj.Data = map[string]string{"server.ini": "[Server]\nName=Living World\n"}
		}
		r.objects[entry.kind+"/"+entry.name] = obj
	}
	deployment := r.objects["Deployment/world-rsdragonwilds"]
	deployment.Spec = json.RawMessage(`{"template":{"spec":{"volumes":[{"persistentVolumeClaim":{"claimName":"world-rsdragonwilds"}}],"containers":[{"envFrom":[{"secretRef":{"name":"world-api"}},{"secretRef":{"name":"world-settings"}}]}]}}}`)
	if withSeed {
		deployment.Spec = json.RawMessage(strings.Replace(string(deployment.Spec), `"volumes":[`, `"volumes":[{"persistentVolumeClaim":{"claimName":"uploaded-seed"}},`, 1))
		seed := deletionResource{APIVersion: "v1", Kind: "PersistentVolumeClaim", Metadata: kubeMetadata{Name: "uploaded-seed", Namespace: "games", UID: "seed-uid", Labels: map[string]string{seedLabel: "uploaded-seed"}}}
		seed.Status.Phase = "Bound"
		r.objects["PersistentVolumeClaim/uploaded-seed"] = seed
	}
	r.objects["Deployment/world-rsdragonwilds"] = deployment
	pvc := r.objects["PersistentVolumeClaim/world-rsdragonwilds"]
	pvc.Metadata.Annotations = map[string]string{}
	if storedKeep {
		pvc.Metadata.Annotations["helm.sh/resource-policy"] = "keep"
	}
	var documents []string
	for _, obj := range []deletionResource{deployment, pvc, r.objects["ConfigMap/world-config"]} {
		data, err := json.Marshal(obj)
		if err != nil {
			t.Fatal(err)
		}
		documents = append(documents, string(data))
	}
	data, err := json.Marshal(storedRelease{Name: "world", Namespace: "games", Version: 1, Manifest: strings.Join(documents, "\n---\n")})
	if err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	if _, err := gz.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	r.storage = deletionResource{Kind: "Secret", Data: map[string]string{"release": base64.StdEncoding.EncodeToString([]byte(base64.StdEncoding.EncodeToString(compressed.Bytes())))}}
	r.storage.Metadata.Name, r.storage.Metadata.Namespace, r.storage.Metadata.UID = "sh.helm.release.v1.world.v1", "games", "helm-uid"
	app := newTestApp(t, false)
	r.storePath = app.store.path
	app.orchestrator = &kubeOrchestrator{runner: r, kubectl: "kubectl", helm: "helm"}
	if err := app.store.Update(func(s *State) error {
		s.Servers["world"] = Server{ID: "world", Name: "Old display", ServerSettings: ServerSettings{WorldName: "Living World"}, Namespace: "games", Release: "world", PasswordSecret: "world-settings", OwnershipToken: "owner-world"}
		s.Servers["neighbor"] = Server{ID: "neighbor", Namespace: "games", Release: "neighbor"}
		if withSeed {
			server := s.Servers["world"]
			server.SaveSeed = &SaveSeed{Claim: "uploaded-seed", Path: "OnlySave.sav"}
			s.Servers["world"] = server
			s.PendingSeeds = map[string]PendingSeed{"uploaded-seed": {Namespace: "games", Release: "world"}}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return app, r
}

func (r *deletionRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := name + " " + strings.Join(args, " ")
	r.calls = append(r.calls, call)
	if name == "helm" || len(args) > 0 && args[0] == "delete" {
		data, err := os.ReadFile(r.storePath)
		if err != nil {
			r.t.Fatal(err)
		}
		var state State
		if err := json.Unmarshal(data, &state); err != nil {
			r.t.Fatal(err)
		}
		record, exists := state.Deletions["world"]
		if !exists || record.Completed || record.Plan.Storage.UID != "helm-uid" {
			r.t.Fatal("external mutation preceded durable deletion intent")
		}
	}
	if name == "helm" && reflect.DeepEqual(args, []string{"uninstall", "world", "--namespace", "games", "--no-hooks", "--wait", "--timeout", "70s", "--cascade=foreground"}) {
		r.effects = append(r.effects, "uninstall world")
		r.storage = deletionResource{}
		delete(r.objects, "Deployment/world-rsdragonwilds")
		// Leave a ConfigMap behind to exercise UID-preconditioned cleanup.
		return nil, nil
	}
	if name == "kubectl" && len(args) >= 6 && args[0] == "-n" && args[1] == "games" && args[2] == "get" {
		if reflect.DeepEqual(args[3:], []string{"secrets", "-o", "json", "-l", "owner=helm,name=world"}) {
			items := []deletionResource{}
			if r.storage.Kind != "" {
				items = append(items, r.storage)
			}
			return json.Marshal(map[string]any{"items": items})
		}
		if reflect.DeepEqual(args[3:], []string{"pods,deployments,replicasets,statefulsets,daemonsets,jobs,cronjobs", "-o", "json"}) {
			items := []deletionResource{}
			for _, obj := range r.objects {
				if obj.Kind == "Deployment" {
					items = append(items, obj)
				}
			}
			return json.Marshal(map[string]any{"items": items})
		}
		if len(args) == 8 && reflect.DeepEqual(args[5:], []string{"--ignore-not-found", "-o", "json"}) {
			if obj, ok := r.objects[args[3]+"/"+args[4]]; ok {
				return json.Marshal(obj)
			}
			return nil, nil
		}
	}
	if name == "kubectl" && len(args) == 5 && args[0] == "delete" && args[1] == "--raw" && args[3] == "-f" {
		data, err := os.ReadFile(args[4])
		if err != nil {
			r.t.Fatal(err)
		}
		var options struct {
			APIVersion, Kind, PropagationPolicy string
			Preconditions                       struct{ UID string }
		}
		if err := json.Unmarshal(data, &options); err != nil {
			r.t.Fatal(err)
		}
		if options.APIVersion != "v1" || options.Kind != "DeleteOptions" || options.PropagationPolicy != "Foreground" {
			r.t.Fatalf("unsafe delete options: %s", data)
		}
		for key, obj := range r.objects {
			plural := map[string]string{"Secret": "secrets", "PersistentVolumeClaim": "persistentvolumeclaims", "ConfigMap": "configmaps"}[obj.Kind]
			if plural != "" && args[2] == "/api/v1/namespaces/games/"+plural+"/"+obj.Metadata.Name {
				if options.Preconditions.UID != obj.Metadata.UID {
					r.t.Fatalf("delete lacks matching UID: %s", data)
				}
				r.effects = append(r.effects, "delete "+key)
				if key == r.failDelete {
					r.failDelete = ""
					return nil, errors.New("scripted access failure")
				}
				delete(r.objects, key)
				return nil, nil
			}
		}
	}
	r.t.Errorf("unexpected command: %s", call)
	return nil, errors.New("unexpected command")
}

func deleteRequest(t *testing.T, app *App, body string, status int) deletionRecord {
	t.Helper()
	w := requestJSON(t, app, http.MethodDelete, "/api/servers/world", body)
	if w.Code != status {
		t.Fatalf("delete status %d, want %d: %s", w.Code, status, w.Body.String())
	}
	var record deletionRecord
	if status == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &record); err != nil {
			t.Fatal(err)
		}
	}
	return record
}

func TestDeletionConfirmationAndStoredRetention(t *testing.T) {
	app, runner := deletionFixture(t, true, true, false)
	for _, body := range []string{`{}`, `{"confirm":"Living World"}`, `{"confirm":"world","mode":"purge"}`, `{"confirm":"world","mode":"purge","purgeConfirm":"DELETE WORLD neighbor"}`, `{"confirm":"world","mode":"other"}`, `{"confirm":"world","extra":true}`, `{"confirm":"world"} {}`} {
		deleteRequest(t, app, body, 400)
	}
	if len(runner.calls) != 0 || len(app.store.Snapshot().Deletions) != 0 {
		t.Fatal("invalid confirmation had effects")
	}
	for _, mode := range []string{"keep", "purge"} {
		t.Run(mode, func(t *testing.T) {
			app, runner := deletionFixture(t, false, true, false)
			deleteRequest(t, app, `{"confirm":"world","mode":"`+mode+`","purgeConfirm":"DELETE WORLD world"}`, 409)
			if len(runner.effects) != 0 || len(app.store.Snapshot().Deletions) != 0 {
				t.Fatal("live keep bypassed missing stored keep")
			}
		})
	}
}

func TestDeletionKeepReceiptReloadAndLegacySecrets(t *testing.T) {
	app, runner := deletionFixture(t, true, false, false)
	if err := app.store.Update(func(s *State) error {
		s.Producers["world"] = AlertProducer{Runtime: "deleted-runtime"}
		s.Integrations["discord"] = DiscordIntegration{ID: "discord", ServerIDs: []string{"world", "neighbor"}}
		for _, status := range []DeliveryStatus{DeliveryPending, DeliveryRetry, DeliverySending, DeliverySent} {
			s.Deliveries[string(status)] = Delivery{ID: string(status), Event: Event{ServerID: "world"}, Status: status}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := app.store.Snapshot().Servers["neighbor"]
	objects := make(map[string]deletionResource)
	for key, obj := range runner.objects {
		if key != "Deployment/world-rsdragonwilds" && key != "ConfigMap/world-config" {
			objects[key] = obj
		}
	}
	record := deleteRequest(t, app, `{"confirm":"world"}`, 200)
	if record.Mode != keepWorld || !record.Completed || record.WorldLabel != "Living World" || len(record.Plan.RetainedSecrets) != 2 || len(record.Plan.Secrets) != 0 {
		t.Fatalf("wrong keep receipt: %+v", record)
	}
	if !reflect.DeepEqual(runner.objects, objects) || !reflect.DeepEqual(runner.effects, []string{"uninstall world", "delete ConfigMap/world-config"}) {
		t.Fatalf("keep changed retained resources: %v", runner.effects)
	}
	reloaded, err := NewStore(app.store.path, false)
	if err != nil {
		t.Fatal(err)
	}
	app.store = reloaded
	if !reflect.DeepEqual(reloaded.Snapshot().Servers["neighbor"], before) || reloaded.Snapshot().Servers["world"].ID != "" {
		t.Fatal("wrong inventory after reload")
	}
	state := reloaded.Snapshot()
	if _, exists := state.Producers["world"]; exists {
		t.Fatal("deleted producer remains")
	}
	if !reflect.DeepEqual(state.Integrations["discord"].ServerIDs, []string{"neighbor"}) {
		t.Fatal("integration targets not cleaned")
	}
	for _, id := range []string{"pending", "retry", "sending"} {
		if state.Deliveries[id].Status != DeliveryFailed {
			t.Fatalf("delivery %s not cancelled: %+v", id, state.Deliveries[id])
		}
	}
	if state.Deliveries["sent"].Status != DeliverySent {
		t.Fatal("sent delivery changed")
	}
	w := requestJSON(t, app, "GET", "/api/servers/world/deletion", "")
	var got deletionRecord
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || !reflect.DeepEqual(got, record) {
		t.Fatalf("receipt did not survive reload: %s", w.Body.String())
	}
	calls := len(runner.calls)
	deleteRequest(t, app, `{"confirm":"world"}`, 200)
	deleteRequest(t, app, `{"confirm":"world","mode":"purge","purgeConfirm":"DELETE WORLD world"}`, 409)
	if len(runner.calls) != calls {
		t.Fatal("completed retry or conflict touched Kubernetes")
	}
	for _, action := range []string{"restart", "update", "check-update"} {
		w := requestJSON(t, app, "POST", "/api/servers/world/actions/"+action, `{}`)
		if w.Code != 404 {
			t.Fatalf("deleted %s status %d", action, w.Code)
		}
	}
	if len(runner.calls) != calls {
		t.Fatal("deleted lifecycle touched Kubernetes")
	}
}

func TestDeletionPurgePartialFailureRetryAndReplacement(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(map[bool]string{false: "retry", true: "replacement"}[replacement], func(t *testing.T) {
			app, runner := deletionFixture(t, true, true, false)
			runner.failDelete = "Secret/world-api"
			body := `{"confirm":"world","mode":"purge","purgeConfirm":"DELETE WORLD world"}`
			deleteRequest(t, app, body, 502)
			reloaded, err := NewStore(app.store.path, false)
			if err != nil {
				t.Fatal(err)
			}
			app.store = reloaded
			record := reloaded.Snapshot().Deletions["world"]
			if record.Completed || record.LastError == "" || record.Mode != purgeWorld || reloaded.Snapshot().Servers["world"].ID == "" {
				t.Fatalf("pending receipt lost: %+v", record)
			}
			calls := len(runner.calls)
			deleteRequest(t, app, `{"confirm":"world"}`, 409)
			for _, action := range []string{"restart", "update", "check-update"} {
				w := requestJSON(t, app, "POST", "/api/servers/world/actions/"+action, `{}`)
				if w.Code != 409 {
					t.Fatalf("pending %s: %d %s", action, w.Code, w.Body.String())
				}
			}
			if len(runner.calls) != calls {
				t.Fatal("conflict/lifecycle touched Kubernetes")
			}
			if replacement {
				secret := runner.objects["Secret/world-api"]
				secret.Metadata.UID = "replacement-uid"
				runner.objects["Secret/world-api"] = secret
				effects := len(runner.effects)
				deleteRequest(t, app, body, 502)
				if len(runner.effects) != effects || app.store.Snapshot().Deletions["world"].Completed {
					t.Fatal("replacement was deleted")
				}
				return
			}
			deleteRequest(t, app, body, 200)
			if !reflect.DeepEqual(runner.effects, []string{"uninstall world", "delete ConfigMap/world-config", "delete PersistentVolumeClaim/world-rsdragonwilds", "delete Secret/world-api", "delete Secret/world-api", "delete Secret/world-settings"}) {
				t.Fatalf("wrong cleanup: %v", runner.effects)
			}
			if len(runner.objects) != 3 || runner.objects["Secret/neighbor-api"].Metadata.UID != "uid-neighbor-api" || runner.objects["Deployment/neighbor-rsdragonwilds"].Metadata.UID != "uid-neighbor-rsdragonwilds" || runner.objects["PersistentVolumeClaim/neighbor-rsdragonwilds"].Metadata.UID != "uid-neighbor-rsdragonwilds" {
				t.Fatal("neighbor resources changed")
			}
		})
	}
}

func TestDeletionAuthorizationAndCSRF(t *testing.T) {
	app, runner := deletionFixture(t, true, true, false)
	authApp, issuer := oidcTestApp(t)
	app.auth = authApp.auth
	viewer, viewerCSRF := loginAs(t, app, issuer, "viewer")
	admin, csrf := loginAs(t, app, issuer, "admin")
	for _, tc := range []struct {
		cookies      []*http.Cookie
		origin, csrf string
		status       int
	}{
		{nil, "https://console.example", "", 401},
		{viewer, "https://console.example", viewerCSRF, 403},
		{admin, "https://console.example", "", 403},
		{admin, "https://foreign.example", csrf, 403},
	} {
		w := authRequest(app, "DELETE", "/api/servers/world", tc.cookies, tc.origin, tc.csrf, `{"confirm":"world"}`)
		if w.Code != tc.status {
			t.Fatalf("auth status %d, want %d: %s", w.Code, tc.status, w.Body.String())
		}
	}
	if len(runner.calls) != 0 || len(app.store.Snapshot().Deletions) != 0 {
		t.Fatal("unauthorized deletion had effects")
	}
	w := authRequest(app, "DELETE", "/api/servers/world", admin, "https://console.example", csrf, `{"confirm":"world"}`)
	if w.Code != 200 {
		t.Fatalf("admin deletion: %d %s", w.Code, w.Body.String())
	}
	w = authRequest(app, "GET", "/api/servers/world/deletion", viewer, "", "", "")
	if w.Code != 403 {
		t.Fatalf("viewer can read receipt: %d", w.Code)
	}
	w = authRequest(app, "GET", "/api/bootstrap", viewer, "", "", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), `"deletions"`) {
		t.Fatalf("viewer receipt projection: %s", w.Body.String())
	}
}

func TestDeletionPoisonedStoreHasNoExternalEffects(t *testing.T) {
	app, runner := deletionFixture(t, true, true, false)
	app.store.writeErr = errors.New("state replacement durability is uncertain")
	before := app.store.Snapshot()
	deleteRequest(t, app, `{"confirm":"world"}`, 503)
	for _, action := range []string{"restart", "update", "check-update"} {
		w := requestJSON(t, app, "POST", "/api/servers/world/actions/"+action, `{}`)
		if w.Code != 503 {
			t.Fatalf("poisoned %s: %d", action, w.Code)
		}
	}
	if len(runner.calls) != 0 || !reflect.DeepEqual(before, app.store.Snapshot()) {
		t.Fatal("poisoned store allowed effects")
	}
}

func TestDeletionIntentWriteFailureHasNoExternalEffects(t *testing.T) {
	app, runner := deletionFixture(t, true, true, false)
	app.store.path = t.TempDir() // Atomic replacement of this directory must fail.
	before := app.store.Snapshot()
	deleteRequest(t, app, `{"confirm":"world"}`, 500)
	if len(runner.effects) != 0 || !reflect.DeepEqual(before, app.store.Snapshot()) {
		t.Fatal("failed intent persistence allowed deletion")
	}
}

func TestDeletionRetainsForeignDirectSecret(t *testing.T) {
	app, runner := deletionFixture(t, true, true, false)
	foreign := runner.objects["Secret/world-settings"]
	foreign.Metadata.Annotations[ownershipAnnotation] = "another-owner"
	runner.objects["Secret/world-settings"] = foreign
	record := deleteRequest(t, app, `{"confirm":"world"}`, 200)
	if !reflect.DeepEqual(record.Plan.RetainedSecrets, []resourceIdentity{{Kind: "Secret", Namespace: "games", Name: "world-settings", UID: "uid-world-settings"}}) {
		t.Fatalf("foreign Secret not recorded: %+v", record.Plan.RetainedSecrets)
	}
	if !reflect.DeepEqual(runner.objects["Secret/world-settings"], foreign) {
		t.Fatal("foreign Secret changed")
	}
	if !reflect.DeepEqual(runner.effects, []string{"uninstall world", "delete ConfigMap/world-config", "delete Secret/world-api"}) {
		t.Fatalf("wrong owned Secret cleanup: %v", runner.effects)
	}
}

func TestDeletionDiscardsInFlightTelemetry(t *testing.T) {
	app, runner, server := fixtureApp(t)
	blocking := &blockingTelemetryRunner{base: runner, started: make(chan struct{}), release: make(chan struct{})}
	app.orchestrator.(*kubeOrchestrator).runner = blocking
	done := make(chan struct{})
	go func() { app.collectTelemetry(context.Background()); close(done) }()
	<-blocking.started
	// A separate handler shares the real store and cache while collection is paused.
	deleter := &App{store: app.store, demo: true, auth: &Auth{demo: true}, telemetry: app.observations()}
	deleter.telemetryOnce.Do(func() {})
	w := requestJSON(t, deleter, "DELETE", "/api/servers/"+server.ID, `{"confirm":"`+server.ID+`"}`)
	close(blocking.release)
	<-done
	if w.Code != 200 {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	if _, ok := app.store.Snapshot().Servers[server.ID]; ok {
		t.Fatal("collector resurrected inventory")
	}
	if len(app.observations().history[server.ID]) != 0 {
		t.Fatal("collector resurrected telemetry")
	}
}

func TestDeletionRetainsUnimportedSeedUnlessPurged(t *testing.T) {
	for _, mode := range []string{"keep", "purge"} {
		t.Run(mode, func(t *testing.T) {
			app, runner := deletionFixture(t, true, true, true)
			root, err := app.seedRoot()
			if err != nil {
				t.Fatal(err)
			}
			if err := writeSeedFile(root, "uploaded-seed.sav", []byte("only saved world")); err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			record := deleteRequest(t, app, `{"confirm":"world","mode":"`+mode+`","purgeConfirm":"DELETE WORLD world"}`, 200)
			if !reflect.DeepEqual(record.Plan.Seeds, []resourceIdentity{{Kind: "PersistentVolumeClaim", Namespace: "games", Name: "uploaded-seed", UID: "seed-uid"}}) {
				t.Fatalf("seed identity missing from receipt: %+v", record.Plan.Seeds)
			}
			_, exists := runner.objects["PersistentVolumeClaim/uploaded-seed"]
			if exists != (mode == "keep") {
				t.Fatalf("%s seed retained=%v", mode, exists)
			}
			reloaded, err := NewStore(app.store.path, false)
			if err != nil {
				t.Fatal(err)
			}
			app.store = reloaded
			calls := len(runner.calls)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			// The cleanup loop performs its first pass before observing cancellation.
			app.runSeedCleanup(ctx)
			app.cleanupSeed("uploaded-seed")
			if len(runner.calls) != calls {
				t.Fatal("cleanup touched receipt-protected seed")
			}
			if mode == "keep" {
				data, err := root.ReadFile("uploaded-seed.sav")
				if err != nil || string(data) != "only saved world" {
					t.Fatalf("retained staged save: %q %v", data, err)
				}
				if err := worldProtected(reloaded.Snapshot(), "neighbor", resourceIdentity{Namespace: "games", Name: "uploaded-seed"}); err == nil {
					t.Fatal("another deletion can claim the retained seed")
				}
			} else if _, err := root.Stat("uploaded-seed.sav"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("purged local staging remains: %v", err)
			}
		})
	}
}

func TestDeletionRefusesSharedConfigMap(t *testing.T) {
	for name, spec := range map[string]string{
		"envFrom":   `{"containers":[{"envFrom":[{"configMapRef":{"name":"world-config"}}]}]}`,
		"env":       `{"containers":[{"env":[{"valueFrom":{"configMapKeyRef":{"name":"world-config","key":"server.ini"}}}]}]}`,
		"volume":    `{"volumes":[{"configMap":{"name":"world-config"}}]}`,
		"projected": `{"volumes":[{"projected":{"sources":[{"configMap":{"name":"world-config"}}]}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			app, runner := deletionFixture(t, true, true, false)
			neighbor := runner.objects["Deployment/neighbor-rsdragonwilds"]
			neighbor.Spec = json.RawMessage(`{"template":{"spec":` + spec + `}}`)
			runner.objects["Deployment/neighbor-rsdragonwilds"] = neighbor
			deleteRequest(t, app, `{"confirm":"world"}`, 409)
			if len(runner.effects) != 0 || len(app.store.Snapshot().Deletions) != 0 {
				t.Fatal("shared ConfigMap was deleted")
			}
		})
	}
}

type deletionParserRunner struct {
	response string
	calls    []string
}

func (r *deletionParserRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	return []byte(r.response), nil
}

func TestDeletionConfigMapPlainData(t *testing.T) {
	const live = `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"world-config","namespace":"games","uid":"config-uid"},"data":{"server.ini":"[Server]\nName=Living World\n","enabled":"true"}}`
	runner := &deletionParserRunner{response: live}
	k := &kubeOrchestrator{runner: runner, kubectl: "kubectl"}
	got, err := k.deletionGet(context.Background(), "ConfigMap", "games", "world-config")
	if err != nil || got.Data["server.ini"] != "[Server]\nName=Living World\n" {
		t.Fatalf("plain ConfigMap get: %+v, %v", got, err)
	}
	runner.response = `{"items":[` + live + `,{"apiVersion":"v1","kind":"Secret","metadata":{"name":"world-api","namespace":"games","uid":"secret-uid"},"data":{"token":"c2VjcmV0"}}]}`
	items, err := k.deletionList(context.Background(), "games", "secrets,deployments,services,configmaps,persistentvolumeclaims")
	if err != nil || len(items) != 2 || items[0].Data["server.ini"] != "[Server]\nName=Living World\n" || items[1].Data["token"] != "c2VjcmV0" {
		t.Fatalf("mixed resource list: %+v, %v", items, err)
	}
	const manifest = `apiVersion: v1
kind: ConfigMap
metadata:
  name: world-config
data:
  server.ini: |
    [Server]
    Name=Living World
  enabled: "true"
`
	resources, err := manifestResources(storedRelease{Namespace: "games", Manifest: manifest})
	if err != nil || len(resources) != 1 || resources[0].Data["server.ini"] != "[Server]\nName=Living World\n" {
		t.Fatalf("stored YAML ConfigMap: %+v, %v", resources, err)
	}
	if len(runner.calls) != 2 || runner.calls[0] != "kubectl -n games get ConfigMap world-config --ignore-not-found -o json" || runner.calls[1] != "kubectl -n games get secrets,deployments,services,configmaps,persistentvolumeclaims -o json" {
		t.Fatalf("unexpected commands: %v", runner.calls)
	}
}

func TestDeletionHelmSecretEncoding(t *testing.T) {
	release := storedRelease{Name: "world", Namespace: "games", Version: 1, Manifest: "plain manifest"}
	data, err := json.Marshal(release)
	if err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	inner := base64.StdEncoding.EncodeToString(compressed.Bytes())
	outer := base64.StdEncoding.EncodeToString([]byte(inner))
	runner := &deletionParserRunner{response: fmt.Sprintf(`{"items":[{"kind":"Secret","metadata":{"name":"sh.helm.release.v1.world.v1","namespace":"games","uid":"helm-uid"},"data":{"release":%q}}]}`, outer)}
	k := &kubeOrchestrator{runner: runner, kubectl: "kubectl"}
	got, storage, err := k.deletionRelease(context.Background(), "games", "world")
	if err != nil || got.Name != "world" || got.Namespace != "games" || got.Version != 1 || got.Manifest != "plain manifest" || storage.UID != "helm-uid" {
		t.Fatalf("Helm double base64/gzip decoding: %+v %+v %v", got, storage, err)
	}
}
