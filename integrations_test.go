package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func integrationFixture() DiscordIntegration {
	return DiscordIntegration{ID: "bot", Name: "Discord alerts", Enabled: true, GuildID: "123", ChannelID: "456", SecretRef: SecretReference{Name: "discord-bot", Key: "token"}, ServerIDs: []string{"scuffedtards"}, Rules: map[EventKind]bool{RestartRequested: true, RestartCompleted: true, RestartFailed: true, PlayerJoined: true, PlayerLimitReached: true, ServerDown: true, ServerRecovered: true}}
}

func integrationJSON(t *testing.T, i DiscordIntegration) string {
	t.Helper()
	data, err := json.Marshal(i)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestIntegrationMigrationAndClone(t *testing.T) {
	for _, fields := range []string{"", `,"integrations":null,"deliveries":null,"alertProducers":null`} {
		path := filepath.Join(t.TempDir(), "state.json")
		legacy := `{"servers":{"world":{"id":"world","saveSeed":{"claim":"seed","path":"World.sav"}}},"events":[{"id":"history","message":"kept"}],"users":{"owner":{"id":"owner","playerId":"abc"}},"pendingSeeds":{"seed":{"claim":"seed"}}` + fields + `}`
		if err := os.WriteFile(path, []byte(legacy), 0600); err != nil {
			t.Fatal(err)
		}
		store, err := NewStore(path, false)
		if err != nil {
			t.Fatal(err)
		}
		s := store.Snapshot()
		if s.Integrations == nil || s.Deliveries == nil || s.Producers == nil || s.Servers["world"].SaveSeed.Claim != "seed" || s.Events[0].ID != "history" || s.Users["owner"].PlayerID != "abc" || len(s.PendingSeeds) != 1 {
			t.Fatalf("lossy migration %+v", s)
		}
		if err := store.Update(func(state *State) error {
			state.Integrations["bot"] = integrationFixture()
			state.Producers["world"] = AlertProducer{Outage: true, HealthyBaseline: true, Restart: &RestartOperation{ID: "op"}}
			server := state.Servers["world"]
			server.Metrics = emptyMetrics()
			setReading(server.Metrics, "players", 2, time.Now())
			state.Servers["world"] = server
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		before := store.Snapshot()
		mutate := func(s *State) {
			i := s.Integrations["bot"]
			i.ServerIDs[0] = "changed"
			i.Rules[ServerDown] = false
			s.Producers["world"].Restart.ID = "changed"
			s.Servers["world"].SaveSeed.Claim = "changed"
			*s.Servers["world"].Metrics["players"].Value = 99
			*s.Servers["world"].Metrics["players"].ObservedAt = time.Time{}
		}
		copy := store.Snapshot()
		mutate(&copy)
		if err := store.Update(func(s *State) error { mutate(s); return errors.New("rollback") }); err == nil {
			t.Fatal("update succeeded")
		}
		if !reflect.DeepEqual(before, store.Snapshot()) {
			t.Fatal("nested mutation escaped a snapshot or failed update")
		}
		reloaded, err := NewStore(path, false)
		if err != nil || !reloaded.Snapshot().Producers["world"].Outage || reloaded.Snapshot().Producers["world"].Restart.ID != "op" {
			t.Fatal("outage or restart lost on reload", err)
		}
	}
}

func TestIntegrationEmptyAssociationsStayArrays(t *testing.T) {
	app := newTestApp(t, true)
	i := integrationFixture()
	i.ID = ""
	i.ServerIDs = nil
	i.Rules = nil
	if res := requestJSON(t, app, "POST", "/api/integrations", integrationJSON(t, i)); res.Code != 201 {
		t.Fatal(res.Body.String())
	}
	listed := requestJSON(t, app, "GET", "/api/integrations", "")
	if !strings.Contains(listed.Body.String(), `"serverIds":[]`) || !strings.Contains(listed.Body.String(), `"rules":{}`) {
		t.Fatal("empty configuration is not safe for the UI", listed.Body.String())
	}
	if err := app.store.Update(func(s *State) error {
		for id, i := range s.Integrations {
			i.ServerIDs = nil
			i.Rules = nil
			s.Integrations[id] = i
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewStore(app.store.path, false)
	if err != nil {
		t.Fatal(err)
	}
	app.store = reloaded
	listed = requestJSON(t, app, "GET", "/api/integrations", "")
	if !strings.Contains(listed.Body.String(), `"serverIds":[]`) || !strings.Contains(listed.Body.String(), `"rules":{}`) {
		t.Fatal("nullable nested configuration not migrated")
	}
}

func TestIntegrationAPIConfigurationAndNoReplay(t *testing.T) {
	app := newTestApp(t, true)
	input := integrationFixture()
	input.ID = ""
	res := requestJSON(t, app, "POST", "/api/integrations", integrationJSON(t, input))
	if res.Code != 201 {
		t.Fatal(res.Code, res.Body.String())
	}
	var saved DiscordIntegration
	if err := json.Unmarshal(res.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.ID == "" || len(app.store.Snapshot().Deliveries) != 0 {
		t.Fatal("missing ID or historical replay")
	}
	for _, kind := range []EventKind{BackupStarted, BackupCompleted, BackupFailed} {
		input.Rules[kind] = true
	}
	if got := requestJSON(t, app, "PUT", "/api/integrations/"+saved.ID, integrationJSON(t, input)); got.Code != 200 {
		t.Fatal("available backup rules rejected", got.Code, got.Body.String())
	}
	input.Rules["made_up"] = true
	if got := requestJSON(t, app, "PUT", "/api/integrations/"+saved.ID, integrationJSON(t, input)); got.Code != 400 {
		t.Fatal("unknown rule accepted", got.Code)
	}
	delete(input.Rules, "made_up")
	input.Rules[ServerStopped] = true
	input.Rules[ServerStarted] = true
	if got := requestJSON(t, app, "PUT", "/api/integrations/"+saved.ID, integrationJSON(t, input)); got.Code != 200 {
		t.Fatal("available stop/start rules rejected", got.Code, got.Body.String())
	}
	for _, body := range []string{
		`{"botToken":"RAW-SECRET"}`, `{"RAW-SECRET":true}`, `{"secretRef":{"name":"valid","key":"token","value":"RAW-SECRET"}}`,
		strings.TrimSuffix(integrationJSON(t, input), "}") + `,"token":"RAW-SECRET"}`, integrationJSON(t, input) + ` {"token":"RAW-SECRET"}`,
	} {
		got := requestJSON(t, app, "POST", "/api/integrations", body)
		if got.Code != 400 || strings.Contains(got.Body.String(), "RAW-SECRET") {
			t.Fatal("raw secret accepted or echoed", got.Code, got.Body.String())
		}
	}
	if got := requestJSON(t, app, "POST", "/api/integrations/"+saved.ID+"/test", "{}"); got.Code != 202 {
		t.Fatal(got.Body.String())
	}
	if err := app.processDeliveries(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, d := range app.store.Snapshot().Deliveries {
		if d.Status != DeliverySent || !strings.Contains(d.Result, "Demo") {
			t.Fatal(d)
		}
	}
	if err := app.store.Update(func(s *State) error {
		emitAlert(s, s.Servers["scuffedtards"], ServerDown, "", "unhealthy", time.Now())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	input.Enabled = false
	input.SecretRef = SecretReference{Name: "rotated", Key: "next"}
	if got := requestJSON(t, app, "PUT", "/api/integrations/"+saved.ID, integrationJSON(t, input)); got.Code != 200 {
		t.Fatal(got.Body.String())
	}
	for _, d := range app.store.Snapshot().Deliveries {
		if d.Event.Kind == ServerDown && d.Status != DeliveryFailed {
			t.Fatal("disable did not cancel queued alert")
		}
	}
	input.Enabled = true
	if got := requestJSON(t, app, "PUT", "/api/integrations/"+saved.ID, integrationJSON(t, input)); got.Code != 200 {
		t.Fatal(got.Body.String())
	}
	if len(app.store.Snapshot().Deliveries) != 2 {
		t.Fatal("reenabling replayed history")
	}
	listed := requestJSON(t, app, "GET", "/api/integrations", "")
	if !strings.Contains(listed.Body.String(), "rotated") || !strings.Contains(listed.Body.String(), `"kind":"backup_started","label":"Backup started","available":true`) {
		t.Fatal(listed.Body.String())
	}
	reloaded, err := NewStore(app.store.path, false)
	if err != nil || reloaded.Snapshot().Integrations[saved.ID].SecretRef.Name != "rotated" {
		t.Fatal("configuration did not persist", err)
	}
}

func TestIntegrationAuthAndCSRF(t *testing.T) {
	app, issuer := oidcTestApp(t)
	viewer, viewerCSRF := loginAs(t, app, issuer, "viewer")
	admin, csrf := loginAs(t, app, issuer, "admin")
	input := integrationFixture()
	input.ID = ""
	body := integrationJSON(t, input)
	before := app.store.Snapshot()
	for _, route := range [][2]string{{"GET", "/api/integrations"}, {"POST", "/api/integrations"}, {"PUT", "/api/integrations/bot"}, {"POST", "/api/integrations/bot/test"}} {
		if r := authRequest(app, route[0], route[1], nil, "", "", body); r.Code != 401 {
			t.Fatal("anonymous integration access", r.Code)
		}
		if r := authRequest(app, route[0], route[1], viewer, app.auth.settings.Origin, viewerCSRF, body); r.Code != 403 {
			t.Fatal("viewer integration access", r.Code)
		}
		if route[0] != "GET" {
			for _, pair := range [][2]string{{"", csrf}, {"https://evil.example", csrf}, {app.auth.settings.Origin, ""}} {
				if r := authRequest(app, route[0], route[1], admin, pair[0], pair[1], body); r.Code != 403 {
					t.Fatal("integration CSRF bypass", r.Code)
				}
			}
		}
	}
	if !reflect.DeepEqual(before, app.store.Snapshot()) {
		t.Fatal("denied request mutated state")
	}
	if r := authRequest(app, "POST", "/api/integrations", admin, app.auth.settings.Origin, csrf, body); r.Code != 201 {
		t.Fatal(r.Code, r.Body.String())
	}
	if capabilities(RoleViewer)["integrations"] || !capabilities(RoleAdmin)["integrations"] {
		t.Fatal("invalid capability")
	}
	app.auth = &Auth{token: "admin-token"}
	if r := requestJSON(t, app, http.MethodGet, "/api/integrations", ""); r.Code != 401 {
		t.Fatal("missing bearer accepted")
	}
}

func TestDeliveryIndependentOfDisplayRetention(t *testing.T) {
	app := newTestApp(t, true)
	var first Event
	err := app.store.Update(func(s *State) error {
		s.Integrations["bot"] = integrationFixture()
		server := s.Servers["scuffedtards"]
		first = emitAlert(s, server, ServerDown, "", "confirmed unhealthy", time.Now())
		for j := 0; j < 600; j++ {
			appendEvent(s, server, "player", "success", "display only", "")
		}
		queueDeliveryForNewEvent(s, s.Integrations["bot"], first)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewStore(app.store.path, false)
	if err != nil {
		t.Fatal(err)
	}
	s := reloaded.Snapshot()
	if !containsEventID(s.Events, first.ID) {
		t.Fatal("player flood dropped ServerDown")
	}
	if got := countEventClass(s.Events, eventRoutine); got != maxRoutineEvents {
		t.Fatalf("routine events = %d, want %d", got, maxRoutineEvents)
	}
	if len(s.Deliveries) != 1 {
		t.Fatal("display retention dropped or duplicated queue")
	}
	for _, d := range s.Deliveries {
		if d.Event.ID != first.ID || d.Event.Details != first.Details || len(d.ID) != 24 {
			t.Fatal("delivery lost its immutable event", d)
		}
	}
	app.store = reloaded
	if err := app.processDeliveries(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, d := range app.store.Snapshot().Deliveries {
		if d.Status != DeliverySent {
			t.Fatal("retained event could not be sent")
		}
	}
}
