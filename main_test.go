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
	"time"
)

type registryTransport func(*http.Request) (*http.Response, error)

func (f registryTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCheckUpdateRegistryBehavior(t *testing.T) {
	for _, tc := range []struct {
		name, tokenBody, tagsBody           string
		tokenStatus, tagsStatus, wantStatus int
	}{
		{"authenticated", `{"token":"registry-secret"}`, `{"tags":["1.2.3","latest","1.2.4","nightly","1.3.0-rc1"]}`, 200, 200, 200},
		{"access token", `{"access_token":"registry-secret"}`, `{"tags":["1.2.3","latest","1.2.4","nightly","1.3.0-rc1"]}`, 200, 200, 200},
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
			t.Setenv("RSDW_IMAGE_REPOSITORY", "ghcr.io/example/server")
			list := requestJSON(t, app, http.MethodGet, "/api/image-tags", "")
			if list.Code != tc.wantStatus {
				t.Fatalf("list status = %d: %s", list.Code, list.Body.String())
			}
			if tc.wantStatus == 200 && strings.TrimSpace(list.Body.String()) != `["1.2.4","1.2.3"]` {
				t.Fatalf("tags = %s", list.Body.String())
			}
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

func TestUpdateReturnsObservedImage(t *testing.T) {
	t.Setenv("RSDW_IMAGE_REPOSITORY", "example/server")
	for _, tc := range []struct {
		name, observed, wantImage string
		demo                      bool
	}{
		{"observed", "example/server:1", "example/server:1", false},
		{"unobserved", "", "", false},
		{"demo", "", "example/server:2", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := newTestApp(t, tc.demo)
			server := Server{ID: "world", Release: "world", CurrentImage: "example/server:stored", DesiredImage: "example/server:stored"}
			if err := app.store.Update(func(s *State) error { s.Servers[server.ID] = server; return nil }); err != nil {
				t.Fatal(err)
			}
			observedAt := time.Now().UTC().Add(-time.Second)
			if tc.observed != "" {
				app.observations().history[server.ID] = []observation{{at: observedAt, image: tc.observed, status: StatusOnline, metrics: emptyMetrics()}}
			}
			res := requestJSON(t, app, http.MethodPost, "/api/servers/world/actions/update", `{"imageTag":"2"}`)
			if res.Code != http.StatusOK {
				t.Fatalf("update = %d: %s", res.Code, res.Body.String())
			}
			var got Server
			if err := json.Unmarshal(res.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.CurrentImage != tc.wantImage || got.DesiredImage != "example/server:2" || got.Status != StatusStarting {
				t.Fatalf("update current=%q desired=%q status=%s", got.CurrentImage, got.DesiredImage, got.Status)
			}
			if !tc.demo {
				wantSeen := ""
				if tc.observed != "" {
					wantSeen = observedAt.Format(time.RFC3339Nano)
				}
				if got.LastSeen != wantSeen || got.UpdateAvailable != (tc.observed != "") {
					t.Fatalf("update lastSeen=%q updateAvailable=%t", got.LastSeen, got.UpdateAvailable)
				}
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

func TestShellRunnerRejectsOutputOverflow(t *testing.T) {
	for _, tc := range []struct {
		name, redirect string
	}{
		{"stdout", ""},
		{"stderr", " >&2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := (shellRunner{}).Run(ctx, "sh", "-c", "while :; do printf 0123456789abcdef"+tc.redirect+"; done")
			if !errors.Is(err, errCommandOutputLimit) {
				t.Fatalf("overflow error = %v", err)
			}
		})
	}
}

type secretFailureRunner struct{ token string }

func (r *secretFailureRunner) Run(ctx context.Context, _ string, args ...string) ([]byte, error) {
	if strings.Contains(strings.Join(args, " "), "get secrets,deployments") {
		return []byte(`{"items":[]}`), nil
	}
	if strings.Contains(strings.Join(args, " "), "--ignore-not-found") {
		return nil, nil
	}
	if len(args) == 3 && args[0] == "create" && args[1] == "-f" {
		data, err := os.ReadFile(args[2])
		if err != nil {
			return nil, err
		}
		var secret struct {
			StringData map[string]string `json:"stringData"`
		}
		if err := json.Unmarshal(data, &secret); err != nil {
			return nil, err
		}
		r.token = secret.StringData["token"]
		return nil, fmt.Errorf("simulated Secret failure: %s", r.token)
	}
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
	res := requestJSON(t, app, http.MethodPost, "/api/servers", `{"name":"World","ownerId":"0123456789abcdef0123456789abcdef","maxPlayers":4}`)
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
	return &App{store: store, orchestrator: demoOrchestrator{}, demo: demo, auth: &Auth{demo: true}}
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

func serverNamed(t *testing.T, state State, name string) Server {
	t.Helper()
	for _, server := range state.Servers {
		if server.Name == name {
			return server
		}
	}
	t.Fatalf("server named %q is absent", name)
	return Server{}
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

func userRequest(t *testing.T, app *App, method, path, body string, wantStatus int) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+app.auth.token)
	res := httptest.NewRecorder()
	app.ServeHTTP(res, req)
	if res.Code != wantStatus {
		t.Fatalf("%s %s = %d, want %d: %s", method, path, res.Code, wantStatus, res.Body.String())
	}
	return res
}

func TestUsersMigrateLegacyState(t *testing.T) {
	for _, users := range []string{"", `,"users":null`} {
		t.Run(users, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			legacy := `{"servers":{"world":{"id":"world","name":"Existing world","ownerId":"legacy-owner"}},"events":[{"id":"event","serverId":"world","message":"Existing event"}]` + users + `}`
			if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
				t.Fatal(err)
			}
			store, err := NewStore(path, true)
			if err != nil {
				t.Fatal(err)
			}
			before := store.Snapshot()
			if before.Users == nil || len(before.Users) != 0 || before.Servers["world"].OwnerID != "legacy-owner" || before.Events[0].Message != "Existing event" {
				t.Fatalf("legacy load = %+v", before)
			}
			app := &App{store: store, auth: &Auth{token: "test-admin"}}
			res := userRequest(t, app, "POST", "/api/users", `{"name":" Alice ","playerId":"0123456789ABCDEF0123456789ABCDEF"}`, 201)
			var user User
			if err := json.Unmarshal(res.Body.Bytes(), &user); err != nil {
				t.Fatal(err)
			}
			reloaded, err := NewStore(path, false)
			if err != nil {
				t.Fatal(err)
			}
			after := reloaded.Snapshot()
			if user.ID == "" || after.Users[user.ID] != (User{ID: user.ID, Name: "Alice", PlayerID: "0123456789abcdef0123456789abcdef"}) || !reflect.DeepEqual(before.Servers, after.Servers) || !reflect.DeepEqual(before.Events, after.Events) {
				t.Fatalf("migration changed or lost data: %+v", after)
			}
			delete(after.Users, user.ID)
			if reloaded.Snapshot().Users[user.ID].Name != "Alice" {
				t.Fatal("snapshot shares the users map")
			}
		})
	}
}

func TestUsersCRUDAndOwnerCopies(t *testing.T) {
	for _, demo := range []bool{false, true} {
		t.Run(fmt.Sprintf("demo=%t", demo), func(t *testing.T) {
			app := newTestApp(t, false)
			app.demo, app.auth.token = demo, "test-admin"
			list := userRequest(t, app, "GET", "/api/users", "", 200)
			if list.Body.String() != "{\"users\":[]}\n" {
				t.Fatalf("empty list = %s", list.Body.String())
			}
			res := userRequest(t, app, "POST", "/api/users", `{"name":" Alice ","playerId":"0123456789ABCDEF0123456789ABCDEF"}`, 201)
			var user User
			if err := json.Unmarshal(res.Body.Bytes(), &user); err != nil {
				t.Fatal(err)
			}
			if user.ID == "" || user.Name != "Alice" || user.PlayerID != "0123456789abcdef0123456789abcdef" {
				t.Fatalf("created user = %+v", user)
			}
			path := "/api/users/" + user.ID
			userRequest(t, app, "POST", "/api/users", `{"name":"Alias","playerId":"0123456789ABCDEF0123456789abcdef"}`, 409)
			userRequest(t, app, "PUT", path, `{"name":"Alice","playerId":"0123456789ABCDEF0123456789ABCDEF"}`, 200)
			userRequest(t, app, "POST", "/api/users", `{"name":"Alice","playerId":"abcdef0123456789abcdef0123456789"}`, 201)
			userRequest(t, app, "PUT", path, `{"name":"Duplicate","playerId":"ABCDEF0123456789ABCDEF0123456789"}`, 409)
			userRequest(t, app, "POST", "/api/servers", `{"name":"Copied owner","ownerId":"0123456789ABCDEF0123456789ABCDEF","maxPlayers":4}`, 201)
			before := app.store.Snapshot()
			if serverNamed(t, before, "Copied owner").OwnerID != user.PlayerID {
				t.Fatal("server owner was not canonical")
			}
			res = userRequest(t, app, "PUT", path, `{"name":"Renamed","playerId":"11111111111111111111111111111111"}`, 200)
			want := User{ID: user.ID, Name: "Renamed", PlayerID: "11111111111111111111111111111111"}
			var edited User
			if err := json.Unmarshal(res.Body.Bytes(), &edited); err != nil || edited != want {
				t.Fatalf("edited user = %+v, %v", edited, err)
			}
			reloaded, err := NewStore(app.store.path, false)
			if err != nil || reloaded.Snapshot().Users[user.ID] != want {
				t.Fatalf("edit did not persist: %v", err)
			}
			var listing struct {
				Users []User `json:"users"`
			}
			list = userRequest(t, app, "GET", "/api/users", "", 200)
			if err := json.Unmarshal(list.Body.Bytes(), &listing); err != nil || len(listing.Users) != 2 || listing.Users[0].Name != "Alice" || listing.Users[1] != want {
				t.Fatalf("list = %s, %v", list.Body.String(), err)
			}
			if strings.Contains(list.Body.String(), "copied-owner") || strings.Contains(list.Body.String(), "test-admin") || strings.Contains(list.Body.String(), "events") {
				t.Fatal("users endpoint exposed unrelated state")
			}
			res = userRequest(t, app, "DELETE", path, "", 204)
			if res.Body.Len() != 0 {
				t.Fatal("delete returned a body")
			}
			userRequest(t, app, "DELETE", path, "", 404)
			userRequest(t, app, "PUT", path, `{"name":"Missing","playerId":"11111111111111111111111111111111"}`, 404)
			reloaded, err = NewStore(app.store.path, false)
			if err != nil {
				t.Fatal(err)
			}
			after := reloaded.Snapshot()
			if _, ok := after.Users[user.ID]; ok || len(after.Users) != 1 || !reflect.DeepEqual(before.Servers, after.Servers) || !reflect.DeepEqual(before.Events, after.Events) {
				t.Fatalf("edit/delete changed server copies or events: %+v", after)
			}
			userRequest(t, app, "POST", "/api/users", `{"name":"Reusable again","playerId":"11111111111111111111111111111111"}`, 201)
		})
	}
}

func TestPlayerIDValidationOnEveryInput(t *testing.T) {
	invalid := []string{"", " ", "owner", "fixture-owner", strings.Repeat("a", 31), strings.Repeat("a", 33), strings.Repeat("g", 32), "01234567-89ab-cdef-0123-456789abcdef", " 0123456789abcdef0123456789abcdef", "0123456789abcdef0123456789abcdef\n", "0123456789abcdef\t0123456789abcdef", "0123456789abcdef0123456789abcdeＦ", "0123456789abcdef0123456789abcde\x00"}
	for _, demo := range []bool{false, true} {
		app := newTestApp(t, false)
		app.demo, app.auth.token = demo, "test-admin"
		res := userRequest(t, app, "POST", "/api/users", `{"name":"Original","playerId":"0123456789abcdef0123456789abcdef"}`, 201)
		var user User
		if err := json.Unmarshal(res.Body.Bytes(), &user); err != nil {
			t.Fatal(err)
		}
		before := app.store.Snapshot()
		for _, value := range invalid {
			t.Run(fmt.Sprintf("demo=%t/id=%q", demo, value), func(t *testing.T) {
				body, _ := json.Marshal(map[string]any{"name": "Invalid", "playerId": value})
				add := userRequest(t, app, "POST", "/api/users", string(body), 400)
				edit := userRequest(t, app, "PUT", "/api/users/"+user.ID, string(body), 400)
				body, _ = json.Marshal(map[string]any{"name": "Invalid", "ownerId": value, "maxPlayers": 4})
				create := userRequest(t, app, "POST", "/api/servers", string(body), 400)
				if add.Body.String() != edit.Body.String() || add.Body.String() != create.Body.String() || !strings.Contains(add.Body.String(), "32 hexadecimal characters") {
					t.Fatalf("validation differs: %s / %s / %s", add.Body.String(), edit.Body.String(), create.Body.String())
				}
			})
		}
		for _, body := range []string{`{`, `{"name":"","playerId":"0123456789abcdef0123456789abcdef"}`, `{"name":"   ","playerId":"0123456789abcdef0123456789abcdef"}`, `{"name":"Two\nlines","playerId":"0123456789abcdef0123456789abcdef"}`, `{"name":"` + strings.Repeat("x", 49) + `","playerId":"0123456789abcdef0123456789abcdef"}`, `{"id":"chosen-id","name":"Name","playerId":"0123456789abcdef0123456789abcdef"}`} {
			userRequest(t, app, "POST", "/api/users", body, 400)
			userRequest(t, app, "PUT", "/api/users/"+user.ID, body, 400)
		}
		if !reflect.DeepEqual(before, app.store.Snapshot()) {
			t.Fatal("invalid requests changed state")
		}
	}
}

func TestUsersRejectUnicodeLineSeparators(t *testing.T) {
	for _, name := range []string{"Two\u2028lines", "Two\u2029lines"} {
		for _, method := range []string{"POST", "PUT"} {
			t.Run(fmt.Sprintf("%s/name=%q", method, name), func(t *testing.T) {
				app := newTestApp(t, false)
				app.auth.token = "test-admin"
				res := userRequest(t, app, "POST", "/api/users", `{"name":"Zoë 山","playerId":"0123456789abcdef0123456789abcdef"}`, 201)
				var user User
				if err := json.Unmarshal(res.Body.Bytes(), &user); err != nil {
					t.Fatal(err)
				}
				if user.Name != "Zoë 山" {
					t.Fatalf("created name = %q", user.Name)
				}
				before := app.store.Snapshot()
				path := "/api/users"
				if method == "PUT" {
					path += "/" + user.ID
				}
				body, _ := json.Marshal(map[string]string{"name": name, "playerId": "11111111111111111111111111111111"})
				res = userRequest(t, app, method, path, string(body), 400)
				if res.Body.String() != "{\"error\":\"name must be a single line of 1 to 48 characters\"}\n" {
					t.Fatalf("validation error = %s", res.Body.String())
				}
				reloaded, err := NewStore(app.store.path, false)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(before, app.store.Snapshot()) || !reflect.DeepEqual(before, reloaded.Snapshot()) {
					t.Fatal("invalid name changed memory or disk")
				}
			})
		}
	}
}

func TestUsersAuthenticationAndRoutes(t *testing.T) {
	app := newTestApp(t, true)
	app.auth.token = "test-admin"
	before := app.store.Snapshot()
	for _, route := range []struct{ method, path string }{{"GET", "/api/users"}, {"POST", "/api/users"}, {"PUT", "/api/users/missing"}, {"DELETE", "/api/users/missing"}} {
		for _, token := range []string{"", "Bearer wrong"} {
			req := httptest.NewRequest(route.method, route.path, strings.NewReader(`{"name":"Name","playerId":"0123456789abcdef0123456789abcdef"}`))
			req.Header.Set("Authorization", token)
			res := httptest.NewRecorder()
			app.ServeHTTP(res, req)
			if res.Code != 401 || res.Body.String() != "{\"error\":\"authentication required\"}\n" {
				t.Fatalf("unauthenticated %s %s = %d %s", route.method, route.path, res.Code, res.Body.String())
			}
		}
	}
	for _, path := range []string{"/api/users/", "/api/users/missing/extra"} {
		userRequest(t, app, "DELETE", path, "", 404)
	}
	res := userRequest(t, app, "DELETE", "/api/users", "", 405)
	if res.Header().Get("Allow") != "GET, POST" {
		t.Fatal("missing collection Allow header")
	}
	res = userRequest(t, app, "POST", "/api/users/missing", "", 405)
	if res.Header().Get("Allow") != "PUT, DELETE" {
		t.Fatal("missing item Allow header")
	}
	if !reflect.DeepEqual(before, app.store.Snapshot()) {
		t.Fatal("unauthorized or invalid routes changed state")
	}
}

func TestUsersOIDCRolesAndCSRF(t *testing.T) {
	app, issuer := oidcTestApp(t)
	admin, csrf := loginAs(t, app, issuer, "admin")
	viewer, viewerCSRF := loginAs(t, app, issuer, "viewer")
	body := `{"name":"Alice","playerId":"0123456789ABCDEF0123456789ABCDEF"}`
	created := authRequest(app, "POST", "/api/users", admin, app.auth.settings.Origin, csrf, body)
	if created.Code != http.StatusCreated {
		t.Fatalf("admin create = %d %s", created.Code, created.Body.String())
	}
	var user User
	if err := json.Unmarshal(created.Body.Bytes(), &user); err != nil {
		t.Fatal(err)
	}
	if user.ID == "" || user.PlayerID != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("created user = %+v", user)
	}
	path := "/api/users/" + user.ID
	before := app.store.Snapshot()
	for _, route := range [][2]string{{"GET", "/api/users"}, {"POST", "/api/users"}, {"PUT", path}, {"DELETE", path}} {
		if res := authRequest(app, route[0], route[1], nil, "", "", body); res.Code != 401 {
			t.Fatalf("anonymous %v = %d", route, res.Code)
		}
		if res := authRequest(app, route[0], route[1], viewer, app.auth.settings.Origin, viewerCSRF, body); res.Code != 403 {
			t.Fatalf("viewer %v = %d", route, res.Code)
		}
		if route[0] != "GET" {
			for _, pair := range [][2]string{{"", csrf}, {"https://evil.example", csrf}, {app.auth.settings.Origin, ""}, {app.auth.settings.Origin, viewerCSRF}} {
				if res := authRequest(app, route[0], route[1], admin, pair[0], pair[1], body); res.Code != 403 {
					t.Fatalf("CSRF bypass %v = %d", route, res.Code)
				}
			}
		}
	}
	if !reflect.DeepEqual(before, app.store.Snapshot()) {
		t.Fatal("denied users requests changed state")
	}
	list := authRequest(app, "GET", "/api/users", admin, "", "", "")
	if list.Code != 200 || !strings.Contains(list.Body.String(), user.PlayerID) || strings.Contains(list.Body.String(), "servers") {
		t.Fatalf("admin list = %d %s", list.Code, list.Body.String())
	}
	for _, endpoint := range []string{"/api/bootstrap", "/api/servers/scuffedtards/telemetry"} {
		res := authRequest(app, "GET", endpoint, viewer, "", "", "")
		if res.Code != 200 || strings.Contains(res.Body.String(), user.PlayerID) || strings.Contains(res.Body.String(), `"users"`) {
			t.Fatalf("viewer projection = %d %s", res.Code, res.Body.String())
		}
	}
	if res := authRequest(app, "POST", "/api/users", admin, app.auth.settings.Origin, csrf, body); res.Code != 409 {
		t.Fatalf("OIDC duplicate = %d %s", res.Code, res.Body.String())
	}
	if res := authRequest(app, "PUT", path, admin, app.auth.settings.Origin, csrf, `{"name":"Renamed","playerId":"11111111111111111111111111111111"}`); res.Code != 200 {
		t.Fatalf("admin edit = %d %s", res.Code, res.Body.String())
	}
	if got := app.store.Snapshot().Users[user.ID]; got != (User{ID: user.ID, Name: "Renamed", PlayerID: "11111111111111111111111111111111"}) {
		t.Fatalf("OIDC edit = %+v", got)
	}
	if res := authRequest(app, "DELETE", path, admin, app.auth.settings.Origin, csrf, ""); res.Code != 204 || len(app.store.Snapshot().Users) != 0 {
		t.Fatalf("admin delete = %d %s", res.Code, res.Body.String())
	}
	if res := authRequest(app, "POST", "/api/auth/logout", admin, app.auth.settings.Origin, csrf, ""); res.Code != 204 {
		t.Fatalf("logout = %d", res.Code)
	}
	if res := authRequest(app, "GET", "/api/users", admin, "", "", ""); res.Code != 401 {
		t.Fatalf("logged out list = %d", res.Code)
	}
}

func TestUsersConcurrentDuplicate(t *testing.T) {
	app := newTestApp(t, false)
	app.auth.token = "test-admin"
	results := make(chan int, 12)
	for i := 0; i < cap(results); i++ {
		go func() {
			req := httptest.NewRequest("POST", "/api/users", strings.NewReader(`{"name":"Concurrent","playerId":"0123456789abcdef0123456789abcdef"}`))
			req.Header.Set("Authorization", "Bearer test-admin")
			res := httptest.NewRecorder()
			app.ServeHTTP(res, req)
			results <- res.Code
		}()
	}
	counts := map[int]int{}
	for i := 0; i < cap(results); i++ {
		counts[<-results]++
	}
	if counts[201] != 1 || counts[409] != 11 || len(app.store.Snapshot().Users) != 1 {
		t.Fatalf("concurrent duplicate responses = %v", counts)
	}
}

func TestUsersFailedPersistenceLeavesStateUnchanged(t *testing.T) {
	for _, method := range []string{"POST", "PUT", "DELETE"} {
		t.Run(method, func(t *testing.T) {
			app := newTestApp(t, true)
			app.auth.token = "test-admin"
			res := userRequest(t, app, "POST", "/api/users", `{"name":"Original","playerId":"0123456789abcdef0123456789abcdef"}`, 201)
			var user User
			if err := json.Unmarshal(res.Body.Bytes(), &user); err != nil {
				t.Fatal(err)
			}
			before := app.store.Snapshot()
			path := app.store.path
			app.store.path = filepath.Join(path, "cannot-create-under-file.json")
			endpoint := "/api/users"
			if method != "POST" {
				endpoint += "/" + user.ID
			}
			res = userRequest(t, app, method, endpoint, `{"name":"Changed","playerId":"11111111111111111111111111111111"}`, 500)
			if res.Body.String() != "{\"error\":\"could not persist saved IDs\"}\n" {
				t.Fatalf("persistence error exposed internal state: %s", res.Body.String())
			}
			reloaded, err := NewStore(path, false)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, app.store.Snapshot()) || !reflect.DeepEqual(before, reloaded.Snapshot()) {
				t.Fatal("failed persistence changed memory or disk")
			}
		})
	}
}

func TestValidateCreate(t *testing.T) {
	for _, limits := range []struct{ memory, cpu int }{{-1, 1000}, {255, 1000}, {67585, 1000}, {2048, -1}, {2048, 99}, {2048, 64001}} {
		if err := validateCreate(CreateServerRequest{Name: "World", OwnerID: "0123456789abcdef0123456789abcdef", MaxPlayers: 4, MemoryLimitMiB: limits.memory, CPULimitMillis: limits.cpu}); err == nil {
			t.Fatalf("invalid resource limits accepted: %+v", limits)
		}
	}
	if err := validateCreate(CreateServerRequest{Name: "World", MaxPlayers: 12}); err == nil {
		t.Fatal("missing owner ID accepted")
	}
	if err := validateCreate(CreateServerRequest{Name: "World", OwnerID: "0123456789abcdef0123456789abcdef", MaxPlayers: 12, Namespace: "Dragonwilds"}); err == nil {
		t.Fatal("invalid namespace accepted")
	}
	if err := validateCreate(CreateServerRequest{Name: "World", OwnerID: "0123456789abcdef0123456789abcdef", MaxPlayers: 12, ImageTag: "v1.2.3"}); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
}

func TestCreatePersistsResourceLimits(t *testing.T) {
	app := newTestApp(t, false)
	res := requestJSON(t, app, http.MethodPost, "/api/servers", `{"name":"Resource test","ownerId":"0123456789abcdef0123456789abcdef","maxPlayers":6,"memoryLimitMiB":1536,"cpuLimitMillis":750}`)
	if res.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", res.Code, res.Body.String())
	}
	reloaded, err := NewStore(app.store.path, false)
	if err != nil {
		t.Fatal(err)
	}
	server := serverNamed(t, reloaded.Snapshot(), "Resource test")
	if server.MemoryLimitMiB != 1536 || server.CPULimitMillis != 750 || server.MaxPlayers != 6 {
		t.Fatalf("limits not persisted: %+v", server)
	}
}

func TestCreateChartSettingsAndSecretPrivacy(t *testing.T) {
	app := newTestApp(t, false)
	runner := &recordingRunner{}
	app.orchestrator = &kubeOrchestrator{runner: runner, helm: "helm", kubectl: "kubectl", chart: "chart", imageRepository: "example/server"}
	res := requestJSON(t, app, http.MethodPost, "/api/servers", `{"name":"Public name","worldName":"Separate world","ownerId":"0123456789abcdef0123456789abcdef","maxPlayers":6,"memoryLimitMiB":1536,"cpuLimitMillis":750,"gamePort":7780,"storageGiB":2,"serviceType":"NodePort","adminIds":"admin-1,admin-2","debugLevel":3,"autoStopOnUpdate":true,"validateGameFiles":true,"additionalArgs":"-log","serverPassword":"private-join","adminPassword":"private-admin"}`)
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
	server := serverNamed(t, app.store.Snapshot(), "Public name")
	if server.WorldName != "Separate world" || server.GamePort != 7780 || server.PasswordSecret != server.ID+"-settings" || server.ServerPassword != "" || server.AdminPassword != "" {
		t.Fatalf("settings not preserved or credentials retained: %+v", server)
	}
	var helmCall string
	for _, call := range runner.calls {
		if strings.HasPrefix(call, "helm ") {
			helmCall = call
		}
	}
	for _, want := range []string{"RSDW_WORLD_NAME=Separate world", "RSDW_ADMINS=admin-1,admin-2", "RSDW_ADDITIONAL_ARGS=-log -ini:Game:[/Script/Engine.GameSession]:MaxPlayers=6", "RSDW_AUTO_STOP_ON_UPDATE=true", "DEBUG=3", "STEAMAPPVALIDATE=1", "server.port=7780,service.port=7780,persistence.size=2Gi,service.type=NodePort", "resources.limits.memory=1536Mi", "resources.limits.cpu=750m", "valueFrom.secretKeyRef.name=" + server.PasswordSecret} {
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
	res = requestJSON(t, app, http.MethodPost, "/api/servers", `{"name":"Night Shift","namespace":"dragonwilds","ownerId":"0123456789abcdef0123456789abcdef","imageTag":"0.1.1","maxPlayers":12}`)
	if res.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", res.Code, res.Body.String())
	}
	var server Server
	if err := json.Unmarshal(res.Body.Bytes(), &server); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(server.ID, "night-shift-") || server.Status != StatusStarting || server.MemoryLimitMiB != 14336 || server.CPULimitMillis != 6000 {
		t.Fatalf("created server = %+v", server)
	}
	for _, action := range []struct {
		path string
		body string
	}{
		{"/api/servers/" + server.ID + "/actions/restart", ""},
		{"/api/servers/" + server.ID + "/actions/update", `{"imageTag":"0.1.2"}`},
		{"/api/servers/" + server.ID + "/actions/check-update", ""},
	} {
		res = requestJSON(t, app, http.MethodPost, action.path, action.body)
		if res.Code != http.StatusOK {
			t.Fatalf("%s status = %d, body = %s", action.path, res.Code, res.Body.String())
		}
	}
	if res = requestJSON(t, app, http.MethodGet, "/api/servers/"+server.ID+"/logs?tail=10", ""); res.Code != http.StatusOK {
		t.Fatalf("logs status = %d", res.Code)
	}
	if res = requestJSON(t, app, http.MethodGet, "/api/servers/"+server.ID+"/telemetry?range=60s", ""); res.Code != http.StatusOK {
		t.Fatalf("telemetry status = %d", res.Code)
	}
	if res = requestJSON(t, app, http.MethodGet, "/api/events?query=update&category=update&serverId="+server.ID, ""); res.Code != http.StatusOK {
		t.Fatalf("events status = %d", res.Code)
	}
	if res = requestJSON(t, app, http.MethodPost, "/api/servers/"+server.ID+"/actions/update", `{"imageTag":"bad tag"}`); res.Code != http.StatusBadRequest {
		t.Fatalf("invalid update status = %d", res.Code)
	}
}

func TestAuthBoundary(t *testing.T) {
	app := newTestApp(t, false)
	app.auth = &Auth{token: "secret"}
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
	if strings.Contains(call, "get secrets,deployments") {
		return []byte(`{"items":[]}`), nil
	}
	if strings.Contains(call, "--ignore-not-found") {
		return nil, nil
	}
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

func TestKubeOrchestratorRefreshRejectsImageFromNonRunningContainer(t *testing.T) {
	runner := &telemetryRunner{}
	runner.override = func(call string) ([]byte, error, bool) {
		if strings.Contains(call, "get pods") {
			pending := strings.Replace(fixturePod(), `"phase":"Running"`, `"phase":"Pending"`, 1)
			pending = strings.Replace(pending, `"containerStatuses":[{"name":"server","containerID":"container-id","ready":true,"state":{"running":{"startedAt":"2026-01-01T00:00:00Z"}}}]`, `"containerStatuses":[{"name":"server","ready":false,"state":{"waiting":{"reason":"ContainerCreating"}}}]`, 1)
			return []byte(`{"items":[` + pending + `]}`), nil, true
		}
		return nil, nil, false
	}
	orchestrator := &kubeOrchestrator{runner: runner, kubectl: "kubectl"}
	server := Server{Release: "world", Namespace: "games", DesiredImage: "example/server:1"}
	if _, err := orchestrator.Refresh(context.Background(), server); err == nil || err.Error() != "server container is not running" {
		t.Fatalf("refresh error = %v, want non-running container error", err)
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

func TestKubeOrchestratorPreservesObservedImageReference(t *testing.T) {
	for _, tc := range []struct {
		name, image, repository, setting string
	}{
		{"registry port and tag", "registry.example:5000/rsdw/server:2026.09", "image.repository=registry.example:5000/rsdw/server", "image.tag=2026.09"},
		{"digest", "registry.example/rsdw/server@sha256:" + strings.Repeat("a", 64), "image.repository=registry.example/rsdw/server", "image.digest=sha256:" + strings.Repeat("a", 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &recordingRunner{}
			orchestrator := &kubeOrchestrator{runner: runner, helm: "helm", kubectl: "kubectl", chart: "chart", imageRepository: "configured/server"}
			if err := orchestrator.Deploy(context.Background(), Server{Release: "world", Namespace: "games", Name: "World", OwnerID: "eos-1", DesiredImage: tc.image}); err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(runner.calls, "\n")
			if !strings.Contains(joined, "--set-string "+tc.repository) || !strings.Contains(joined, "--set-string "+tc.setting) {
				t.Fatalf("deployment did not preserve %s:\n%s", tc.image, joined)
			}
			if strings.Contains(joined, "image.repository=configured/server") {
				t.Fatalf("deployment used configured repository:\n%s", joined)
			}
		})
	}
}

func TestPlayerResourceDefaults(t *testing.T) {
	for _, tc := range []struct{ players, memory, cpu int }{{1, 3072, 500}, {4, 6144, 2000}, {64, 67584, 32000}} {
		t.Run(fmt.Sprint(tc.players), func(t *testing.T) {
			app := newTestApp(t, false)
			runner := &recordingRunner{}
			app.orchestrator = &kubeOrchestrator{runner: runner, helm: "helm", kubectl: "kubectl", chart: "chart", imageRepository: "example/server"}
			res := requestJSON(t, app, http.MethodPost, "/api/servers", fmt.Sprintf(`{"name":"Owner","worldName":"World","ownerId":"0123456789abcdef0123456789abcdef","imageTag":"0.2.0","maxPlayers":%d}`, tc.players))
			if res.Code != 201 {
				t.Fatalf("create: %d %s", res.Code, res.Body.String())
			}
			stored, err := NewStore(app.store.path, false)
			if err != nil {
				t.Fatal(err)
			}
			server := serverNamed(t, stored.Snapshot(), "Owner")
			if server.MemoryLimitMiB != tc.memory || server.CPULimitMillis != tc.cpu || server.MaxPlayers != tc.players || server.WorldName != "World" || server.Region != "" {
				t.Fatalf("stored server: %+v", server)
			}
			calls := strings.Join(runner.calls, "\n")
			for _, value := range []string{fmt.Sprintf("resources.limits.memory=%dMi", tc.memory), fmt.Sprintf("resources.limits.cpu=%dm", tc.cpu), "image.tag=0.2.0"} {
				if !strings.Contains(calls, value) {
					t.Fatalf("missing %s in %s", value, calls)
				}
			}
		})
	}
	for _, players := range []int{-1, 0, 65, int(^uint(0) >> 1)} {
		res := requestJSON(t, newTestApp(t, false), http.MethodPost, "/api/servers", fmt.Sprintf(`{"name":"Owner","ownerId":"0123456789abcdef0123456789abcdef","maxPlayers":%d}`, players))
		if res.Code != 400 {
			t.Fatalf("players %d accepted", players)
		}
	}
}
