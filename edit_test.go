package main

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

type editOrchestrator struct {
	demoOrchestrator
	deployed      []Server
	err           error
	observedImage string
	refreshErr    error
}

func (o *editOrchestrator) Deploy(_ context.Context, server Server) error {
	o.deployed = append(o.deployed, server)
	return o.err
}

func (o *editOrchestrator) Refresh(_ context.Context, server Server) (Server, error) {
	if o.refreshErr != nil {
		return server, o.refreshErr
	}
	if o.observedImage != "" {
		server.CurrentImage = o.observedImage
	}
	return server, nil
}

func editFixtureServer() Server {
	return Server{
		ID: "target", Name: "Original creator", Namespace: "games", Release: "target", Region: "us-east", OwnerID: "0123456789abcdef0123456789abcdef",
		OwnershipToken: "owner-token", PasswordSecret: "target-settings", DesiredImage: "example/server:1.2.3", CurrentImage: "example/server:1.2.3",
		Status: StatusOnline, MaxPlayers: 4, MemoryLimitMiB: 1024, CPULimitMillis: 500, Endpoint: "target.example:7780",
		ServerSettings: ServerSettings{WorldName: "Original world", GamePort: 7780, StorageGiB: 120, ServiceType: "NodePort", AdminIDs: "admin-1", AdditionalArgs: "-log", DebugLevel: 3, AutoStopOnUpdate: true, ValidateGameFiles: true},
		SaveSeed:       &SaveSeed{Claim: "uploaded-seed", Path: "/seed/World.sav"},
	}
}

func TestEditSettingsPreservesIdentityAndUneditedFields(t *testing.T) {
	app := newTestApp(t, false)
	orchestrator := &editOrchestrator{}
	app.orchestrator = orchestrator
	original := editFixtureServer()
	if err := app.store.Update(func(state *State) error { state.Servers[original.ID] = original; return nil }); err != nil {
		t.Fatal(err)
	}

	res := requestJSON(t, app, http.MethodPost, "/api/servers/target/actions/edit-settings", `{"name":"Edited creator","worldName":"Edited world","maxPlayers":8,"memoryLimitMiB":2048,"cpuLimitMillis":750,"confirm":true,"confirmWorldName":true}`)
	if res.Code != http.StatusOK {
		t.Fatalf("edit status = %d: %s", res.Code, res.Body.String())
	}
	got := app.store.Snapshot().Servers[original.ID]
	want := original
	want.Name = "Edited creator"
	want.WorldName = "Edited world"
	want.MaxPlayers = 8
	want.MemoryLimitMiB = 2048
	want.CPULimitMillis = 750
	want.Status = StatusStarting
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("edited server changed more than requested:\n got  %+v\n want %+v", got, want)
	}
	if len(orchestrator.deployed) != 1 || orchestrator.deployed[0].ID != original.ID || orchestrator.deployed[0].Release != original.Release {
		t.Fatalf("deploy target = %+v", orchestrator.deployed)
	}
	if events := app.store.Snapshot().Events; len(events) != 1 || events[0].Message != "Settings apply requested" || !strings.Contains(events[0].Details, "readiness is not verified") {
		t.Fatalf("settings event = %+v", events)
	}
}

func TestEditSettingsCreatorChangeMaterializesLegacyWorldFallback(t *testing.T) {
	app := newTestApp(t, false)
	orchestrator := &editOrchestrator{}
	app.orchestrator = orchestrator
	original := editFixtureServer()
	original.WorldName = ""
	if err := app.store.Update(func(state *State) error { state.Servers[original.ID] = original; return nil }); err != nil {
		t.Fatal(err)
	}

	res := requestJSON(t, app, http.MethodPost, "/api/servers/target/actions/edit-settings", `{"name":"New creator","confirm":true}`)
	if res.Code != http.StatusOK {
		t.Fatalf("edit status = %d: %s", res.Code, res.Body.String())
	}
	got := app.store.Snapshot().Servers[original.ID]
	if got.Name != "New creator" || got.WorldName != original.Name || len(orchestrator.deployed) != 1 || orchestrator.deployed[0].WorldName != original.Name {
		t.Fatalf("legacy fallback changed effective world: persisted=%+v deployed=%+v", got, orchestrator.deployed)
	}
}

func TestEditSettingsRejectsInvalidOrImmutableRequests(t *testing.T) {
	app := newTestApp(t, false)
	orchestrator := &editOrchestrator{}
	app.orchestrator = orchestrator
	original := editFixtureServer()
	if err := app.store.Update(func(state *State) error { state.Servers[original.ID] = original; return nil }); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, body string
	}{
		{"missing confirmation", `{"name":"new"}`},
		{"empty patch", `{"confirm":true}`},
		{"unknown owner field", `{"ownerId":"11111111111111111111111111111111","confirm":true}`},
		{"invalid player limit", `{"maxPlayers":0,"confirm":true}`},
		{"invalid memory limit", `{"memoryLimitMiB":67585,"confirm":true}`},
		{"world name requires explicit confirmation", `{"worldName":"New world","confirm":true}`},
		{"invalid CPU limit", `{"cpuLimitMillis":99,"confirm":true}`},
		{"blank creator", `{"name":"  ","confirm":true}`},
		{"trailing JSON", `{"name":"new","confirm":true}{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := app.store.Snapshot()
			res := requestJSON(t, app, http.MethodPost, "/api/servers/target/actions/edit-settings", tc.body)
			if res.Code != http.StatusBadRequest {
				t.Fatalf("status = %d: %s", res.Code, res.Body.String())
			}
			if !reflect.DeepEqual(before, app.store.Snapshot()) || len(orchestrator.deployed) != 0 {
				t.Fatal("invalid edit changed state or called the orchestrator")
			}
		})
	}
}

func TestEditSettingsReportsHelmFailureWithoutPersisting(t *testing.T) {
	app := newTestApp(t, false)
	orchestrator := &editOrchestrator{err: errors.New("helm failed")}
	app.orchestrator = orchestrator
	original := editFixtureServer()
	if err := app.store.Update(func(state *State) error { state.Servers[original.ID] = original; return nil }); err != nil {
		t.Fatal(err)
	}
	before := app.store.Snapshot()
	res := requestJSON(t, app, http.MethodPost, "/api/servers/target/actions/edit-settings", `{"name":"Edited","confirm":true}`)
	if res.Code != http.StatusBadGateway || !strings.Contains(res.Body.String(), "Cluster outcome may be uncertain") || !strings.Contains(res.Body.String(), "games/target") {
		t.Fatalf("failure response = %d: %s", res.Code, res.Body.String())
	}
	if !reflect.DeepEqual(before, app.store.Snapshot()) || len(orchestrator.deployed) != 1 {
		t.Fatal("Helm failure persisted settings or skipped deployment")
	}
}

func TestEditSettingsUsesObservedImageAndPreservesPendingIntent(t *testing.T) {
	app := newTestApp(t, false)
	orchestrator := &editOrchestrator{observedImage: "example/server:1.2.4"}
	app.orchestrator = orchestrator
	server := editFixtureServer()
	server.DesiredImage = "example/server:2.0.0"
	if err := app.store.Update(func(state *State) error { state.Servers[server.ID] = server; return nil }); err != nil {
		t.Fatal(err)
	}
	res := requestJSON(t, app, http.MethodPost, "/api/servers/target/actions/edit-settings", `{"name":"Edited","confirm":true}`)
	if res.Code != http.StatusOK {
		t.Fatalf("edit status = %d: %s", res.Code, res.Body.String())
	}
	if len(orchestrator.deployed) != 1 || orchestrator.deployed[0].DesiredImage != "example/server:1.2.4" {
		t.Fatalf("settings rollout image = %+v", orchestrator.deployed)
	}
	got := app.store.Snapshot().Servers[server.ID]
	if got.CurrentImage != "example/server:1.2.4" || got.DesiredImage != "example/server:2.0.0" || !got.UpdateAvailable {
		t.Fatalf("image state = current %q desired %q available %t", got.CurrentImage, got.DesiredImage, got.UpdateAvailable)
	}
}

func TestEditSettingsReportsImageLookupFailureWithoutDeploying(t *testing.T) {
	app := newTestApp(t, false)
	orchestrator := &editOrchestrator{refreshErr: errors.New("server is not ready")}
	app.orchestrator = orchestrator
	original := editFixtureServer()
	if err := app.store.Update(func(state *State) error { state.Servers[original.ID] = original; return nil }); err != nil {
		t.Fatal(err)
	}
	before := app.store.Snapshot()
	res := requestJSON(t, app, http.MethodPost, "/api/servers/target/actions/edit-settings", `{"name":"Edited","confirm":true}`)
	if res.Code != http.StatusBadGateway || !strings.Contains(res.Body.String(), "currently running image") || !strings.Contains(res.Body.String(), "Settings were not applied") {
		t.Fatalf("image lookup response = %d: %s", res.Code, res.Body.String())
	}
	if !reflect.DeepEqual(before, app.store.Snapshot()) || len(orchestrator.deployed) != 0 {
		t.Fatal("image lookup failure changed state or deployed settings")
	}
}

func TestEditSettingsAcceptsCreationMemoryCeiling(t *testing.T) {
	app := newTestApp(t, false)
	orchestrator := &editOrchestrator{}
	app.orchestrator = orchestrator
	server := editFixtureServer()
	server.MemoryLimitMiB = 67584
	if err := app.store.Update(func(state *State) error { state.Servers[server.ID] = server; return nil }); err != nil {
		t.Fatal(err)
	}
	res := requestJSON(t, app, http.MethodPost, "/api/servers/target/actions/edit-settings", `{"name":"Edited","confirm":true}`)
	if res.Code != http.StatusOK || app.store.Snapshot().Servers[server.ID].MemoryLimitMiB != 67584 {
		t.Fatalf("maximum memory edit = %d: %s", res.Code, res.Body.String())
	}
}

func TestEditSettingsReportsPersistenceFailureAfterDeploy(t *testing.T) {
	app := newTestApp(t, false)
	orchestrator := &editOrchestrator{}
	app.orchestrator = orchestrator
	original := editFixtureServer()
	if err := app.store.Update(func(state *State) error { state.Servers[original.ID] = original; return nil }); err != nil {
		t.Fatal(err)
	}
	before := app.store.Snapshot()
	app.store.path = t.TempDir()
	res := requestJSON(t, app, http.MethodPost, "/api/servers/target/actions/edit-settings", `{"name":"Edited","confirm":true}`)
	if res.Code != http.StatusInternalServerError || !strings.Contains(res.Body.String(), "cluster may already contain") || !strings.Contains(res.Body.String(), "games/target") {
		t.Fatalf("persistence response = %d: %s", res.Code, res.Body.String())
	}
	if !reflect.DeepEqual(before, app.store.Snapshot()) || len(orchestrator.deployed) != 1 {
		t.Fatal("persistence failure did not preserve C2 state or deploy once")
	}
}

func TestEditSettingsHelmValuesUseExistingRelease(t *testing.T) {
	app := newTestApp(t, false)
	runner := &recordingRunner{}
	app.orchestrator = &editKubeOrchestrator{kubeOrchestrator: &kubeOrchestrator{runner: runner, helm: "helm", kubectl: "kubectl", chart: "chart", imageRepository: "example/server"}}
	server := editFixtureServer()
	server.OwnershipToken = ""
	server.DesiredImage = "example/server:2.0.0"
	if err := app.store.Update(func(state *State) error { state.Servers[server.ID] = server; return nil }); err != nil {
		t.Fatal(err)
	}
	res := requestJSON(t, app, http.MethodPost, "/api/servers/target/actions/edit-settings", `{"name":"Edited creator","worldName":"Edited world","maxPlayers":9,"memoryLimitMiB":3072,"cpuLimitMillis":1250,"confirm":true,"confirmWorldName":true}`)
	if res.Code != http.StatusOK {
		t.Fatalf("edit status = %d: %s", res.Code, res.Body.String())
	}
	var helmCall string
	for _, call := range runner.calls {
		if strings.HasPrefix(call, "helm ") {
			helmCall = call
		}
	}
	for _, want := range []string{"upgrade --install target chart", "image.tag=1.2.3", "RSDW_SERVER_NAME=Edited creator", "RSDW_WORLD_NAME=Edited world", "MaxPlayers=9", "resources.limits.memory=3072Mi", "resources.limits.cpu=1250m"} {
		if !strings.Contains(helmCall, want) {
			t.Fatalf("Helm call missing %q: %s", want, helmCall)
		}
	}
	if strings.Contains(helmCall, "target-new") || !strings.Contains(helmCall, "persistence.size=120Gi") || !strings.Contains(helmCall, "server.port=7780") {
		t.Fatalf("edit changed release or immutable deployment settings: %s", helmCall)
	}
	if got := app.store.Snapshot().Servers[server.ID].DesiredImage; got != "example/server:2.0.0" {
		t.Fatalf("edit changed pending image intent: %q", got)
	}
}

func TestOIDCEditSettingsRequiresAdminAndCSRF(t *testing.T) {
	app, issuer := oidcTestApp(t)
	orchestrator := &countingOrchestrator{}
	app.orchestrator = orchestrator
	viewer, viewerCSRF := loginAs(t, app, issuer, "viewer")
	body := `{"name":"Viewer cannot edit","confirm":true}`
	if res := authRequest(app, http.MethodPost, "/api/servers/scuffedtards/actions/edit-settings", viewer, app.auth.settings.Origin, viewerCSRF, body); res.Code != http.StatusForbidden {
		t.Fatalf("viewer edit status = %d: %s", res.Code, res.Body.String())
	}
	if res := authRequest(app, http.MethodPost, "/api/servers/scuffedtards/actions/edit-settings", nil, app.auth.settings.Origin, "", body); res.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous edit status = %d: %s", res.Code, res.Body.String())
	}
	admin, adminCSRF := loginAs(t, app, issuer, "admin")
	if res := authRequest(app, http.MethodPost, "/api/servers/scuffedtards/actions/edit-settings", admin, app.auth.settings.Origin, "wrong", body); res.Code != http.StatusForbidden {
		t.Fatalf("bad CSRF edit status = %d: %s", res.Code, res.Body.String())
	}
	if res := authRequest(app, http.MethodPost, "/api/servers/scuffedtards/actions/edit-settings", admin, app.auth.settings.Origin, adminCSRF, body); res.Code != http.StatusOK {
		t.Fatalf("admin edit status = %d: %s", res.Code, res.Body.String())
	}
	if orchestrator.calls != 2 {
		t.Fatalf("orchestrator calls = %d, want 2 for image lookup and deploy", orchestrator.calls)
	}
}

type editKubeOrchestrator struct{ *kubeOrchestrator }

func (o *editKubeOrchestrator) Refresh(_ context.Context, server Server) (Server, error) {
	return server, nil
}
