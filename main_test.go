package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type registryTransport func(*http.Request) (*http.Response, error)

func (f registryTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCheckUpdateRegistryBehavior(t *testing.T) {
	for _, tc := range []struct {
		name, tokenBody, tagsBody           string
		tokenStatus, tagsStatus, wantStatus int
	}{
		{"authenticated", `{"token":"registry-secret"}`, `{"tags":["1.2.3","1.2.4"]}`, 200, 200, 200},
		{"access token", `{"access_token":"registry-secret"}`, `{"tags":["1.2.3","1.2.4"]}`, 200, 200, 200},
		{"token denied", `{}`, `{}`, 403, 200, 502},
		{"empty token", `{}`, `{}`, 200, 200, 502},
		{"invalid token JSON", `{`, `{}`, 200, 200, 502},
		{"registry failure", `{"token":"registry-secret"}`, `{}`, 200, 503, 502},
		{"retry denied", `{"token":"registry-secret"}`, `{}`, 200, 401, 502},
		{"invalid tags JSON", `{"token":"registry-secret"}`, `{`, 200, 200, 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/token":
					if r.URL.Query().Get("service") != "ghcr.io" || r.URL.Query().Get("scope") != "repository:example/server:pull" {
						t.Errorf("unexpected token query: %s", r.URL.RawQuery)
					}
					w.WriteHeader(tc.tokenStatus)
					fmt.Fprint(w, tc.tokenBody)
				case "/v2/example/server/tags/list":
					if r.Header.Get("Authorization") != "Bearer registry-secret" {
						w.Header().Set("WWW-Authenticate", `Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="repository:example/server:pull"`)
						w.WriteHeader(http.StatusUnauthorized)
						return
					}
					w.WriteHeader(tc.tagsStatus)
					fmt.Fprint(w, tc.tagsBody)
				default:
					t.Errorf("unexpected registry path %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer registry.Close()
			original := http.DefaultClient
			http.DefaultClient = &http.Client{Transport: registryTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "ghcr.io" {
					return nil, fmt.Errorf("unexpected host %s", r.URL.Host)
				}
				clone := r.Clone(r.Context())
				clone.URL.Host = strings.TrimPrefix(registry.URL, "https://")
				return registry.Client().Transport.RoundTrip(clone)
			})}
			t.Cleanup(func() { http.DefaultClient = original })
			app := newTestApp(t, false)
			app.orchestrator = &kubeOrchestrator{runner: &metricsRunner{}, kubectl: "kubectl", imageRepository: "ghcr.io/example/server"}
			server := Server{ID: "world", Release: "world", CurrentImage: "example/server:1.2.3", DesiredImage: "example/server:1.2.3"}
			if err := app.store.Update(func(s *State) error { s.Servers[server.ID] = server; return nil }); err != nil {
				t.Fatal(err)
			}
			res := requestJSON(t, app, http.MethodPost, "/api/servers/world/actions/check-update", "")
			if res.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", res.Code, tc.wantStatus, res.Body.String())
			}
			state := app.store.Snapshot()
			if tc.wantStatus == 200 {
				var got Server
				if err := json.Unmarshal(res.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if !got.UpdateAvailable || got.DesiredImage != "ghcr.io/example/server:1.2.4" || len(state.Events) != 1 {
					t.Fatalf("update result = %+v, events = %d", got, len(state.Events))
				}
			} else if !reflect.DeepEqual(state.Servers[server.ID], server) || len(state.Events) != 0 {
				t.Fatalf("failed check changed state: %+v", state)
			}
		})
	}
}

func TestShellRunnerSecretFailure(t *testing.T) {
	output, err := (shellRunner{}).Run(context.Background(), "sh", "-c", `printf '%s' "$1" >&2; exit 1`, "sh", "--from-literal=token=test-secret-value")
	if err == nil {
		t.Fatal("expected command failure")
	}
	if strings.Contains(err.Error(), "test-secret-value") || strings.Contains(string(output), "test-secret-value") {
		t.Fatal("command failure exposed the Secret token")
	}
	if !strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("missing failure reason: %v", err)
	}
}

type secretFailureRunner struct{ token string }

func (r *secretFailureRunner) Run(ctx context.Context, _ string, args ...string) ([]byte, error) {
	if len(args) > 1 && args[0] == "get" && args[1] == "namespace" {
		return nil, nil
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, "--from-literal=token=") {
			r.token = strings.TrimPrefix(arg, "--from-literal=token=")
			return (shellRunner{}).Run(ctx, "sh", "-c", `printf '%s' "$1" >&2; exit 1`, "sh", arg)
		}
	}
	return nil, errors.New("not found")
}

func TestCreateSecretFailureDoesNotLeakToken(t *testing.T) {
	app := newTestApp(t, false)
	runner := &secretFailureRunner{}
	app.orchestrator = &kubeOrchestrator{runner: runner, kubectl: "kubectl"}
	res := requestJSON(t, app, http.MethodPost, "/api/servers", `{"name":"World","ownerId":"owner","maxPlayers":4}`)
	if res.Code != http.StatusBadGateway || !strings.Contains(res.Body.String(), "create API token Secret") {
		t.Fatalf("create response = %d: %s", res.Code, res.Body.String())
	}
	if runner.token == "" || strings.Contains(res.Body.String(), runner.token) {
		t.Fatal("Secret creation was not attempted or its token leaked")
	}
	state := app.store.Snapshot()
	if len(state.Servers) != 0 || len(state.Events) != 0 {
		t.Fatalf("failed creation recorded success: %+v", state)
	}
}

func newTestApp(t *testing.T, demo bool) *App {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := NewStore(path, demo)
	if err != nil {
		t.Fatal(err)
	}
	return &App{store: store, orchestrator: demoOrchestrator{}, demo: demo}
}

func requestJSON(t *testing.T, app *App, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	res := httptest.NewRecorder()
	app.ServeHTTP(res, req)
	return res
}

func TestStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := NewStore(path, false)
	if err != nil {
		t.Fatal(err)
	}
	server := Server{ID: "world", Name: "World", Status: StatusOnline}
	if err := store.Update(func(state *State) error { state.Servers[server.ID] = server; return nil }); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewStore(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Snapshot().Servers["world"].Name; got != "World" {
		t.Fatalf("round trip name = %q, want World", got)
	}
	if mode, err := os.Stat(path); err != nil || mode.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode = %v, want 0600", mode.Mode().Perm())
	}
}

func TestValidateCreate(t *testing.T) {
	for _, limits := range []struct{ memory, cpu int }{{-1, 1000}, {255, 1000}, {65537, 1000}, {2048, -1}, {2048, 99}, {2048, 64001}} {
		if err := validateCreate(CreateServerRequest{Name: "World", OwnerID: "owner", MaxPlayers: 4, MemoryLimitMiB: limits.memory, CPULimitMillis: limits.cpu}, false); err == nil {
			t.Fatalf("invalid resource limits accepted: %+v", limits)
		}
	}
	if err := validateCreate(CreateServerRequest{Name: "World", MaxPlayers: 12}, false); err == nil {
		t.Fatal("missing owner ID accepted")
	}
	if err := validateCreate(CreateServerRequest{Name: "World", OwnerID: "eos", MaxPlayers: 12, Namespace: "Dragonwilds"}, false); err == nil {
		t.Fatal("invalid namespace accepted")
	}
	if err := validateCreate(CreateServerRequest{Name: "World", OwnerID: "eos", MaxPlayers: 12, ImageTag: "v1.2.3"}, false); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
}

func TestCreatePersistsResourceLimits(t *testing.T) {
	app := newTestApp(t, false)
	res := requestJSON(t, app, http.MethodPost, "/api/servers", `{"name":"Resource test","ownerId":"owner","maxPlayers":6,"memoryLimitMiB":1536,"cpuLimitMillis":750}`)
	if res.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", res.Code, res.Body.String())
	}
	reloaded, err := NewStore(app.store.path, false)
	if err != nil {
		t.Fatal(err)
	}
	server := reloaded.Snapshot().Servers["resource-test"]
	if server.MemoryLimitMiB != 1536 || server.CPULimitMillis != 750 || server.MaxPlayers != 6 {
		t.Fatalf("limits not persisted: %+v", server)
	}
}

func TestCreateChartSettingsAndSecretPrivacy(t *testing.T) {
	app := newTestApp(t, false)
	runner := &recordingRunner{}
	app.orchestrator = &kubeOrchestrator{runner: runner, helm: "helm", kubectl: "kubectl", chart: "chart", imageRepository: "example/server"}
	res := requestJSON(t, app, http.MethodPost, "/api/servers", `{"name":"Public name","worldName":"Separate world","ownerId":"owner-123","maxPlayers":6,"memoryLimitMiB":1536,"cpuLimitMillis":750,"gamePort":7780,"storageGiB":2,"serviceType":"NodePort","adminIds":"admin-1,admin-2","debugLevel":3,"autoStopOnUpdate":true,"validateGameFiles":true,"additionalArgs":"-log","serverPassword":"private-join","adminPassword":"private-admin"}`)
	if res.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", res.Code, res.Body.String())
	}
	persisted, err := os.ReadFile(app.store.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-join", "private-admin"} {
		if strings.Contains(res.Body.String(), secret) || strings.Contains(string(persisted), secret) {
			t.Fatal("password exposed outside Secret command")
		}
	}
	server := app.store.Snapshot().Servers["public-name"]
	if server.WorldName != "Separate world" || server.GamePort != 7780 || server.PasswordSecret != "public-name-settings" || server.ServerPassword != "" || server.AdminPassword != "" {
		t.Fatalf("settings not preserved or credentials retained: %+v", server)
	}
	var helmCall string
	for _, call := range runner.calls {
		if strings.HasPrefix(call, "helm ") {
			helmCall = call
		}
	}
	for _, want := range []string{"RSDW_WORLD_NAME=Separate world", "RSDW_ADMINS=admin-1,admin-2", "RSDW_ADDITIONAL_ARGS=-log -ini:Game:[/Script/Engine.GameSession]:MaxPlayers=6", "RSDW_AUTO_STOP_ON_UPDATE=true", "DEBUG=3", "STEAMAPPVALIDATE=1", "server.port=7780,service.port=7780,persistence.size=2Gi,service.type=NodePort", "resources.limits.memory=1536Mi", "resources.limits.cpu=750m", "valueFrom.secretKeyRef.name=public-name-settings"} {
		if !strings.Contains(helmCall, want) {
			t.Fatalf("missing chart setting %q", want)
		}
	}
	if strings.Contains(helmCall, "private-join") || strings.Contains(helmCall, "private-admin") {
		t.Fatal("password passed as a Helm value")
	}
}

func TestDemoAPIExercisesMutations(t *testing.T) {
	app := newTestApp(t, false)
	res := requestJSON(t, app, http.MethodGet, "/", "")
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "DRAGONWILDS") {
		t.Fatalf("embedded UI status = %d, body = %s", res.Code, res.Body.String())
	}
	res = requestJSON(t, app, http.MethodPost, "/api/servers", `{"name":"Night Shift","namespace":"dragonwilds","region":"eu-central","ownerId":"eos-1","imageTag":"0.1.1","maxPlayers":12}`)
	if res.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", res.Code, res.Body.String())
	}
	var server Server
	if err := json.Unmarshal(res.Body.Bytes(), &server); err != nil {
		t.Fatal(err)
	}
	if server.ID != "night-shift" || server.Status != StatusStarting || server.MemoryLimitMiB != 2048 || server.CPULimitMillis != 1000 {
		t.Fatalf("created server = %+v", server)
	}
	for _, action := range []struct {
		path string
		body string
	}{
		{"/api/servers/night-shift/actions/restart", ""},
		{"/api/servers/night-shift/actions/update", `{"imageTag":"0.1.2"}`},
		{"/api/servers/night-shift/actions/check-update", ""},
	} {
		res = requestJSON(t, app, http.MethodPost, action.path, action.body)
		if res.Code != http.StatusOK {
			t.Fatalf("%s status = %d, body = %s", action.path, res.Code, res.Body.String())
		}
	}
	if res = requestJSON(t, app, http.MethodGet, "/api/servers/night-shift/logs?tail=10", ""); res.Code != http.StatusOK {
		t.Fatalf("logs status = %d", res.Code)
	}
	if res = requestJSON(t, app, http.MethodGet, "/api/servers/night-shift/telemetry?range=60s", ""); res.Code != http.StatusOK {
		t.Fatalf("telemetry status = %d", res.Code)
	}
	if res = requestJSON(t, app, http.MethodGet, "/api/events?query=update&category=update&serverId=night-shift", ""); res.Code != http.StatusOK {
		t.Fatalf("events status = %d", res.Code)
	}
	if res = requestJSON(t, app, http.MethodPost, "/api/servers/night-shift/actions/update", `{"imageTag":"bad tag"}`); res.Code != http.StatusBadRequest {
		t.Fatalf("invalid update status = %d", res.Code)
	}
}

func TestAuthBoundary(t *testing.T) {
	app := newTestApp(t, false)
	app.authToken = "secret"
	res := requestJSON(t, app, http.MethodGet, "/api/bootstrap", "")
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", res.Code)
	}
	res = requestJSON(t, app, http.MethodPost, "/api/session", `{"token":"secret"}`)
	if res.Code != http.StatusNoContent {
		t.Fatalf("session status = %d", res.Code)
	}
	res = requestJSON(t, app, http.MethodGet, "/api/bootstrap", "")
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("session did not create a bearer token = %d", res.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/bootstrap", nil)
	req.Header.Set("Authorization", "Bearer secret")
	res = httptest.NewRecorder()
	app.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("valid bearer status = %d", res.Code)
	}
}

func TestParseLogs(t *testing.T) {
	lines := parseLogs("2026-09-16T10:00:00Z WARN image update available\nplain info")
	if len(lines) != 2 || lines[0].Level != "WARN" || lines[1].Message != "plain info" {
		t.Fatalf("parsed logs = %+v", lines)
	}
}

func TestConfigureInClusterKubeconfig(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "token")
	caPath := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(tokenPath, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caPath, []byte("ca"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", "")
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	t.Setenv("KUBERNETES_SERVICE_PORT_HTTPS", "443")
	t.Setenv("RSDW_SERVICE_ACCOUNT_TOKEN_FILE", tokenPath)
	t.Setenv("RSDW_SERVICE_ACCOUNT_CA_FILE", caPath)
	configureInClusterKubeconfig()
	path := filepath.Join(os.TempDir(), "rsdw-c2-kubeconfig")
	defer os.Remove(path)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	config := string(data)
	for _, expected := range []string{"server: https://kubernetes.default.svc:443", "certificate-authority: " + caPath, "tokenFile: " + tokenPath} {
		if !strings.Contains(config, expected) {
			t.Fatalf("kubeconfig missing %q: %s", expected, config)
		}
	}
	if got := os.Getenv("KUBECONFIG"); got != path {
		t.Fatalf("KUBECONFIG = %q, want %q", got, path)
	}
}

type recordingRunner struct {
	calls []string
}

func (r *recordingRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := strings.Join(append([]string{name}, args...), " ")
	r.calls = append(r.calls, call)
	if strings.Contains(call, "get namespace") || strings.Contains(call, "get secret") {
		return nil, errors.New("not found")
	}
	return []byte("ok"), nil
}

type metricsRunner struct {
	calls []string
}

func (r *metricsRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := strings.Join(append([]string{name}, args...), " ")
	r.calls = append(r.calls, call)
	switch {
	case strings.Contains(call, "get deployment"):
		namespace := ""
		release := "world"
		if strings.Contains(call, "night-shift") {
			namespace = "dragonwilds"
			release = "night-shift"
		}
		return []byte(fmt.Sprintf(`{"metadata":{"name":%q,"namespace":%q,"uid":"deployment"},"spec":{"replicas":1}}`, deploymentName(release), namespace)), nil
	case strings.Contains(call, "get replicasets"):
		namespace := ""
		if strings.Contains(call, "-n dragonwilds") {
			namespace = "dragonwilds"
		}
		return []byte(fmt.Sprintf(`{"items":[{"metadata":{"namespace":%q,"uid":"rs","ownerReferences":[{"kind":"Deployment","uid":"deployment","controller":true}]}}]}`, namespace)), nil
	case strings.Contains(call, "get pods"):
		namespace := ""
		if strings.Contains(call, "-n dragonwilds") {
			namespace = "dragonwilds"
		}
		return []byte(fmt.Sprintf(`{"items":[{"metadata":{"name":"world-pod","namespace":%q,"uid":"pod","ownerReferences":[{"kind":"ReplicaSet","uid":"rs","controller":true}]},"spec":{"containers":[{"name":"server","image":"example/server:1.2.3"}]},"status":{"phase":"Running","containerStatuses":[{"name":"server","containerID":"container","ready":true,"state":{"running":{"startedAt":"2026-01-01T00:00:00Z"}}}]}}]}`, namespace)), nil
	case strings.Contains(call, "api/health"):
		return []byte(`{"engineReady":true,"uptimeSeconds":123.5}`), nil
	case strings.Contains(call, "api/players"):
		return []byte(`{"count":4}`), nil
	default:
		return nil, errors.New("unexpected command: " + call)
	}
}

func TestKubeOrchestratorRefreshOnlyDiscoversImage(t *testing.T) {
	runner := &metricsRunner{}
	orchestrator := &kubeOrchestrator{runner: runner, kubectl: "kubectl", gameAPIPort: "8080"}
	server := Server{Release: "night-shift", Namespace: "dragonwilds", DesiredImage: "example/server:1.2.3"}
	refreshed, err := orchestrator.Refresh(context.Background(), server)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Status != StatusOnline || refreshed.CurrentImage != "example/server:1.2.3" || refreshed.MetricsAvailable {
		t.Fatalf("refreshed server = %+v", refreshed)
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "exec ") {
			t.Fatalf("image discovery executed a game API command: %s", call)
		}
	}
	for key, reading := range refreshed.Metrics {
		if reading.Value != nil {
			t.Fatalf("image discovery returned metric %s: %+v", key, reading)
		}
	}
}

func TestCheckUpdateRejectsMissingObservedImage(t *testing.T) {
	k, runner, server := collectorFixture()
	server.CurrentImage = "example/server:old"
	server.DesiredImage = ""
	runner.override = func(call string) ([]byte, error, bool) {
		if strings.Contains(call, "get pods") {
			return []byte(`{"items":[` + strings.ReplaceAll(fixturePod(), `"image":"example/server:1"`, `"image":""`) + `]}`), nil, true
		}
		return nil, nil, false
	}
	registryCalls := 0
	original := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: registryTransport(func(*http.Request) (*http.Response, error) {
		registryCalls++
		return nil, errors.New("unexpected registry lookup")
	})}
	t.Cleanup(func() { http.DefaultClient = original })
	k.imageRepository = "ghcr.io/example/server"
	for _, check := range []struct {
		name string
		run  func(context.Context, Server) (Server, error)
	}{{"Refresh", k.Refresh}, {"CheckUpdate", k.CheckUpdate}} {
		got, err := check.run(context.Background(), server)
		if err == nil || err.Error() != "observed server image is unavailable" {
			t.Errorf("%s error = %v, want unavailable observed image", check.name, err)
		}
		if !reflect.DeepEqual(got, server) {
			t.Errorf("%s changed server without an observed image", check.name)
		}
	}
	if registryCalls != 0 {
		t.Fatalf("missing observed image triggered %d registry lookups", registryCalls)
	}
}

func TestSemverTagOrdering(t *testing.T) {
	oldVersion, oldOK := semverTag("v0.1.9")
	newVersionValue, newOK := semverTag("0.1.10")
	if !oldOK || !newOK || !newerVersion(newVersionValue, oldVersion) {
		t.Fatalf("semver ordering failed: old=%v/%t new=%v/%t", oldVersion, oldOK, newVersionValue, newOK)
	}
	if _, ok := semverTag("latest"); ok {
		t.Fatal("latest should not be treated as semver")
	}
}

func TestTelemetryDoesNotFabricateKubernetesSamples(t *testing.T) {
	server := Server{Status: StatusOnline, Players: 4, UptimeSeconds: 123}
	telemetry := newTestApp(t, false).telemetryFor(server, "60s")
	if telemetry.MetricsAvailable || len(telemetry.Samples) != 0 {
		t.Fatalf("kubernetes telemetry = %+v, want no synthetic samples", telemetry)
	}
	if len(telemetry.HealthChecks) != 0 {
		t.Fatalf("unperformed health checks = %+v", telemetry.HealthChecks)
	}
}

func TestKubeOrchestratorUsesChartContract(t *testing.T) {
	runner := &recordingRunner{}
	orchestrator := &kubeOrchestrator{runner: runner, helm: "helm", kubectl: "kubectl", chart: "oci://example/chart", chartVersion: "0.1.1", imageRepository: "example/server"}
	server := Server{Release: "night-shift", Namespace: "dragonwilds", Name: "Night Shift", OwnerID: "eos-1", DesiredImage: "example/server:1.2.3", MaxPlayers: 12}
	if err := orchestrator.Deploy(context.Background(), server); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(runner.calls, "\n")
	for _, expected := range []string{
		"kubectl create namespace dragonwilds",
		"kubectl -n dragonwilds create secret generic night-shift-api",
		"helm upgrade --install night-shift oci://example/chart",
		"--set-literal server.env.RSDW_OWNER_ID=eos-1",
		"--set-literal server.env.RSDW_ADDITIONAL_ARGS=-ini:Game:[/Script/Engine.GameSession]:MaxPlayers=12",
		"--set-string image.tag=1.2.3",
		"--set-string api.bearerTokenSecret.name=night-shift-api",
		"--version 0.1.1",
		"--set-string resources.limits.memory=2048Mi",
		"--set-string resources.limits.cpu=1000m",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("deployment command missing %q in:\n%s", expected, joined)
		}
	}
}
