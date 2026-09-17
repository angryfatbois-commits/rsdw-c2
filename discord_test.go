package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func discordResponse(status int, body string, header http.Header) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body))}
}

func discordApp(t *testing.T) (*App, string) {
	t.Helper()
	t.Setenv("RSDW_NAMESPACE", "c2-tests")
	app := newTestApp(t, true)
	app.demo = false
	app.orchestrator = &kubeOrchestrator{kubectl: "kubectl", runner: commandFunc(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if !app.store.mu.TryLock() {
			t.Fatal("Secret network I/O held Store lock")
		}
		app.store.mu.Unlock()
		if strings.Join(args, " ") != "-n c2-tests get secret discord-bot -o json" {
			t.Fatal("unexpected Secret access", args)
		}
		return []byte(`{"data":{"token":"` + base64.StdEncoding.EncodeToString([]byte("fixture.bot.secret")) + `"}}`), nil
	})}
	var id string
	if err := app.store.Update(func(s *State) error {
		i := integrationFixture()
		s.Integrations[i.ID] = i
		e := emitAlert(s, s.Servers["scuffedtards"], ServerDown, "", "Confirmed unhealthy", time.Now())
		id = queueDelivery(s, i, e).ID
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return app, id
}

func TestDiscordDeliveryResultsAndNoSecretLeakage(t *testing.T) {
	for _, tc := range []struct {
		name           string
		code           int
		transportError bool
		want           DeliveryStatus
	}{
		{"accepted", 200, false, DeliverySent},
		{"forbidden", 403, false, DeliveryFailed},
		{"rate limit", 429, false, DeliveryRetry},
		{"server error", 503, false, DeliveryUncertain},
		{"request timeout", 408, false, DeliveryUncertain},
		{"connection loss", 0, true, DeliveryUncertain},
		{"redirect", 302, false, DeliveryFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, id := discordApp(t)
			posts := 0
			app.discordTransport = transportFunc(func(r *http.Request) (*http.Response, error) {
				if !app.store.mu.TryLock() {
					t.Fatal("Discord network I/O held Store lock")
				}
				app.store.mu.Unlock()
				if r.URL.Scheme != "https" || r.URL.Host != "discord.com" || r.Header.Get("Authorization") != "Bot fixture.bot.secret" {
					t.Fatal("invalid Discord target or auth")
				}
				if r.Method == "GET" {
					return discordResponse(200, `{"id":"456","guild_id":"123"}`, nil), nil
				}
				posts++
				if r.URL.Path != "/api/v10/channels/456/messages" || app.store.Snapshot().Deliveries[id].Status != DeliverySending {
					t.Fatal("send preceded durable sending state")
				}
				var payload struct {
					Nonce   string `json:"nonce"`
					Enforce bool   `json:"enforce_nonce"`
					Allowed struct {
						Parse []string `json:"parse"`
					} `json:"allowed_mentions"`
					Content string `json:"content"`
				}
				if json.NewDecoder(r.Body).Decode(&payload) != nil || payload.Nonce != id || !payload.Enforce || payload.Allowed.Parse == nil || len(payload.Allowed.Parse) != 0 || !strings.Contains(payload.Content, id) {
					t.Fatal("unsafe message payload", payload)
				}
				if tc.transportError {
					return nil, errors.New("fixture.bot.secret transport detail")
				}
				return discordResponse(tc.code, `{"retry_after":12.5,"message":"fixture.bot.secret"}`, http.Header{"Location": []string{"https://evil.example/fixture.bot.secret"}, "Retry-After": []string{"10"}}), nil
			})
			now := time.Now().UTC()
			if err := app.processDeliveries(context.Background(), now); err != nil {
				t.Fatal(err)
			}
			d := app.store.Snapshot().Deliveries[id]
			if d.Status != tc.want || posts != 1 || d.Attempts != 1 {
				t.Fatal(d, posts)
			}
			data, _ := os.ReadFile(app.store.path)
			listed := requestJSON(t, app, "GET", "/api/integrations", "")
			if strings.Contains(string(data)+listed.Body.String(), "fixture.bot.secret") {
				t.Fatal("secret leaked into persistence or API")
			}
			if tc.want == DeliveryRetry {
				if d.NextAttempt.Before(now.Add(12500 * time.Millisecond)) {
					t.Fatal("Retry-After ignored", d.NextAttempt)
				}
				if err := app.processDeliveries(context.Background(), now.Add(time.Second)); err != nil {
					t.Fatal(err)
				}
				if posts != 1 {
					t.Fatal("sent before Retry-After")
				}
			} else {
				if err := app.processDeliveries(context.Background(), now.Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
				if posts != 1 {
					t.Fatal("terminal delivery retried")
				}
			}
		})
	}
}

func TestDiscordRetryUsesStableIDAndRotatedSecret(t *testing.T) {
	app, id := discordApp(t)
	posts := 0
	app.discordTransport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			return discordResponse(200, `{"id":"456","guild_id":"123"}`, nil), nil
		}
		posts++
		var payload map[string]any
		if json.NewDecoder(r.Body).Decode(&payload) != nil || payload["nonce"] != id {
			t.Fatal("retry changed nonce")
		}
		if posts == 1 {
			return discordResponse(429, `{"retry_after":2}`, nil), nil
		}
		if r.Header.Get("Authorization") != "Bot rotated.secret" {
			t.Fatal("cached old secret")
		}
		return discordResponse(200, `{}`, nil), nil
	})
	if err := app.processDeliveries(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewStore(app.store.path, false)
	if err != nil {
		t.Fatal(err)
	}
	app.store = reloaded
	input := integrationFixture()
	input.ID = ""
	input.SecretRef = SecretReference{Name: "new-bot", Key: "new-key"}
	if got := requestJSON(t, app, "PUT", "/api/integrations/bot", integrationJSON(t, input)); got.Code != 200 {
		t.Fatal(got.Body.String())
	}
	app.orchestrator.(*kubeOrchestrator).runner = commandFunc(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if strings.Join(args, " ") != "-n c2-tests get secret new-bot -o json" {
			t.Fatal(args)
		}
		return []byte(`{"data":{"new-key":"` + base64.StdEncoding.EncodeToString([]byte("rotated.secret")) + `"}}`), nil
	})
	if err := app.processDeliveries(context.Background(), app.store.Snapshot().Deliveries[id].NextAttempt); err != nil {
		t.Fatal(err)
	}
	d := app.store.Snapshot().Deliveries[id]
	if posts != 2 || d.Status != DeliverySent || d.Attempts != 2 {
		t.Fatal(d, posts)
	}
}

func TestDiscordPreSendFailuresAndRetryBound(t *testing.T) {
	for _, mode := range []string{"secret error", "invalid secret", "channel transport", "channel mismatch", "channel server", "channel redirect", "rate limit"} {
		t.Run(mode, func(t *testing.T) {
			app, id := discordApp(t)
			posts := 0
			if strings.Contains(mode, "secret") {
				app.orchestrator.(*kubeOrchestrator).runner = commandFunc(func(context.Context, string, ...string) ([]byte, error) {
					if mode == "secret error" {
						return []byte("RAW-SECRET"), errors.New("RAW-SECRET")
					}
					return []byte(`{"data":{"token":"RAW-SECRET"}}`), nil
				})
			}
			app.discordTransport = transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method == "POST" {
					posts++
					return discordResponse(429, `{"retry_after":1}`, nil), nil
				}
				switch mode {
				case "channel transport":
					return nil, errors.New("RAW-SECRET")
				case "channel mismatch":
					return discordResponse(200, `{"id":"456","guild_id":"999"}`, nil), nil
				case "channel server":
					return discordResponse(503, "RAW-SECRET", nil), nil
				case "channel redirect":
					return discordResponse(302, "RAW-SECRET", http.Header{"Location": []string{"https://evil.example"}}), nil
				default:
					return discordResponse(200, `{"id":"456","guild_id":"123"}`, nil), nil
				}
			})
			now := time.Now()
			for j := 0; j < 8; j++ {
				if err := app.processDeliveries(context.Background(), now.Add(time.Duration(j)*time.Hour)); err != nil {
					t.Fatal(err)
				}
			}
			d := app.store.Snapshot().Deliveries[id]
			if d.Status != DeliveryFailed || d.Attempts > deliveryMaxAttempts {
				t.Fatal("unbounded retry", d)
			}
			if mode == "rate limit" {
				if posts != deliveryMaxAttempts {
					t.Fatal(posts)
				}
			} else if posts != 0 {
				t.Fatal("failed verification sent message")
			}
			data, _ := os.ReadFile(app.store.path)
			if strings.Contains(string(data), "RAW-SECRET") {
				t.Fatal("failure leaked secret")
			}
		})
	}
}

func TestDiscordGlobalRateLimitPausesOtherDeliveries(t *testing.T) {
	app, _ := discordApp(t)
	if err := app.store.Update(func(s *State) error {
		emitAlert(s, s.Servers["scuffedtards"], ServerRecovered, "", "healthy", time.Now())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	app.discordTransport = transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return discordResponse(429, `{"retry_after":600,"global":true}`, nil), nil
	})
	now := time.Now()
	if err := app.processDeliveries(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("rate limit did not stop other deliveries", calls)
	}
	reloaded, err := NewStore(app.store.path, false)
	if err != nil {
		t.Fatal(err)
	}
	app.store = reloaded
	if err := app.processDeliveries(context.Background(), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("restart lost rate limit")
	}
}

func TestCrashAndResultPersistenceFailureAreUncertain(t *testing.T) {
	t.Run("crash", func(t *testing.T) {
		app, id := discordApp(t)
		if err := app.store.Update(func(s *State) error {
			d := s.Deliveries[id]
			d.Status = DeliverySending
			d.Attempts = 1
			s.Deliveries[id] = d
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		store, err := NewStore(app.store.path, false)
		if err != nil {
			t.Fatal(err)
		}
		app.store = store
		app.discordTransport = transportFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("crash recovery resent ambiguous delivery")
			return nil, nil
		})
		if err := app.processDeliveries(context.Background(), time.Now()); err != nil {
			t.Fatal(err)
		}
		if store.Snapshot().Deliveries[id].Status != DeliveryUncertain {
			t.Fatal("crash did not expose uncertainty")
		}
	})
	t.Run("result write fails", func(t *testing.T) {
		app, id := discordApp(t)
		posts := 0
		app.discordTransport = transportFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method == "GET" {
				return discordResponse(200, `{"id":"456","guild_id":"123"}`, nil), nil
			}
			posts++
			if err := os.Remove(app.store.path); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(app.store.path, 0700); err != nil {
				t.Fatal(err)
			}
			return discordResponse(200, `{}`, nil), nil
		})
		if err := app.processDeliveries(context.Background(), time.Now()); err == nil {
			t.Fatal("result write unexpectedly succeeded")
		}
		if err := os.Remove(app.store.path); err != nil {
			t.Fatal(err)
		}
		if err := app.processDeliveries(context.Background(), time.Now()); err != nil {
			t.Fatal(err)
		}
		if posts != 1 || app.store.Snapshot().Deliveries[id].Status != DeliveryUncertain {
			t.Fatal("result persistence failure duplicated message")
		}
	})
}
