package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestJoinEvidenceRequiresConsistentHealthyRosterDelta(t *testing.T) {
	alice := ConnectedPlayer{Name: "Alice", CharacterName: "Archer"}
	bob := ConnectedPlayer{Name: "Bob", CharacterName: "Builder"}
	for _, tc := range []struct {
		name          string
		before, after []ConnectedPlayer
		status        RosterStatus
		observed      bool
	}{
		{"named", []ConnectedPlayer{alice}, []ConnectedPlayer{alice, bob}, RosterAvailable, true},
		{"empty baseline", nil, []ConnectedPlayer{alice, bob}, RosterAvailable, false},
		{"empty current", []ConnectedPlayer{alice}, nil, RosterAvailable, false},
		{"unavailable", []ConnectedPlayer{alice}, []ConnectedPlayer{alice, bob}, RosterUnavailable, false},
		{"failed", []ConnectedPlayer{alice}, []ConnectedPlayer{alice, bob}, RosterError, false},
		{"duplicate", []ConnectedPlayer{alice}, []ConnectedPlayer{alice, alice}, RosterAvailable, false},
		{"count mismatch", []ConnectedPlayer{alice}, []ConnectedPlayer{alice}, RosterAvailable, false},
		{"unjustified replacement", []ConnectedPlayer{alice}, []ConnectedPlayer{bob, {CharacterName: "Mage"}}, RosterAvailable, false},
		{"unsafe name", []ConnectedPlayer{alice}, []ConnectedPlayer{alice, {CharacterName: "@everyone"}}, RosterAvailable, false},
		{"missing character", []ConnectedPlayer{alice}, []ConnectedPlayer{alice, {Name: "Bob"}}, RosterAvailable, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, server, now := alertFixture(t)
			before := alertSample(now, "healthy", "runtime", 1)
			before.playerRoster = PlayerRoster{Status: RosterAvailable, Players: tc.before}
			observeAlerts(s, server, before, now)
			after := alertSample(now.Add(15*time.Second), "healthy", "runtime", 2)
			after.playerRoster = PlayerRoster{Status: tc.status, Players: tc.after}
			after.at = after.at.Add(time.Second)
			observeAlerts(s, server, after, after.at)
			if len(s.Events) != 1 || s.Events[0].Kind != PlayerJoined {
				t.Fatalf("events = %+v", s.Events)
			}
			event := s.Events[0]
			embed, err := renderDiscordEmbed(event)
			if err != nil {
				t.Fatal(err)
			}
			if tc.observed {
				if event.Accuracy != "observed" || !reflect.DeepEqual(event.Evidence.JoinedPlayers, []ConnectedPlayer{bob}) || embed.Description != "Builder (Bob) joined the server." {
					t.Fatalf("event = %+v, embed = %+v", event, embed)
				}
			} else if event.Accuracy != "approximate" || len(event.Evidence.JoinedPlayers) != 0 || !strings.Contains(event.Details, "Approximate increase of 1") {
				t.Fatalf("invented join evidence %+v", event)
			}
		})
	}
}

func TestRecoveryEvidencePreservesZeroAndUnavailable(t *testing.T) {
	for _, count := range []int{0, -1, 3} {
		s, server, now := alertFixture(t)
		for index, health := range []string{"healthy", "unhealthy", "unhealthy", "unhealthy", "healthy", "healthy"} {
			at := now.Add(time.Duration(index) * 15 * time.Second)
			observeAlerts(s, server, alertSample(at, health, "runtime", count), at)
		}
		var recovered Event
		for _, event := range s.Events {
			if event.Kind == ServerRecovered {
				recovered = event
			}
		}
		if recovered.Evidence.PlayerCount == nil {
			t.Fatal("recovery omitted player evidence")
		}
		want := PlayerCountEvidence{Available: count >= 0, MaxPlayers: 4}
		if count >= 0 {
			want.Players = count
		}
		if *recovered.Evidence.PlayerCount != want {
			t.Fatalf("count = %+v, want %+v", recovered.Evidence.PlayerCount, want)
		}
		embed, _ := renderDiscordEmbed(recovered)
		wantText := map[int]string{-1: "unavailable/4", 0: "0/4", 3: "3/4"}[count]
		if len(embed.Fields) != 2 || embed.Fields[1].Value != wantText {
			t.Fatalf("embed = %+v", embed)
		}
	}
}

func TestAlertEvidenceCopiesHistoryDeliveriesAndSnapshots(t *testing.T) {
	s, server, now := alertFixture(t)
	i := s.Integrations["bot"]
	i.QuietHours = &QuietHours{Start: "22:00", End: "08:00", Timezone: "UTC"}
	s.Integrations["bot"] = i
	s.Producers[server.ID] = AlertProducer{Roster: []ConnectedPlayer{{CharacterName: "baseline"}}}
	evidence := AlertEvidence{JoinedPlayers: []ConnectedPlayer{{CharacterName: "new"}}, PlayerCount: &PlayerCountEvidence{Available: true, Players: 2, MaxPlayers: 4}, RestartWarning: &RestartWarningEvidence{Trigger: "scheduled", Minutes: 5}}
	event := emitAlertEvidence(s, server, PlayerJoined, "", "", now, evidence, "", "")
	delivery := queueDeliveryForNewEvent(s, i, event)
	requeued := queueDeliveryForNewEvent(s, i, event)
	requeued.Event.Evidence.PlayerCount.Players = 77
	if s.Deliveries[delivery.ID].Event.Evidence.PlayerCount.Players != 2 {
		t.Fatal("existing delivery returned aliased evidence")
	}
	evidence.JoinedPlayers[0].CharacterName = "mutated input"
	evidence.PlayerCount.Players = 99
	evidence.RestartWarning.Minutes = 99
	event.Evidence.JoinedPlayers[0].Name = "mutated return"
	delivery.Event.Evidence.PlayerCount.Players = 88
	clone := s.clone()
	clone.Events[0].Evidence.JoinedPlayers[0].CharacterName = "mutated snapshot"
	clone.Events[0].Evidence.RestartWarning.Minutes = 77
	clone.Deliveries[delivery.ID].Event.Evidence.PlayerCount.Players = 77
	clone.Producers[server.ID].Roster[0].CharacterName = "changed"
	clone.Integrations["bot"].QuietHours.Start = "12:00"
	if s.Events[0].Evidence.JoinedPlayers[0].CharacterName != "new" || s.Events[0].Evidence.JoinedPlayers[0].Name != "" || s.Events[0].Evidence.PlayerCount.Players != 2 || s.Events[0].Evidence.RestartWarning.Minutes != 5 || s.Deliveries[delivery.ID].Event.Evidence.PlayerCount.Players != 2 || s.Producers[server.ID].Roster[0].CharacterName != "baseline" || s.Integrations["bot"].QuietHours.Start != "22:00" {
		t.Fatalf("payload aliasing: %+v", s)
	}
}

func TestQuietHoursSuppressOnlyNewDeliveriesInStoredTimezone(t *testing.T) {
	s, server, _ := alertFixture(t)
	i := s.Integrations["bot"]
	i.QuietHours = &QuietHours{Start: "22:00", End: "08:00", Timezone: "America/New_York"}
	s.Integrations["bot"] = i
	for _, tc := range []struct {
		at    string
		quiet bool
	}{
		{"2026-09-18T01:59:00Z", false}, {"2026-09-18T02:00:00Z", true}, {"2026-09-18T11:59:00Z", true}, {"2026-09-18T12:00:00Z", false},
		{"2026-11-01T05:30:00Z", true}, {"2026-11-01T06:30:00Z", true},
	} {
		at, _ := time.Parse(time.RFC3339, tc.at)
		before := len(s.Deliveries)
		emitAlert(s, server, ServerDown, "", "", at)
		want := before + 1
		if tc.quiet {
			want = before
		}
		if len(s.Deliveries) != want {
			t.Fatalf("%s deliveries = %d, want %d", tc.at, len(s.Deliveries), want)
		}
	}
	if len(s.Events) != 6 {
		t.Fatalf("quiet hours suppressed event history: %d", len(s.Events))
	}
}

func TestScheduledWarningIsIdempotentAndCancelsWhenServerStops(t *testing.T) {
	app := newTestApp(t, true)
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	next := now.Add(5 * time.Minute)
	countWarnings := func(events []Event) int {
		count := 0
		for _, event := range events {
			if event.Kind == RestartWarning {
				count++
			}
		}
		return count
	}
	server := app.store.Snapshot().Servers["scuffedtards"]
	if err := app.store.Update(func(state *State) error {
		state.Events = nil
		integration := integrationFixture()
		integration.Rules[RestartWarning] = true
		state.Integrations[integration.ID] = integration
		state.RebootSchedules["schedule"] = rebootSchedule{ID: "schedule", Definition: rebootDefinition{ServerID: server.ID, Mode: rebootModeDaily, DailyTimes: []string{"13:00"}, ExecutionTimezone: "UTC", WarningMinutes: 10}, Enabled: true, Revision: 1, NextRun: cloneTimePtr(&next)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := app.scanRestartWarnings(now); err != nil {
		t.Fatal(err)
	}
	first := app.store.Snapshot()
	if len(first.Events) != 1 || first.Events[0].Kind != RestartWarning || first.Events[0].Severity != "warning" || len(first.Deliveries) != 1 {
		t.Fatalf("first warning = %+v", first)
	}
	if err := app.scanRestartWarnings(now); err != nil {
		t.Fatal(err)
	}
	second := app.store.Snapshot()
	if countWarnings(second.Events) != 1 || len(second.Deliveries) != 1 {
		t.Fatalf("warning was not idempotent: events=%d deliveries=%d", len(second.Events), len(second.Deliveries))
	}
	app.clock = func() time.Time { return now }
	definition := second.RebootSchedules["schedule"].Definition
	if _, err := app.saveReboot("schedule", definition, true, "test"); err != nil {
		t.Fatal(err)
	}
	if err := app.scanRestartWarnings(now); err != nil {
		t.Fatal(err)
	}
	if got := countWarnings(app.store.Snapshot().Events); got != 1 {
		t.Fatalf("no-op schedule save emitted duplicate warning: %d warnings", got)
	}
	reloaded, err := NewStore(app.store.path, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := (&App{store: reloaded, demo: true}).scanRestartWarnings(now); err != nil {
		t.Fatal(err)
	}
	if got := countWarnings(reloaded.Snapshot().Events); got != 1 {
		t.Fatalf("reload emitted duplicate warning: %d warnings", got)
	}
	if err := app.store.Update(func(state *State) error {
		server := state.Servers[server.ID]
		server.Status = StatusStopped
		state.Servers[server.ID] = server
		cancelObsoleteWarnings(state, now)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, delivery := range app.store.Snapshot().Deliveries {
		if delivery.Status != DeliveryFailed || !strings.Contains(delivery.Result, "cancelled") {
			t.Fatalf("stopped server left warning delivery active: %+v", delivery)
		}
	}
}

func TestScheduledWarningDoesNotConsumeIneligibleOccurrence(t *testing.T) {
	app := newTestApp(t, true)
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	next := now.Add(5 * time.Minute)
	server := app.store.Snapshot().Servers["scuffedtards"]
	if err := app.store.Update(func(state *State) error {
		state.Events = nil
		integration := integrationFixture()
		integration.Rules[RestartWarning] = true
		state.Integrations[integration.ID] = integration
		state.RebootSchedules["schedule"] = rebootSchedule{ID: "schedule", Definition: rebootDefinition{ServerID: server.ID, WarningMinutes: 10}, Enabled: true, Revision: 1, NextRun: cloneTimePtr(&next)}
		state.Producers[server.ID] = AlertProducer{Restart: &RestartOperation{ID: "pending", RequestedAt: now.Add(-time.Minute)}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := app.scanRestartWarnings(now); err != nil {
		t.Fatal(err)
	}
	blocked := app.store.Snapshot()
	if len(blocked.Events) != 0 || blocked.RebootSchedules["schedule"].LastWarningOccurrence != "" {
		t.Fatalf("ineligible occurrence was consumed: events=%v schedule=%+v", blocked.Events, blocked.RebootSchedules["schedule"])
	}
	if err := app.store.Update(func(state *State) error {
		producer := state.Producers[server.ID]
		producer.Restart = nil
		state.Producers[server.ID] = producer
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := app.scanRestartWarnings(now); err != nil {
		t.Fatal(err)
	}
	if got := app.store.Snapshot(); len(got.Events) != 1 || got.Events[0].Kind != RestartWarning {
		t.Fatalf("eligible occurrence did not warn: %+v", got.Events)
	}
}

func TestWarningOnlyScheduleEditPreservesNextRun(t *testing.T) {
	app := newTestApp(t, true)
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	app.clock = func() time.Time { return now }
	definition := rebootDefinition{ServerID: "scuffedtards", Mode: rebootModeInterval, IntervalValue: 1, IntervalUnit: "hours", ExecutionTimezone: "UTC"}
	created, err := app.saveReboot("", definition, true, "test")
	if err != nil {
		t.Fatal(err)
	}
	nextRun, anchor := *created.NextRun, *created.IntervalAnchor
	definition.WarningMinutes = 5
	now = now.Add(10 * time.Minute)
	updated, err := app.saveReboot(created.ID, definition, true, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !updated.NextRun.Equal(nextRun) || !updated.IntervalAnchor.Equal(anchor) {
		t.Fatalf("warning-only edit moved schedule: next=%s anchor=%s", updated.NextRun, updated.IntervalAnchor)
	}
	revision := updated.Revision
	unchanged, err := app.saveReboot(created.ID, definition, true, "test")
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Revision != revision || !unchanged.NextRun.Equal(nextRun) {
		t.Fatalf("no-op edit changed occurrence: revision=%d next=%s", unchanged.Revision, unchanged.NextRun)
	}
}

func TestObservedStoppedStatusBlocksWarnings(t *testing.T) {
	s, server, now := alertFixture(t)
	server.Status = StatusOnline
	s.Servers[server.ID] = server
	observeAlerts(s, server, observation{at: now, status: StatusStopped}, now)
	if s.Servers[server.ID].Status != StatusStopped {
		t.Fatalf("stopped observation was not persisted: %+v", s.Servers[server.ID])
	}
	next := now.Add(time.Minute)
	schedule := rebootSchedule{ID: "schedule", Definition: rebootDefinition{ServerID: server.ID, WarningMinutes: 5}, Enabled: true, Revision: 1, NextRun: cloneTimePtr(&next)}
	if warningEligible(s, schedule, now) {
		t.Fatal("stopped observation remained warning-eligible")
	}
	online := alertSample(now.Add(time.Minute), "healthy", "runtime", -1)
	online.status = StatusOnline
	observeAlerts(s, server, online, online.at)
	if got := s.Servers[server.ID]; got.Status != StatusOnline || got.StopSource != "" {
		t.Fatalf("observed stop did not recover after scale-up: %+v", got)
	}
	parked := server
	parked.Status, parked.StopSource = StatusStopped, stopSourceOperator
	s.Servers[server.ID] = parked
	observeAlerts(s, server, online, online.at.Add(time.Minute))
	if got := s.Servers[server.ID]; got.Status != StatusStopped || got.StopSource != stopSourceOperator {
		t.Fatalf("operator stop was not preserved: %+v", got)
	}
}

func TestMemoryPressureWarningIgnoresScheduledRebootFlag(t *testing.T) {
	t.Setenv("RSDW_REBOOTS_ENABLED", "false")
	app := newTestApp(t, true)
	app.demo = false
	app.orchestrator = &kubeOrchestrator{runner: commandFunc(func(context.Context, string, ...string) ([]byte, error) {
		return []byte(`{"data":{"token":"` + base64.StdEncoding.EncodeToString([]byte("fixture-token")) + `"}}`), nil
	})}
	app.webhookTransport = transportFunc(func(*http.Request) (*http.Response, error) {
		return discordResponse(http.StatusNoContent, "", nil), nil
	})
	now := time.Now().UTC()
	server := app.store.Snapshot().Servers["scuffedtards"]
	occurrence := "memory-pressure/scuffedtards/runtime/2026-09-18T12:00:00Z"
	if err := app.store.Update(func(state *State) error {
		integration := integrationFixture()
		integration.Provider, integration.GuildID, integration.ChannelID, integration.WebhookURL = providerWebhook, "", "", "https://alerts.example.com/events"
		integration.Rules = map[EventKind]bool{RestartWarning: true}
		state.Integrations[integration.ID] = integration
		state.Producers[server.ID] = AlertProducer{MemoryPressure: &MemoryPressureState{WarningOccurrence: occurrence}}
		event := Event{ID: "pressure-warning", Kind: RestartWarning, ServerID: server.ID, ServerName: server.Name, Timestamp: now, OccurrenceID: occurrence, Evidence: AlertEvidence{RestartWarning: &RestartWarningEvidence{Trigger: "memory-pressure", RestartAt: now.Add(time.Minute), Minutes: 1}}}
		queueDeliveryForNewEvent(state, integration, event)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := app.processDeliveries(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	for _, delivery := range app.store.Snapshot().Deliveries {
		if delivery.Status != DeliverySent {
			t.Fatalf("memory-pressure warning was gated by scheduled reboot flag: %+v", delivery)
		}
	}
}

func TestIntegrationProviderValidationAndFrozenTargets(t *testing.T) {
	s, _, _ := alertFixture(t)
	legacy := integrationFixture()
	if err := validateIntegration(legacy, *s); err != nil {
		t.Fatal(err)
	}
	webhook := legacy
	webhook.Provider, webhook.GuildID, webhook.ChannelID, webhook.WebhookURL = providerWebhook, "", "", "https://alerts.example.com/events"
	if err := validateIntegration(webhook, *s); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"http://alerts.example.com", "https://localhost/events", "https://127.0.0.1/events", "https://10.0.0.1", "https://100.64.0.1", "https://169.254.169.254", "https://198.18.0.1", "https://2001:db8::1", "https://user:pass@example.com", "https://example.com?token=secret", "https://example.com/#secret", "https://example.com:8443"} {
		invalid := webhook
		invalid.WebhookURL = target
		if validateIntegration(invalid, *s) == nil {
			t.Errorf("accepted %s", target)
		}
	}
	invalid := webhook
	invalid.GuildID = "123"
	if validateIntegration(invalid, *s) == nil {
		t.Fatal("accepted contradictory target")
	}
	invalid = legacy
	invalid.Provider = "unknown"
	if validateIntegration(invalid, *s) == nil {
		t.Fatal("accepted unknown provider")
	}
	d := queueDeliveryForNewEvent(s, webhook, Event{ID: "test", Kind: IntegrationTest})
	if !deliveryEnabled(webhook, d) {
		t.Fatal("new delivery disabled")
	}
	webhook.WebhookURL = "https://different.example.com/events"
	if deliveryEnabled(webhook, d) {
		t.Fatal("queued delivery followed a changed target")
	}
	for _, quiet := range []QuietHours{{Start: "22:00", End: "22:00", Timezone: "UTC"}, {Start: "25:00", End: "08:00", Timezone: "UTC"}, {Start: "22:00", End: "08:00", Timezone: "invalid"}} {
		invalid = legacy
		invalid.QuietHours = &quiet
		if validateIntegration(invalid, *s) == nil {
			t.Fatalf("accepted quiet hours %+v", quiet)
		}
	}
	for _, ip := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "::1", "fc00::1"} {
		if publicWebhookIP(net.ParseIP(ip)) {
			t.Errorf("public IP accepted %s", ip)
		}
	}
}

func TestWebhookDeliveryContractAndIndependentCooldown(t *testing.T) {
	for _, tc := range []struct {
		code   int
		fail   bool
		status DeliveryStatus
	}{
		{204, false, DeliverySent}, {429, false, DeliveryRetry}, {500, false, DeliveryUncertain}, {408, false, DeliveryUncertain}, {302, false, DeliveryFailed}, {401, false, DeliveryFailed}, {0, true, DeliveryUncertain},
	} {
		t.Run(string(tc.status)+http.StatusText(tc.code), func(t *testing.T) {
			app, id := discordApp(t)
			now := time.Now().UTC()
			cooldown := now.Add(time.Hour)
			if err := app.store.Update(func(s *State) error {
				i := s.Integrations["bot"]
				i.Provider, i.GuildID, i.ChannelID, i.WebhookURL = providerWebhook, "", "", "https://alerts.example.com/events"
				s.Integrations["bot"] = i
				d := s.Deliveries[id]
				d.Provider, d.GuildID, d.ChannelID, d.WebhookURL = i.Provider, "", "", i.WebhookURL
				s.Deliveries[id] = d
				s.DiscordRetryAt = cooldown
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			app.webhookTransport = transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer fixture.bot.secret" || r.Header.Get("Idempotency-Key") != id || r.URL.String() != "https://alerts.example.com/events" {
					t.Fatalf("request %+v", r)
				}
				data, _ := io.ReadAll(r.Body)
				var payload struct {
					Version    int
					DeliveryID string
					Event      Event
				}
				if json.Unmarshal(data, &payload) != nil || payload.Version != 1 || payload.DeliveryID != id || payload.Event.Kind != ServerDown || strings.Contains(string(data), "fixture.bot.secret") {
					t.Fatalf("body %s", data)
				}
				if tc.fail {
					return nil, errors.New("fixture.bot.secret")
				}
				return discordResponse(tc.code, `{"message":"fixture.bot.secret"}`, http.Header{"Retry-After": []string{"20"}, "Location": []string{"https://evil.example.com"}}), nil
			})
			if err := app.processDeliveries(context.Background(), now); err != nil {
				t.Fatal(err)
			}
			snapshot := app.store.Snapshot()
			if d := snapshot.Deliveries[id]; d.Status != tc.status || d.Attempts != 1 {
				t.Fatalf("delivery %+v", d)
			}
			if !snapshot.DiscordRetryAt.Equal(cooldown) {
				t.Fatal("webhook modified Discord cooldown")
			}
			data, _ := json.Marshal(snapshot)
			if strings.Contains(string(data), "fixture.bot.secret") {
				t.Fatal("secret leaked into state")
			}
		})
	}
}
