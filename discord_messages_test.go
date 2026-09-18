package main

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDiscordMessagePayloads(t *testing.T) {
	cases := []struct {
		kind               EventKind
		title, description string
		color              int
		fields             []discordEmbedField
	}{
		{MemoryPressureRestartRequested, "Memory pressure restart requested", "The server kept hogging memory. C2 requested a restart before it ate the whole damn limit.", 0xF59E0B, []discordEmbedField{{"Server", "Example server"}}},
		{RestartWarning, "Restart approaching", "A scheduled restart is approaching. Save your progress.", 0xF59E0B, []discordEmbedField{{"Server", "Example server"}}},
		{RestartRequested, "Restart requested", "The server was told to get its act together. It chose a dramatic reboot instead.", 0xF59E0B, []discordEmbedField{{"Server", "Example server"}}},
		{RestartCompleted, "Restart completed", "The corpse has staggered back online. It did the bare minimum and expects a fucking parade.", 0x22C55E, []discordEmbedField{{"Server", "Example server"}}},
		{RestartFailed, "Restart not confirmed", "The restart fucked off into the void and never bothered to report back. Even the server can't explain what the hell it's doing.", 0xEF4444, []discordEmbedField{{"Server", "Example server"}}},
		{PlayerJoined, "Player count increased", "Another idiot crawled into this rancid little server. The population is up; the average brain-cell count remains terminal.", 0x3B82F6, []discordEmbedField{{"Server", "Example server"}}},
		{PlayerLimitReached, "Player limit reached", "It's full. Stop cramming people into this overcrowded shit-casket before it bursts and sprays everyone's problems across Discord.", 0xF59E0B, []discordEmbedField{{"Server", "Example server"}}},
		{ServerDown, "Server unhealthy", "The server has stopped pretending to be functional. How honest of it.", 0xEF4444, []discordEmbedField{{"Server", "Example server"}}},
		{ServerRecovered, "Server recovered", "It is back. Nobody knows why, and nobody should trust it.", 0x22C55E, []discordEmbedField{{"Server", "Example server"}}},
		{ServerStopped, "Server stopped", "The operator parked this world. The volume is still here. The players are not.", 0xF59E0B, []discordEmbedField{{"Server", "Example server"}}},
		{ServerStarted, "Server started", "Same world, same id, same disk. Try not to immediately fill it with tragedy.", 0x22C55E, []discordEmbedField{{"Server", "Example server"}}},
		{IntegrationTest, "Discord integration test", "The bot successfully vomited into Discord and called it a test. The webhook works; civilization remains a mistake.", 0x8B5CF6, nil},
	}
	covered := map[EventKind]bool{}
	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			covered[tc.kind] = true
			event := Event{Kind: tc.kind, ServerName: "Example server", Timestamp: time.Date(2026, 1, 1, 7, 0, 0, 0, time.FixedZone("EST", -5*60*60)), ID: "private-event", ServerID: "private-server", OperationID: "private-operation", Message: "private-message", Details: "private-endpoint secret player identity", Source: "private-source", Accuracy: "private-accuracy"}
			message, err := buildDiscordMessage(Delivery{ID: "private-delivery", Event: event})
			if err != nil {
				t.Fatal(err)
			}
			want := discordEmbed{Title: tc.title, Description: tc.description, Color: tc.color, Fields: tc.fields, Footer: discordEmbedFooter{Text: "Dragonwilds C2"}, Timestamp: "2026-01-01T12:00:00Z"}
			if !reflect.DeepEqual(message, discordMessage{Embeds: []discordEmbed{want}, Nonce: "private-delivery", EnforceNonce: true, AllowedMentions: discordAllowedMentions{Parse: []string{}}}) {
				t.Fatalf("message = %+v", message)
			}
			data, err := json.Marshal(message)
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(data, &payload); err != nil {
				t.Fatal(err)
			}
			if len(payload) != 4 || string(payload["nonce"]) != `"private-delivery"` || string(payload["enforce_nonce"]) != "true" || string(payload["allowed_mentions"]) != `{"parse":[]}` {
				t.Fatalf("payload = %s", data)
			}
			if strings.Contains(string(payload["embeds"]), "private-") {
				t.Fatalf("private data in embed: %s", data)
			}
		})
	}
	for _, rule := range alertRules {
		if covered[rule.Kind] != rule.Available {
			t.Fatalf("rule coverage differs for %s", rule.Kind)
		}
	}
}

func TestDiscordMessageRejectsUnsupportedKinds(t *testing.T) {
	app := newTestApp(t, true)
	app.demo = false
	app.orchestrator = &kubeOrchestrator{runner: commandFunc(func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("unsupported message read a Secret")
		return nil, nil
	})}
	app.discordTransport = transportFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("unsupported message contacted Discord")
		return nil, nil
	})
	for _, kind := range []EventKind{BackupStarted, BackupCompleted, BackupFailed, "unknown-private-kind", ""} {
		delivery := Delivery{Event: Event{Kind: kind, Message: "private message", Details: "private details"}}
		if _, err := renderDiscordEmbed(delivery.Event); err != errUnsupportedDiscordMessage {
			t.Fatalf("render error = %v", err)
		}
		if _, err := buildDiscordMessage(delivery); err != errUnsupportedDiscordMessage || err.Error() != "Discord message kind is unsupported or unavailable" {
			t.Fatalf("build error = %v", err)
		}
		if result := app.sendDiscord(context.Background(), DiscordIntegration{}, delivery); result.status != DeliveryFailed || result.reason != "Discord message kind is unsupported or unavailable" {
			t.Fatalf("result = %+v", result)
		}
	}
}

func TestDiscordMessageMissingServerAndTimestamp(t *testing.T) {
	embed, err := renderDiscordEmbed(Event{Kind: ServerDown, ServerID: "internal-server-id"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(embed.Fields, []discordEmbedField{{Name: "Server", Value: "Unknown server"}}) {
		t.Fatal(embed.Fields)
	}
	data, _ := json.Marshal(embed)
	if strings.Contains(string(data), "timestamp") || strings.Contains(string(data), "internal-server-id") {
		t.Fatalf("embed = %s", data)
	}
	testEmbed, err := renderDiscordEmbed(Event{Kind: IntegrationTest, ServerName: "Must not appear"})
	if err != nil || len(testEmbed.Fields) != 0 {
		t.Fatalf("test embed = %+v, %v", testEmbed, err)
	}
}

func TestDiscordDeliveryViewsUseRenderedEmbed(t *testing.T) {
	delivery := Delivery{ID: "delivery-id", IntegrationID: "bot", Event: Event{Kind: RestartCompleted, ServerName: "Example server", Timestamp: time.Date(2026, 1, 1, 7, 0, 0, 0, time.FixedZone("EST", -5*60*60))}}
	views := discordDeliveryViews([]Delivery{delivery})
	if len(views) != 1 || !reflect.DeepEqual(views[0].Delivery, delivery) {
		t.Fatalf("views = %+v", views)
	}
	if views[0].Embed == nil || views[0].Embed.Title != "Restart completed" || views[0].Embed.Description != "The corpse has staggered back online. It did the bare minimum and expects a fucking parade." {
		t.Fatalf("embed = %+v", views[0].Embed)
	}
	unsupported := discordDeliveryViews([]Delivery{{Event: Event{Kind: BackupStarted}}})
	if len(unsupported) != 1 || unsupported[0].Embed != nil {
		t.Fatalf("unsupported view = %+v", unsupported)
	}
}
