package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const saveSettings = `{"name":"Imported world","namespace":"other-worlds","ownerId":"0123456789abcdef0123456789abcdef","maxPlayers":4}`

var saveFixture = []byte("GVAS\x00\xffprivate-world-fixture\x00\x01")

type uploadPart struct {
	field, filename string
	data            []byte
}

func saveRequest(t *testing.T, parts ...uploadPart) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, part := range parts {
		header := textproto.MIMEHeader{}
		disposition := fmt.Sprintf(`form-data; name=%q`, part.field)
		if part.filename != "" {
			disposition += fmt.Sprintf(`; filename=%q`, part.filename)
		}
		header.Set("Content-Disposition", disposition)
		output, err := writer.CreatePart(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := output.Write(part.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/servers", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer test-admin")
	return req
}

func validSaveRequest(t *testing.T) *http.Request {
	return saveRequest(t, uploadPart{"request", "", []byte(saveSettings)}, uploadPart{"save", "World 123.sav", saveFixture})
}

type saveRunner struct {
	t                                      *testing.T
	calls                                  []string
	claim                                  string
	pvc, writer, referenced, podReferenced bool
	manifest                               map[string]any
	copied                                 []byte
	fail                                   string
	onHelm                                 func()
}

func (r *saveRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := strings.Join(append([]string{name}, args...), " ")
	r.calls = append(r.calls, call)
	if r.fail != "" && strings.Contains(call, r.fail) {
		return nil, errors.New("private-world-fixture: simulated failure")
	}
	switch {
	case strings.Contains(call, "get secrets,deployments"):
		return []byte(`{"items":[]}`), nil
	case strings.Contains(call, "get namespace") && strings.Contains(call, "--ignore-not-found"):
		return nil, nil
	case strings.Contains(call, "get Secret"):
		return nil, nil
	case len(args) > 2 && args[0] == "create" && args[1] == "-f":
		data, err := os.ReadFile(args[2])
		if err != nil {
			return nil, err
		}
		var manifest map[string]any
		if err := json.Unmarshal(data, &manifest); err != nil {
			return nil, err
		}
		if manifest["kind"] == "Secret" {
			return []byte("created"), nil
		}
		r.manifest = manifest
		items := r.manifest["items"].([]any)
		r.claim = items[0].(map[string]any)["metadata"].(map[string]any)["name"].(string)
		r.pvc, r.writer = true, true
	case strings.Contains(call, " cp -c writer "):
		data, err := os.ReadFile(args[5])
		if err != nil {
			return nil, err
		}
		r.copied = data
	case strings.Contains(call, " get pods -o json"):
		items := []any{}
		if r.writer {
			items = append(items, map[string]any{"metadata": map[string]any{"name": r.claim, "labels": map[string]string{seedLabel: r.claim}}})
		}
		if r.podReferenced {
			items = append(items, map[string]any{"metadata": map[string]string{"name": "old-replicaset-pod"}, "spec": seedTestSpec(r.claim)})
		}
		return json.Marshal(map[string]any{"items": items})
	case strings.Contains(call, " get deployments -o json"):
		items := []any{}
		if r.referenced {
			items = append(items, map[string]any{"spec": map[string]any{"template": map[string]any{"spec": seedTestSpec(r.claim)}}})
		}
		return json.Marshal(map[string]any{"items": items})
	case strings.Contains(call, " get pvc "):
		if !r.pvc {
			return nil, nil
		}
		return json.Marshal(map[string]any{"metadata": map[string]any{"labels": map[string]string{seedLabel: r.claim}}})
	case strings.Contains(call, " delete pvc "):
		if r.referenced || r.podReferenced {
			r.t.Fatal("deleted a referenced seed")
		}
		r.pvc = false
	case strings.Contains(call, " delete pod "):
		r.writer = false
	case name == "helm":
		if r.onHelm != nil {
			r.onHelm()
		}
	}
	return []byte("ok"), nil
}

func seedTestSpec(claim string) map[string]any {
	return map[string]any{"volumes": []any{map[string]any{"persistentVolumeClaim": map[string]string{"claimName": claim}}}}
}

func saveApp(t *testing.T) (*App, *saveRunner) {
	t.Helper()
	app := newTestApp(t, false)
	app.auth = &Auth{token: "test-admin"}
	runner := &saveRunner{t: t}
	app.orchestrator = &kubeOrchestrator{runner: runner, helm: "helm", kubectl: "kubectl", chart: "chart", imageRepository: "fixture/server"}
	return app, runner
}

func assertNoLocalSeeds(t *testing.T, app *App) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(filepath.Dir(app.store.path), "save-staging"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("local staged files remain: %v", entries)
	}
}

func TestSaveUploadPersistsReferenceAndSurvivesRestart(t *testing.T) {
	app, runner := saveApp(t)
	res := httptest.NewRecorder()
	app.ServeHTTP(res, validSaveRequest(t))
	if res.Code != 201 {
		t.Fatalf("create = %d %s", res.Code, res.Body.String())
	}
	if !bytes.Equal(runner.copied, saveFixture) {
		t.Fatalf("copied save = %q", runner.copied)
	}
	server := serverNamed(t, app.store.Snapshot(), "Imported world")
	if server.SaveSeed == nil || server.SaveSeed.Claim != runner.claim || server.SaveSeed.Path != "World 123.sav" || server.Namespace != "other-worlds" {
		t.Fatalf("server = %+v", server)
	}
	if !runner.pvc || runner.writer || len(app.store.Snapshot().PendingSeeds) != 0 {
		t.Fatal("incorrect seed ownership after success")
	}
	assertNoLocalSeeds(t, app)
	data, err := os.ReadFile(app.store.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{string(data), res.Body.String(), strings.Join(runner.calls, "\n")} {
		if strings.Contains(output, "private-world-fixture") {
			t.Fatal("save content leaked")
		}
	}
	reloaded, err := NewStore(app.store.path, false)
	if err != nil {
		t.Fatal(err)
	}
	app.store = reloaded
	root, err := app.seedRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile(server.SaveSeed.Claim+".sav", saveFixture, 0o600); err != nil {
		t.Fatal(err)
	}
	root.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	app.runSeedCleanup(ctx)
	assertNoLocalSeeds(t, app)
	if !runner.pvc {
		t.Fatal("restart cleanup deleted a successful seed")
	}
	for _, action := range []struct{ path, body string }{{"restart", ""}, {"update", `{"imageTag":"next"}`}} {
		req := httptest.NewRequest("POST", "/api/servers/"+server.ID+"/actions/"+action.path, strings.NewReader(action.body))
		req.Header.Set("Authorization", "Bearer test-admin")
		res := httptest.NewRecorder()
		app.ServeHTTP(res, req)
		if res.Code != 200 {
			t.Fatalf("%s = %d %s", action.path, res.Code, res.Body.String())
		}
	}
	var deployments int
	for _, call := range runner.calls {
		if strings.HasPrefix(call, "helm ") {
			deployments++
			for _, setting := range []string{"--namespace other-worlds", "saveSeed.existingClaim=" + runner.claim, "saveSeed.path=World 123.sav"} {
				if !strings.Contains(call, setting) {
					t.Fatalf("missing %q in %s", setting, call)
				}
			}
		}
	}
	if deployments != 2 || !runner.pvc {
		t.Fatalf("deployments = %d, pvc retained = %t", deployments, runner.pvc)
	}
	firstClaim := runner.claim
	duplicate := httptest.NewRecorder()
	app.ServeHTTP(duplicate, validSaveRequest(t))
	if duplicate.Code != 201 || runner.claim == firstClaim || len(app.store.Snapshot().Servers) != 2 {
		t.Fatal("second world did not get a distinct identity and seed")
	}
}

func TestSaveWriterSecurityAndAtomicPublish(t *testing.T) {
	app, runner := saveApp(t)
	res := httptest.NewRecorder()
	app.ServeHTTP(res, validSaveRequest(t))
	if res.Code != 201 {
		t.Fatal(res.Body.String())
	}
	items := runner.manifest["items"].([]any)
	for _, item := range items {
		metadata := item.(map[string]any)["metadata"].(map[string]any)
		if metadata["namespace"] != "other-worlds" || metadata["labels"].(map[string]any)[seedLabel] != runner.claim {
			t.Fatalf("wrong owner: %v", metadata)
		}
	}
	spec := items[1].(map[string]any)["spec"].(map[string]any)
	grace, ok := spec["terminationGracePeriodSeconds"].(float64)
	if !ok || grace < 0 || grace >= 30 {
		t.Fatalf("writer termination grace must leave time within the 30-second deletion timeout, got %v", spec["terminationGracePeriodSeconds"])
	}
	wantSecurity := map[string]any{"runAsUser": float64(1000), "runAsGroup": float64(1000), "runAsNonRoot": true, "fsGroup": float64(1000), "seccompProfile": map[string]any{"type": "RuntimeDefault"}}
	if !reflect.DeepEqual(spec["securityContext"], wantSecurity) || spec["automountServiceAccountToken"] != false || spec["restartPolicy"] != "Never" {
		t.Fatalf("unsafe writer: %v", spec)
	}
	container := spec["containers"].([]any)[0].(map[string]any)
	security := container["securityContext"].(map[string]any)
	if security["allowPrivilegeEscalation"] != false || security["readOnlyRootFilesystem"] != true || !reflect.DeepEqual(security["capabilities"], map[string]any{"drop": []any{"ALL"}}) {
		t.Fatalf("unsafe container: %v", security)
	}
	calls := strings.Join(runner.calls, "\n")
	copyAt, publishAt, deleteAt, deployAt := strings.Index(calls, " cp "), strings.Index(calls, "mv -T /seed/.incoming"), strings.Index(calls, " delete pod "), strings.Index(calls, "helm ")
	if copyAt < 0 || publishAt <= copyAt || deleteAt <= publishAt || deployAt <= deleteAt {
		t.Fatalf("wrong staging order: %s", calls)
	}
	if !strings.Contains(calls, "delete pod "+runner.claim+" --ignore-not-found --wait=true --timeout=30s") {
		t.Fatalf("writer removal must be confirmed before deployment: %s", calls)
	}
}

func TestSaveUploadImporterFilenameCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"-World.sav", http.StatusBadRequest},
		{"--help.sav", http.StatusBadRequest},
		{"World-save.sav", http.StatusCreated},
		{"World 123.sav", http.StatusCreated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, runner := saveApp(t)
			res := httptest.NewRecorder()
			app.ServeHTTP(res, saveRequest(t, uploadPart{"request", "", []byte(saveSettings)}, uploadPart{"save", tc.name, saveFixture}))
			if res.Code != tc.status {
				t.Fatalf("upload = %d, want %d: %s", res.Code, tc.status, res.Body.String())
			}
			assertNoLocalSeeds(t, app)
			if tc.status == http.StatusBadRequest {
				if len(runner.calls) != 0 || len(app.store.Snapshot().PendingSeeds) != 0 || len(app.store.Snapshot().Servers) != 0 {
					t.Fatal("option-like filename reached staging or deployment")
				}
				return
			}
			seed := serverNamed(t, app.store.Snapshot(), "Imported world").SaveSeed
			if seed == nil || seed.Path != tc.name || !bytes.Equal(runner.copied, saveFixture) {
				t.Fatal("staged save differs from upload")
			}
			if _, err := exec.LookPath("bash"); err != nil {
				t.Skip("Bash unavailable; importer execution is covered by verification/save-import.test.cjs")
			}
			source, world := t.TempDir(), filepath.Join(t.TempDir(), "world")
			if err := os.WriteFile(filepath.Join(source, seed.Path), runner.copied, 0o600); err != nil {
				t.Fatal(err)
			}
			output, err := exec.Command("bash", "verification/fixture-chart/files/seed-save.sh", source, seed.Path, world).CombinedOutput()
			if err != nil {
				t.Fatalf("import staged save: %v: %s", err, output)
			}
			data, err := os.ReadFile(filepath.Join(world, tc.name))
			if err != nil || !bytes.Equal(data, saveFixture) {
				t.Fatalf("imported save differs from upload: %v", err)
			}
			if _, err := os.Stat(filepath.Join(world, ".seed-complete")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSaveUploadRejectsInvalidInputBeforeWriting(t *testing.T) {
	settings := uploadPart{"request", "", []byte(saveSettings)}
	save := uploadPart{"save", "World.sav", saveFixture}
	for _, tc := range []struct {
		name  string
		parts []uploadPart
	}{
		{"type", []uploadPart{settings, {"save", "world.zip", saveFixture}}},
		{"traversal", []uploadPart{settings, {"save", "../World.sav", saveFixture}}},
		{"absolute", []uploadPart{settings, {"save", "/tmp/World.sav", saveFixture}}},
		{"windows", []uploadPart{settings, {"save", `..\World.sav`, saveFixture}}},
		{"empty", []uploadPart{settings, {"save", "world.sav", nil}}},
		{"two files", []uploadPart{settings, save, save}},
		{"two settings", []uploadPart{settings, settings, save}},
		{"missing settings", []uploadPart{save}},
		{"missing file", []uploadPart{settings}},
		{"unknown part", []uploadPart{settings, save, {"extra", "", []byte("private-world-fixture")}}},
		{"bad settings", []uploadPart{{"request", "", []byte(`{"bad":"private-world-fixture"}`)}, save}},
		{"trailing JSON", []uploadPart{{"request", "", []byte(saveSettings + ` {}`)}, save}},
		{"invalid owner", []uploadPart{{"request", "", []byte(`{"name":"x","ownerId":"invalid","maxPlayers":4}`)}, save}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, runner := saveApp(t)
			res := httptest.NewRecorder()
			app.ServeHTTP(res, saveRequest(t, tc.parts...))
			if res.Code != 400 || strings.Contains(res.Body.String(), "private-world-fixture") {
				t.Fatalf("response = %d %s", res.Code, res.Body.String())
			}
			if len(runner.calls) != 0 || len(app.store.Snapshot().PendingSeeds) != 0 {
				t.Fatal("invalid input reached staging")
			}
			if _, err := os.Stat(app.store.path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("invalid input wrote state")
			}
			assertNoLocalSeeds(t, app)
		})
	}
}

type repeatedByteReader struct{}

func (repeatedByteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func TestSaveUploadSizeBoundary(t *testing.T) {
	for _, knownLength := range []bool{true, false} {
		app, runner := saveApp(t)
		req := validSaveRequest(t)
		req.Body = io.NopCloser(io.LimitReader(repeatedByteReader{}, maxCreateBytes+1))
		req.ContentLength = -1
		if knownLength {
			req.ContentLength = maxCreateBytes + 1
		}
		res := httptest.NewRecorder()
		app.ServeHTTP(res, req)
		if res.Code != 413 || !strings.Contains(res.Body.String(), "32 MiB") || len(runner.calls) != 0 {
			t.Fatalf("limit response = %d %s", res.Code, res.Body.String())
		}
		assertNoLocalSeeds(t, app)
	}
	for _, size := range []int{maxSaveBytes, maxSaveBytes + 1} {
		app, runner := saveApp(t)
		req := saveRequest(t, uploadPart{"request", "", []byte(saveSettings)}, uploadPart{"save", "big.sav", bytes.Repeat([]byte{'s'}, size)})
		res := httptest.NewRecorder()
		app.ServeHTTP(res, req)
		if size == maxSaveBytes {
			if res.Code != 201 || len(runner.copied) != maxSaveBytes {
				t.Fatalf("exact limit = %d %s", res.Code, res.Body.String())
			}
		} else if res.Code != 413 || len(runner.calls) != 0 {
			t.Fatalf("file limit = %d %s", res.Code, res.Body.String())
		}
		assertNoLocalSeeds(t, app)
	}
}

func TestSaveUploadAuthenticationAndCreateLock(t *testing.T) {
	app, runner := saveApp(t)
	req := validSaveRequest(t)
	req.Header.Del("Authorization")
	res := httptest.NewRecorder()
	app.ServeHTTP(res, req)
	if res.Code != 401 {
		t.Fatalf("unauthenticated = %d", res.Code)
	}
	app.lifecycleMu.Lock()
	res = httptest.NewRecorder()
	app.ServeHTTP(res, validSaveRequest(t))
	app.lifecycleMu.Unlock()
	if res.Code != 409 || len(runner.calls) != 0 {
		t.Fatalf("concurrent create = %d", res.Code)
	}
	assertNoLocalSeeds(t, app)
}

func TestSaveRequiresPersistentStorageButEmptyWorldStillWorks(t *testing.T) {
	t.Setenv("RSDW_SAVE_UPLOADS_ENABLED", "false")
	app, runner := saveApp(t)
	res := httptest.NewRecorder()
	app.ServeHTTP(res, validSaveRequest(t))
	if res.Code != 503 || len(runner.calls) != 0 {
		t.Fatalf("ephemeral upload = %d %s", res.Code, res.Body.String())
	}
	req := httptest.NewRequest("POST", "/api/servers", strings.NewReader(saveSettings))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-admin")
	res = httptest.NewRecorder()
	app.ServeHTTP(res, req)
	if res.Code != 201 || serverNamed(t, app.store.Snapshot(), "Imported world").SaveSeed != nil {
		t.Fatalf("empty world = %d %s", res.Code, res.Body.String())
	}
	if strings.Contains(strings.Join(runner.calls, "\n"), "saveSeed.") {
		t.Fatal("empty world unexpectedly requested a seed")
	}
	assertNoLocalSeeds(t, app)
}

func TestSaveUploadOIDCRolesAndCSRF(t *testing.T) {
	app, issuer := oidcTestApp(t)
	admin, csrf := loginAs(t, app, issuer, "admin")
	viewer, viewerCSRF := loginAs(t, app, issuer, "viewer")
	runner := &saveRunner{t: t}
	app.orchestrator = &kubeOrchestrator{runner: runner, helm: "helm", kubectl: "kubectl", chart: "chart"}
	for _, tc := range []struct {
		cookie       []*http.Cookie
		origin, csrf string
		status       int
	}{
		{nil, "", "", 401},
		{viewer, app.auth.settings.Origin, viewerCSRF, 403},
		{admin, app.auth.settings.Origin, "", 403},
		{admin, "https://evil.example", csrf, 403},
		{admin, app.auth.settings.Origin, csrf, 201},
	} {
		req := validSaveRequest(t)
		req.Header.Del("Authorization")
		for _, cookie := range tc.cookie {
			req.AddCookie(cookie)
		}
		req.Header.Set("Origin", tc.origin)
		req.Header.Set("X-CSRF-Token", tc.csrf)
		res := httptest.NewRecorder()
		app.ServeHTTP(res, req)
		if res.Code != tc.status {
			t.Fatalf("OIDC upload = %d, want %d: %s", res.Code, tc.status, res.Body.String())
		}
		if tc.status != 201 && len(runner.calls) != 0 {
			t.Fatal("unauthorized upload reached Kubernetes")
		}
	}
	if !bytes.Equal(runner.copied, saveFixture) {
		t.Fatal("admin upload was not staged")
	}
}

type interruptedSaveReader struct{}

func (interruptedSaveReader) Read([]byte) (int, error) {
	return 0, errors.New("private-world-fixture interrupted")
}

func TestSaveAbandonedRequestAndStagingRecovery(t *testing.T) {
	app, runner := saveApp(t)
	req := validSaveRequest(t)
	req.Body = io.NopCloser(interruptedSaveReader{})
	req.ContentLength = -1
	res := httptest.NewRecorder()
	app.ServeHTTP(res, req)
	if res.Code != 400 || len(runner.calls) != 0 || strings.Contains(res.Body.String(), "private-world-fixture") {
		t.Fatalf("interruption = %d %s", res.Code, res.Body.String())
	}
	assertNoLocalSeeds(t, app)
	seed, err := app.prepareSeed(context.Background(), app.orchestrator.(*kubeOrchestrator), Server{Release: "abandoned", Namespace: "other-worlds"}, &saveUpload{name: "World.sav", data: saveFixture})
	if err != nil {
		t.Fatal(err)
	}
	if !runner.pvc || len(app.store.Snapshot().PendingSeeds) != 1 {
		t.Fatal("staging not durably owned")
	}
	reloaded, err := NewStore(app.store.path, false)
	if err != nil {
		t.Fatal(err)
	}
	app.store = reloaded
	root, err := app.seedRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile(seed.Claim+".sav.tmp", saveFixture, 0o600); err != nil {
		t.Fatal(err)
	}
	root.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	app.runSeedCleanup(ctx)
	assertNoLocalSeeds(t, app)
	if runner.pvc || len(app.store.Snapshot().PendingSeeds) != 0 {
		t.Fatal("restart did not clean abandoned staging")
	}
}

func TestSaveFailureCleanupAndRestartRecovery(t *testing.T) {
	for _, failure := range []string{"create -f", " wait ", " cp ", " exec ", " delete pod ", "helm "} {
		t.Run(failure, func(t *testing.T) {
			app, runner := saveApp(t)
			runner.fail = failure
			res := httptest.NewRecorder()
			app.ServeHTTP(res, validSaveRequest(t))
			if res.Code != 502 || strings.Contains(res.Body.String(), "private-world-fixture") {
				t.Fatalf("failure = %d %s", res.Code, res.Body.String())
			}
			if len(app.store.Snapshot().Servers) != 0 {
				t.Fatal("failure registered a successful server")
			}
			if failure == " delete pod " && strings.Contains(strings.Join(runner.calls, "\n"), "helm ") {
				t.Fatal("deployed before confirming writer removal")
			}
			assertNoLocalSeeds(t, app)
			if failure == " delete pod " {
				if !runner.pvc || len(app.store.Snapshot().PendingSeeds) != 1 {
					t.Fatal("cleanup failure lost ownership")
				}
				reloaded, err := NewStore(app.store.path, false)
				if err != nil {
					t.Fatal(err)
				}
				app.store = reloaded
				runner.fail = ""
				for claim := range reloaded.Snapshot().PendingSeeds {
					app.cleanupSeed(claim)
				}
			}
			if runner.pvc || runner.writer || len(app.store.Snapshot().PendingSeeds) != 0 {
				t.Fatal("failed request left staging resources")
			}
		})
	}
}

func TestSaveCleanupRetainsDeploymentAndPodReferences(t *testing.T) {
	for _, reference := range []string{"deployment", "pod"} {
		t.Run(reference, func(t *testing.T) {
			app, runner := saveApp(t)
			runner.fail = "helm "
			runner.referenced = reference == "deployment"
			runner.podReferenced = reference == "pod"
			res := httptest.NewRecorder()
			app.ServeHTTP(res, validSaveRequest(t))
			if res.Code != 502 || !runner.pvc || runner.writer || len(app.store.Snapshot().PendingSeeds) != 1 {
				t.Fatal("lost a referenced seed")
			}
			reloaded, err := NewStore(app.store.path, false)
			if err != nil {
				t.Fatal(err)
			}
			app.store = reloaded
			runner.fail = " get deployments "
			app.cleanupSeed(runner.claim)
			if !runner.pvc || len(app.store.Snapshot().PendingSeeds) != 1 {
				t.Fatal("uncertain cleanup lost ownership")
			}
			runner.fail = ""
			runner.referenced = false
			runner.podReferenced = false
			app.cleanupSeed(runner.claim)
			if runner.pvc || len(app.store.Snapshot().PendingSeeds) != 0 {
				t.Fatal("unused seed was not cleaned")
			}
		})
	}
}

func TestSaveStagingRejectsSymlinksAndKeepsAtomicFiles(t *testing.T) {
	app, _ := saveApp(t)
	root, err := app.seedRoot()
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := writeSeedFile(root, "fixture.sav", saveFixture); err != nil {
		t.Fatal(err)
	}
	data, err := root.ReadFile("fixture.sav")
	if err != nil {
		t.Fatal(err)
	}
	info, err := root.Stat("fixture.sav")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, saveFixture) || info.Mode().Perm() != 0o600 {
		t.Fatal("incorrect staged bytes or mode")
	}
	if _, err := root.Stat("fixture.sav.tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("temporary file survived rename")
	}
	outside := filepath.Join(t.TempDir(), "outside.sav")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := root.Symlink(outside, "escape.sav.tmp"); err != nil {
		t.Fatal(err)
	}
	if err := writeSeedFile(root, "escape.sav", saveFixture); err == nil {
		t.Fatal("accepted escaped temporary symlink")
	}
	if err := root.Symlink(outside, "existing.sav"); err != nil {
		t.Fatal(err)
	}
	if err := writeSeedFile(root, "existing.sav", saveFixture); err == nil {
		t.Fatal("replaced existing symlink")
	}
	if err := writeSeedFile(root, "../outside.sav", saveFixture); err == nil {
		t.Fatal("accepted traversal")
	}
	data, err = os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "keep" {
		t.Fatal("modified external file")
	}
}

func TestSavePersistenceFailureKeepsSeedOwned(t *testing.T) {
	app, runner := saveApp(t)
	path := app.store.path
	runner.onHelm = func() { runner.referenced = true; app.store.path = filepath.Join(path, "unwritable.json") }
	res := httptest.NewRecorder()
	app.ServeHTTP(res, validSaveRequest(t))
	app.store.path = path
	if res.Code != 500 || !runner.pvc {
		t.Fatalf("commit failure = %d", res.Code)
	}
	reloaded, err := NewStore(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Snapshot().PendingSeeds) != 1 || len(reloaded.Snapshot().Servers) != 0 {
		t.Fatal("commit failure lost seed ownership")
	}
	app.store = reloaded
	app.cleanupSeed(runner.claim)
	assertNoLocalSeeds(t, app)
	if !runner.pvc {
		t.Fatal("commit failure deleted a referenced seed")
	}
}
