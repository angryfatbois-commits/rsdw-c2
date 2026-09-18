package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

type scaleRecorder struct {
	app   *App
	calls []string
	err   error
}

func (r *scaleRecorder) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	joined := name + " " + strings.Join(args, " ")
	if r.app != nil {
		status := r.app.store.Snapshot().Servers["world"].Status
		if strings.Contains(joined, "--replicas=0") && status != StatusStopped {
			return nil, errors.New("scale zero before persist")
		}
		if strings.Contains(joined, "--replicas=1") && status != StatusStopped && status != StatusStarting && status != StatusOnline {
			return nil, errors.New("scale one from an unexpected status")
		}
	}
	r.calls = append(r.calls, joined)
	return nil, r.err
}

func stopStartKubeApp(t *testing.T) (*App, *scaleRecorder) {
	t.Helper()
	app := newTestApp(t, false)
	recorder := &scaleRecorder{app: app}
	app.orchestrator = &kubeOrchestrator{runner: recorder, kubectl: "kubectl", helm: "helm", chart: "chart", imageRepository: "example/server"}
	if err := app.store.Update(func(state *State) error {
		state.Servers["world"] = Server{
			ID: "world", Name: "World", Namespace: "games", Release: "world",
			Status: StatusOnline, CurrentImage: "example/server:1", DesiredImage: "example/server:1",
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return app, recorder
}

func TestDemoStopThenStartKeepsInventory(t *testing.T) {
	app := newTestApp(t, true)
	id := "scuffedtards"
	res := requestJSON(t, app, http.MethodPost, "/api/servers/"+id+"/actions/stop", "")
	if res.Code != http.StatusOK {
		t.Fatalf("stop = %d %s", res.Code, res.Body.String())
	}
	var stopped Server
	if err := json.Unmarshal(res.Body.Bytes(), &stopped); err != nil {
		t.Fatal(err)
	}
	if stopped.ID != id || stopped.Status != StatusStopped {
		t.Fatalf("stopped = %+v", stopped)
	}
	if got := app.store.Snapshot().Servers[id]; got.ID != id || got.Status != StatusStopped {
		t.Fatalf("persisted stop = %+v", got)
	}
	res = requestJSON(t, app, http.MethodPost, "/api/servers/"+id+"/actions/start", "")
	if res.Code != http.StatusOK {
		t.Fatalf("start = %d %s", res.Code, res.Body.String())
	}
	var started Server
	if err := json.Unmarshal(res.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	if started.ID != id || started.Status != StatusOnline {
		t.Fatalf("started = %+v", started)
	}
	if _, ok := app.store.Snapshot().Servers[id]; !ok {
		t.Fatal("inventory row was removed")
	}
}

func TestStopIsIdempotentAndDoesNotDuplicateServerStopped(t *testing.T) {
	app := newTestApp(t, true)
	id := "scuffedtards"
	if res := requestJSON(t, app, http.MethodPost, "/api/servers/"+id+"/actions/stop", ""); res.Code != http.StatusOK {
		t.Fatalf("first stop = %d %s", res.Code, res.Body.String())
	}
	if res := requestJSON(t, app, http.MethodPost, "/api/servers/"+id+"/actions/stop", ""); res.Code != http.StatusOK {
		t.Fatalf("second stop = %d %s", res.Code, res.Body.String())
	}
	state := app.store.Snapshot()
	if state.Servers[id].Status != StatusStopped {
		t.Fatalf("status = %s", state.Servers[id].Status)
	}
	if got := eventKinds(&state); countKind(got, ServerStopped) != 1 {
		t.Fatalf("events = %v", got)
	}
}

func TestKubeStopPersistsBeforeScaleAndKeepsInventoryOnFailure(t *testing.T) {
	app, recorder := stopStartKubeApp(t)
	recorder.err = errors.New("scale exploded")
	res := requestJSON(t, app, http.MethodPost, "/api/servers/world/actions/stop", "")
	if res.Code != http.StatusBadGateway || !strings.Contains(res.Body.String(), "scale exploded") {
		t.Fatalf("failed stop = %d %s", res.Code, res.Body.String())
	}
	server := app.store.Snapshot().Servers["world"]
	if server.ID != "world" || server.Status != StatusStopped {
		t.Fatalf("inventory after failed scale = %+v", server)
	}
	if len(recorder.calls) != 1 || !strings.Contains(recorder.calls[0], "kubectl -n games scale deployment/world-rsdragonwilds --replicas=0") {
		t.Fatalf("scale args = %v", recorder.calls)
	}
	recorder.err = nil
	res = requestJSON(t, app, http.MethodPost, "/api/servers/world/actions/stop", "")
	if res.Code != http.StatusOK {
		t.Fatalf("retry stop = %d %s", res.Code, res.Body.String())
	}
	if len(recorder.calls) != 2 || !strings.Contains(recorder.calls[1], "--replicas=0") {
		t.Fatalf("retry scale args = %v", recorder.calls)
	}
	if countKind(eventKinds(ptr(app.store.Snapshot())), ServerStopped) != 1 {
		t.Fatalf("events = %v", eventKinds(ptr(app.store.Snapshot())))
	}
}

func TestKubeStartKeepsInventoryOnScaleFailure(t *testing.T) {
	app, recorder := stopStartKubeApp(t)
	if res := requestJSON(t, app, http.MethodPost, "/api/servers/world/actions/stop", ""); res.Code != http.StatusOK {
		t.Fatalf("stop = %d %s", res.Code, res.Body.String())
	}
	recorder.err = errors.New("start scale failed")
	res := requestJSON(t, app, http.MethodPost, "/api/servers/world/actions/start", "")
	if res.Code != http.StatusBadGateway || !strings.Contains(res.Body.String(), "start scale failed") {
		t.Fatalf("failed start = %d %s", res.Code, res.Body.String())
	}
	server := app.store.Snapshot().Servers["world"]
	if server.ID != "world" || server.Status != StatusStopped {
		t.Fatalf("inventory after failed start = %+v", server)
	}
	if res := requestJSON(t, app, http.MethodPost, "/api/servers/world/actions/restart", ""); res.Code != http.StatusConflict {
		t.Fatalf("restart after failed start = %d %s", res.Code, res.Body.String())
	}
	recorder.err = nil
	res = requestJSON(t, app, http.MethodPost, "/api/servers/world/actions/start", "")
	if res.Code != http.StatusOK {
		t.Fatalf("retry start = %d %s", res.Code, res.Body.String())
	}
	if !strings.Contains(recorder.calls[len(recorder.calls)-1], "--replicas=1") {
		t.Fatalf("retry scale args = %v", recorder.calls)
	}
	if countKind(eventKinds(ptr(app.store.Snapshot())), ServerStarted) != 1 {
		t.Fatalf("events = %v", eventKinds(ptr(app.store.Snapshot())))
	}
}

func TestKubeStartScalesToOneWithTheSameRelease(t *testing.T) {
	app, recorder := stopStartKubeApp(t)
	if res := requestJSON(t, app, http.MethodPost, "/api/servers/world/actions/stop", ""); res.Code != http.StatusOK {
		t.Fatalf("stop = %d %s", res.Code, res.Body.String())
	}
	res := requestJSON(t, app, http.MethodPost, "/api/servers/world/actions/start", "")
	if res.Code != http.StatusOK {
		t.Fatalf("start = %d %s", res.Code, res.Body.String())
	}
	var started Server
	if err := json.Unmarshal(res.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	if started.ID != "world" || started.Status != StatusStarting || started.Release != "world" {
		t.Fatalf("started = %+v", started)
	}
	if len(recorder.calls) < 2 || !strings.Contains(recorder.calls[len(recorder.calls)-1], "kubectl -n games scale deployment/world-rsdragonwilds --replicas=1") {
		t.Fatalf("scale args = %v", recorder.calls)
	}
}

func TestKubeScaleRejectsReplicaCountsOtherThanZeroOrOne(t *testing.T) {
	recorder := &scaleRecorder{}
	k := &kubeOrchestrator{runner: recorder, kubectl: "kubectl"}
	if err := k.Scale(context.Background(), Server{Namespace: "games", Release: "world"}, 2); err == nil || !strings.Contains(err.Error(), "0 or 1") {
		t.Fatalf("err = %v", err)
	}
	if len(recorder.calls) != 0 {
		t.Fatalf("kubectl ran = %v", recorder.calls)
	}
}

func TestKubeDeployRefusesStoppedServer(t *testing.T) {
	recorder := &scaleRecorder{}
	k := &kubeOrchestrator{runner: recorder, helm: "helm", kubectl: "kubectl", chart: "chart", imageRepository: "example/server"}
	err := k.Deploy(context.Background(), Server{ID: "world", Name: "World", Namespace: "games", Release: "world", Status: StatusStopped, DesiredImage: "example/server:1"})
	if !errors.Is(err, errServerStopped) {
		t.Fatalf("err = %v", err)
	}
	if len(recorder.calls) != 0 {
		t.Fatalf("helm ran = %v", recorder.calls)
	}
}

func TestStoppedServerBlocksLifecycleActions(t *testing.T) {
	app, recorder := stopStartKubeApp(t)
	if res := requestJSON(t, app, http.MethodPost, "/api/servers/world/actions/stop", ""); res.Code != http.StatusOK {
		t.Fatalf("stop = %d %s", res.Code, res.Body.String())
	}
	calls := len(recorder.calls)
	for _, tc := range []struct {
		method, path, body string
	}{
		{http.MethodPost, "/api/servers/world/actions/restart", ""},
		{http.MethodPost, "/api/servers/world/actions/update", `{"imageTag":"2"}`},
		{http.MethodPost, "/api/servers/world/actions/check-update", ""},
		{http.MethodPost, "/api/servers/world/actions/edit-settings", `{"name":"Edited","confirm":true}`},
		{http.MethodGet, "/api/servers/world/logs", ""},
	} {
		res := requestJSON(t, app, tc.method, tc.path, tc.body)
		if res.Code != http.StatusConflict || !strings.Contains(res.Body.String(), errServerStopped.Error()) {
			t.Fatalf("%s %s = %d %s", tc.method, tc.path, res.Code, res.Body.String())
		}
	}
	if len(recorder.calls) != calls {
		t.Fatalf("blocked actions touched the cluster: %v", recorder.calls[calls:])
	}
}

func TestStopClearsOutageWithoutServerRecovered(t *testing.T) {
	app := newTestApp(t, true)
	id := "scuffedtards"
	if err := app.store.Update(func(state *State) error {
		state.Producers[id] = AlertProducer{HealthyBaseline: true, Outage: true, Streak: "unhealthy", StreakCount: 3, PlayerAt: time.Now().UTC()}
		state.Deliveries = map[string]Delivery{
			"pending-down": {ID: "pending-down", Event: Event{ServerID: id, Kind: ServerDown}, Status: DeliveryPending},
			"sent-down":    {ID: "sent-down", Event: Event{ServerID: id, Kind: ServerDown}, Status: DeliverySent},
			"pending-join": {ID: "pending-join", Event: Event{ServerID: id, Kind: PlayerJoined}, Status: DeliveryPending},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if res := requestJSON(t, app, http.MethodPost, "/api/servers/"+id+"/actions/stop", ""); res.Code != http.StatusOK {
		t.Fatalf("stop = %d %s", res.Code, res.Body.String())
	}
	state := app.store.Snapshot()
	p := state.Producers[id]
	if p.Outage || p.HealthyBaseline || p.Streak != "" || p.StreakCount != 0 || !p.PlayerAt.IsZero() {
		t.Fatalf("producer = %+v", p)
	}
	if slices.Contains(eventKinds(&state), ServerRecovered) || slices.Contains(eventKinds(&state), ServerDown) {
		t.Fatalf("events = %v", eventKinds(&state))
	}
	if state.Deliveries["pending-down"].Status != DeliveryFailed || !strings.Contains(state.Deliveries["pending-down"].Result, "stopped") || strings.Contains(state.Deliveries["pending-down"].Result, "deletion") {
		t.Fatalf("down delivery = %+v", state.Deliveries["pending-down"])
	}
	if state.Deliveries["sent-down"].Status != DeliverySent {
		t.Fatalf("sent delivery changed: %+v", state.Deliveries["sent-down"])
	}
	if state.Deliveries["pending-join"].Status != DeliveryPending {
		t.Fatalf("unrelated delivery cancelled: %+v", state.Deliveries["pending-join"])
	}
}

func TestObserveAlertsSkipsParkedServers(t *testing.T) {
	s, server, now := alertFixture(t)
	server.Status = StatusStopped
	s.Servers[server.ID] = server
	s.Producers[server.ID] = AlertProducer{HealthyBaseline: true, LastAt: now.Add(-time.Minute)}
	for _, offset := range []int{0, 15, 30, 45} {
		at := now.Add(time.Duration(offset) * time.Second)
		observeAlerts(s, server, alertSample(at, "unhealthy", "runtime", -1), at)
	}
	if len(s.Events) != 0 {
		t.Fatalf("parked server emitted %v", eventKinds(s))
	}
}

func TestStopStartAuthorizationAndCapabilities(t *testing.T) {
	app, issuer := oidcTestApp(t)
	viewer, viewerCSRF := loginAs(t, app, issuer, "viewer")
	admin, adminCSRF := loginAs(t, app, issuer, "admin")
	for _, action := range []string{"stop", "start"} {
		path := "/api/servers/scuffedtards/actions/" + action
		if res := authRequest(app, http.MethodPost, path, viewer, app.auth.settings.Origin, viewerCSRF, ""); res.Code != http.StatusForbidden {
			t.Fatalf("viewer %s = %d %s", action, res.Code, res.Body.String())
		}
		if res := authRequest(app, http.MethodPost, path, nil, app.auth.settings.Origin, "", ""); res.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous %s = %d", action, res.Code)
		}
		if res := authRequest(app, http.MethodPost, path, admin, app.auth.settings.Origin, "wrong", ""); res.Code != http.StatusForbidden {
			t.Fatalf("bad CSRF %s = %d", action, res.Code)
		}
	}
	if capabilities(RoleViewer)["stop"] || capabilities(RoleViewer)["start"] || !capabilities(RoleAdmin)["stop"] || !capabilities(RoleAdmin)["start"] {
		t.Fatalf("capabilities viewer=%v admin=%v", capabilities(RoleViewer), capabilities(RoleAdmin))
	}
	if res := authRequest(app, http.MethodPost, "/api/servers/scuffedtards/actions/stop", admin, app.auth.settings.Origin, adminCSRF, ""); res.Code != http.StatusOK {
		t.Fatalf("admin stop = %d %s", res.Code, res.Body.String())
	}
}

func TestStopConflictsWithPendingRestart(t *testing.T) {
	app, recorder := stopStartKubeApp(t)
	if err := app.store.Update(func(state *State) error {
		p := state.Producers["world"]
		p.Restart = &RestartOperation{ID: "op", RequestedAt: time.Now().UTC()}
		state.Producers["world"] = p
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	res := requestJSON(t, app, http.MethodPost, "/api/servers/world/actions/stop", "")
	if res.Code != http.StatusConflict || !strings.Contains(res.Body.String(), "a restart is already awaiting reconciliation") {
		t.Fatalf("stop = %d %s", res.Code, res.Body.String())
	}
	if app.store.Snapshot().Servers["world"].Status == StatusStopped {
		t.Fatal("pending restart was parked")
	}
	if len(recorder.calls) != 0 {
		t.Fatalf("scale ran = %v", recorder.calls)
	}
}

func TestStartHealsWithoutDuplicateServerStarted(t *testing.T) {
	app := newTestApp(t, true)
	id := "scuffedtards"
	if res := requestJSON(t, app, http.MethodPost, "/api/servers/"+id+"/actions/start", ""); res.Code != http.StatusOK {
		t.Fatalf("heal start = %d %s", res.Code, res.Body.String())
	}
	if slices.Contains(eventKinds(ptr(app.store.Snapshot())), ServerStarted) {
		t.Fatal("heal emitted ServerStarted")
	}
	if res := requestJSON(t, app, http.MethodPost, "/api/servers/"+id+"/actions/stop", ""); res.Code != http.StatusOK {
		t.Fatal(res.Body.String())
	}
	if res := requestJSON(t, app, http.MethodPost, "/api/servers/"+id+"/actions/start", ""); res.Code != http.StatusOK {
		t.Fatal(res.Body.String())
	}
	if res := requestJSON(t, app, http.MethodPost, "/api/servers/"+id+"/actions/start", ""); res.Code != http.StatusOK {
		t.Fatal(res.Body.String())
	}
	if got := countKind(eventKinds(ptr(app.store.Snapshot())), ServerStarted); got != 1 {
		t.Fatalf("ServerStarted count = %d events=%v", got, eventKinds(ptr(app.store.Snapshot())))
	}
}

func TestSkipsAutomatedRestartsReasons(t *testing.T) {
	var state State
	state.Servers = map[string]Server{"parked": {ID: "parked", Status: StatusStopped}, "live": {ID: "live", Status: StatusOnline}}
	state.Deletions = map[string]deletionRecord{"gone": {ServerID: "gone"}}
	if skip, reason := state.skipsAutomatedRestarts("gone"); !skip || reason != "Target server deletion is in progress or recorded" {
		t.Fatalf("deleting = %v %q", skip, reason)
	}
	if skip, reason := state.skipsAutomatedRestarts("parked"); !skip || reason != "Target server is stopped" {
		t.Fatalf("stopped = %v %q", skip, reason)
	}
	if skip, reason := state.skipsAutomatedRestarts("live"); skip || reason != "" {
		t.Fatalf("live = %v %q", skip, reason)
	}
}

func ptr[T any](v T) *T { return &v }

func countKind(kinds []EventKind, want EventKind) int {
	n := 0
	for _, kind := range kinds {
		if kind == want {
			n++
		}
	}
	return n
}
