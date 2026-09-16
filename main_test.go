package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
	if server.ID != "night-shift" || server.Status != StatusOnline {
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
		return []byte(`{"spec":{"template":{"spec":{"containers":[{"name":"server","image":"example/server:1.2.3"}]}}},"status":{"availableReplicas":1,"replicas":1}}`), nil
	case strings.Contains(call, "api/health"):
		return []byte(`{"engineReady":true,"uptimeSeconds":123.5}`), nil
	case strings.Contains(call, "api/players"):
		return []byte(`{"count":4}`), nil
	default:
		return nil, errors.New("unexpected command: " + call)
	}
}

func TestKubeOrchestratorRefreshReadsGameMetrics(t *testing.T) {
	runner := &metricsRunner{}
	orchestrator := &kubeOrchestrator{runner: runner, kubectl: "kubectl", gameAPIPort: "8080"}
	server := Server{Release: "night-shift", Namespace: "dragonwilds", DesiredImage: "example/server:1.2.3"}
	refreshed, err := orchestrator.Refresh(context.Background(), server)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Status != StatusOnline || refreshed.Players != 4 || refreshed.UptimeSeconds != 123 {
		t.Fatalf("refreshed server = %+v", refreshed)
	}
	if !strings.Contains(strings.Join(runner.calls, "\n"), `-H "Authorization: Bearer $(cat /run/rsdwapi/token)"`) {
		t.Fatalf("game API calls did not read the pod token: %v", runner.calls)
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
	telemetry := telemetryFor(server, "60s", false)
	if telemetry.MetricsAvailable || len(telemetry.Samples) != 0 {
		t.Fatalf("kubernetes telemetry = %+v, want no synthetic samples", telemetry)
	}
	if got := telemetry.HealthChecks[0].Status; got != "online" {
		t.Fatalf("health status = %q, want online", got)
	}
}

func TestKubeOrchestratorUsesChartContract(t *testing.T) {
	runner := &recordingRunner{}
	orchestrator := &kubeOrchestrator{runner: runner, helm: "helm", kubectl: "kubectl", chart: "oci://example/chart", chartVersion: "0.1.1", imageRepository: "example/server"}
	server := Server{Release: "night-shift", Namespace: "dragonwilds", Name: "Night Shift", OwnerID: "eos-1", DesiredImage: "example/server:1.2.3"}
	if err := orchestrator.Deploy(context.Background(), server); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(runner.calls, "\n")
	for _, expected := range []string{
		"kubectl create namespace dragonwilds",
		"kubectl -n dragonwilds create secret generic night-shift-api",
		"helm upgrade --install night-shift oci://example/chart",
		"--set-string server.env.RSDW_OWNER_ID=eos-1",
		"--set-string image.tag=1.2.3",
		"--set-string api.bearerTokenSecret.name=night-shift-api",
		"--version 0.1.1",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("deployment command missing %q in:\n%s", expected, joined)
		}
	}
}
