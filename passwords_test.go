package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeAdminIDs(t *testing.T) {
	valid := "0123456789abcdef0123456789abcdef"
	other := "ABCDEF0123456789ABCDEF0123456789"
	got, err := normalizeAdminIDs("  " + valid + "," + other + "," + valid + ",")
	if err != nil {
		t.Fatal(err)
	}
	if got != valid+","+strings.ToLower(other) {
		t.Fatalf("normalized IDs = %q", got)
	}
	if got, err := normalizeAdminIDs(""); err != nil || got != "" {
		t.Fatalf("empty IDs = %q, %v", got, err)
	}
	if _, err := normalizeAdminIDs("not-an-eos-id"); err == nil {
		t.Fatal("invalid administrator ID accepted")
	}
}

func TestEditSettingsPasswordIntentIsTransient(t *testing.T) {
	app := newTestApp(t, false)
	orchestrator := &editOrchestrator{}
	app.orchestrator = orchestrator
	original := editFixtureServer()
	if err := app.store.Update(func(state *State) error { state.Servers[original.ID] = original; return nil }); err != nil {
		t.Fatal(err)
	}
	serverPassword := "join secret"
	adminPassword := "admin secret"
	res := requestJSON(t, app, http.MethodPost, "/api/servers/target/actions/edit-settings", fmt.Sprintf(`{"serverPassword":%q,"adminPassword":%q,"adminIds":"0123456789abcdef0123456789abcdef,ABCDEF0123456789ABCDEF0123456789","confirm":true}`, serverPassword, adminPassword))
	if res.Code != http.StatusOK {
		t.Fatalf("edit status = %d: %s", res.Code, res.Body.String())
	}
	if strings.Contains(res.Body.String(), serverPassword) || strings.Contains(res.Body.String(), adminPassword) {
		t.Fatal("password returned in edit response")
	}
	if len(orchestrator.deployed) != 1 || orchestrator.deployed[0].PasswordUpdate == nil {
		t.Fatalf("password intent was not sent to orchestrator: %+v", orchestrator.deployed)
	}
	intent := orchestrator.deployed[0].PasswordUpdate
	if intent.ServerPassword == nil || *intent.ServerPassword != serverPassword || intent.AdminPassword == nil || *intent.AdminPassword != adminPassword || intent.Revision == "" {
		t.Fatalf("password intent = %+v", intent)
	}
	got := app.store.Snapshot().Servers[original.ID]
	if got.PasswordUpdate != nil || got.ServerPassword != "" || got.AdminPassword != "" || got.AdminIDs != "0123456789abcdef0123456789abcdef,abcdef0123456789abcdef0123456789" {
		t.Fatalf("transient credential state persisted: %+v", got)
	}
	data, err := os.ReadFile(app.store.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), serverPassword) || strings.Contains(string(data), adminPassword) {
		t.Fatal("password written to state")
	}
}

func TestEditSettingsRejectsInvalidCredentialInputs(t *testing.T) {
	app := newTestApp(t, false)
	orchestrator := &editOrchestrator{}
	app.orchestrator = orchestrator
	server := editFixtureServer()
	if err := app.store.Update(func(state *State) error { state.Servers[server.ID] = server; return nil }); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"serverPassword":"line\nbreak","confirm":true}`,
		`{"adminPassword":"line\rbreak","confirm":true}`,
		`{"adminIds":"not-an-eos-id","confirm":true}`,
		fmt.Sprintf(`{"serverPassword":%q,"confirm":true}`, strings.Repeat("x", 2049)),
	} {
		before := app.store.Snapshot()
		res := requestJSON(t, app, http.MethodPost, "/api/servers/target/actions/edit-settings", body)
		if res.Code != http.StatusBadRequest {
			t.Fatalf("invalid credential status = %d: %s", res.Code, res.Body.String())
		}
		if len(orchestrator.deployed) != 0 || !reflect.DeepEqual(before, app.store.Snapshot()) {
			t.Fatal("invalid credential request changed state")
		}
	}
}

type passwordRunner struct {
	calls           []string
	patchDocuments  [][]byte
	createDocuments [][]byte
	secretPresent   bool
	secretOwner     string
	passwordOwner   string
}

func (r *passwordRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := strings.Join(append([]string{name}, args...), " ")
	r.calls = append(r.calls, call)
	if strings.Contains(call, " create ") && len(args) > 0 && strings.HasSuffix(args[len(args)-1], ".json") {
		data, err := os.ReadFile(args[len(args)-1])
		if err != nil {
			return nil, err
		}
		r.createDocuments = append(r.createDocuments, data)
	}
	switch {
	case strings.Contains(call, "get Secret target-api"):
		return r.secretJSON("target-api", "api-uid", r.secretOwner, map[string]string{"token": "api-token"}), nil
	case strings.Contains(call, "get Secret target-settings"):
		if !r.secretPresent {
			return nil, nil
		}
		owner := r.passwordOwner
		if owner == "" {
			owner = r.secretOwner
		}
		return r.secretJSON("target-settings", "settings-uid", owner, map[string]string{
			"serverPassword": base64.StdEncoding.EncodeToString([]byte("old join")),
			"adminPassword":  base64.StdEncoding.EncodeToString([]byte("old admin")),
		}), nil
	case strings.Contains(call, "--patch-file"):
		path := args[len(args)-1]
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		r.patchDocuments = append(r.patchDocuments, data)
		return []byte("ok"), nil
	case strings.Contains(call, "get namespace"):
		return []byte("ok"), nil
	default:
		return []byte("ok"), nil
	}
}

func (r *passwordRunner) secretJSON(name, uid, owner string, data map[string]string) []byte {
	resource := map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name": name, "namespace": "games", "uid": uid, "resourceVersion": "rv-" + uid,
			"annotations": map[string]string{ownershipAnnotation: owner},
		},
		"data": data,
	}
	encoded, _ := json.Marshal(resource)
	return encoded
}

func TestDeployPatchesOnlyRequestedPasswordKeyAndRollsOut(t *testing.T) {
	runner := &passwordRunner{secretPresent: true, secretOwner: "owner-token"}
	kube := &kubeOrchestrator{runner: runner, helm: "helm", kubectl: "kubectl", chart: "chart", imageRepository: "example/server"}
	newPassword := "new join"
	server := editFixtureServer()
	server.PasswordUpdate = &PasswordUpdate{ServerPassword: &newPassword, Revision: "credential-revision"}
	if err := kube.Deploy(context.Background(), server); err != nil {
		t.Fatal(err)
	}
	if len(runner.patchDocuments) != 1 {
		t.Fatalf("patch documents = %d, calls = %v", len(runner.patchDocuments), runner.calls)
	}
	var patch struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(runner.patchDocuments[0], &patch); err != nil {
		t.Fatal(err)
	}
	if patch.Data["serverPassword"] != base64.StdEncoding.EncodeToString([]byte(newPassword)) {
		t.Fatalf("server password patch = %+v", patch.Data)
	}
	if patch.Metadata.ResourceVersion != "rv-settings-uid" || !strings.Contains(strings.Join(runner.calls, " "), "-n games patch secret/target-settings") {
		t.Fatalf("Secret patch lacks namespace or resource version: metadata=%+v calls=%v", patch.Metadata, runner.calls)
	}
	if _, ok := patch.Data["adminPassword"]; ok {
		t.Fatalf("omitted admin password was patched: %+v", patch.Data)
	}
	joined := strings.Join(runner.calls, " ")
	if strings.Contains(joined, newPassword) || strings.Contains(joined, "old join") || strings.Contains(joined, "old admin") || !strings.Contains(joined, "credential-revision") {
		t.Fatalf("secret material or rollout revision leaked in command trace: %s", joined)
	}
}

func TestDeployRejectsForeignPasswordSecret(t *testing.T) {
	runner := &passwordRunner{secretPresent: true, secretOwner: "owner-token", passwordOwner: "foreign-owner"}
	kube := &kubeOrchestrator{runner: runner, helm: "helm", kubectl: "kubectl", chart: "chart", imageRepository: "example/server"}
	newPassword := "new join"
	server := editFixtureServer()
	server.PasswordUpdate = &PasswordUpdate{ServerPassword: &newPassword, Revision: "credential-revision"}
	if err := kube.Deploy(context.Background(), server); err == nil || !strings.Contains(err.Error(), "ownership") {
		t.Fatalf("foreign Secret error = %v", err)
	}
	for _, call := range runner.calls {
		if strings.HasPrefix(call, "helm ") {
			t.Fatal("Helm ran after foreign Secret rejection")
		}
	}
}

func TestDeployCreatesEmptyKeysWhenClearingMissingPasswordSecret(t *testing.T) {
	runner := &passwordRunner{secretOwner: "owner-token"}
	kube := &kubeOrchestrator{runner: runner, helm: "helm", kubectl: "kubectl", chart: "chart", imageRepository: "example/server"}
	empty := ""
	server := editFixtureServer()
	server.PasswordUpdate = &PasswordUpdate{ServerPassword: &empty, AdminPassword: &empty, Revision: "credential-revision"}
	server.PasswordSecret = "target-settings"
	if err := kube.Deploy(context.Background(), server); err != nil {
		t.Fatal(err)
	}
	if len(runner.patchDocuments) != 0 {
		t.Fatal("missing Secret used patch instead of create")
	}
	var createDocument struct {
		StringData map[string]string `json:"stringData"`
	}
	for _, data := range runner.createDocuments {
		if err := json.Unmarshal(data, &createDocument); err != nil {
			t.Fatal(err)
		}
	}
	if createDocument.StringData["serverPassword"] != "" || createDocument.StringData["adminPassword"] != "" {
		t.Fatalf("created Secret keys = %+v", createDocument.StringData)
	}
}
