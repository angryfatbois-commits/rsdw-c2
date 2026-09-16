package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func fixtureApp(t *testing.T) (*App, *telemetryRunner, Server) {
	t.Helper()
	k, runner, server := collectorFixture()
	app := newTestApp(t, false)
	app.orchestrator = k
	server.Name = "World"
	if err := app.store.Update(func(state *State) error { state.Servers[server.ID] = server; return nil }); err != nil {
		t.Fatal(err)
	}
	return app, runner, server
}

func TestActualGameTickCapture(t *testing.T) {
	data, err := os.ReadFile("verification/tick-live-observations.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Observations []struct {
			Payload json.RawMessage `json:"payload"`
		} `json:"observations"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatal(err)
	}
	if len(capture.Observations) != 13 {
		t.Fatalf("got %d captured windows", len(capture.Observations))
	}
	for _, observation := range capture.Observations {
		var source struct {
			Tick struct {
				ObservedAt int64   `json:"observedAtUnixMs"`
				Age        float64 `json:"lastSampleAgeSeconds"`
			} `json:"tick"`
		}
		if err := json.Unmarshal(observation.Payload, &source); err != nil {
			t.Fatal(err)
		}
		at := time.UnixMilli(source.Tick.ObservedAt)
		received := at.Add(time.Duration(source.Tick.Age * float64(time.Second))).Add(10 * time.Millisecond)
		metrics := emptyMetrics()
		applyTicks(metrics, observation.Payload, received, 20*time.Millisecond)
		for _, key := range tickKeys {
			if metrics[key].Status != "available" || metrics[key].Value == nil {
				t.Fatalf("%s: %+v", key, metrics[key])
			}
			if !metrics[key].ObservedAt.Equal(at) {
				t.Fatalf("%s lost producer timestamp", key)
			}
		}
	}
	metrics := emptyMetrics()
	applyTicks(metrics, capture.Observations[12].Payload, time.UnixMilli(1789586295722), 50*time.Millisecond)
	rate, p95 := 29.8939524714, 0.729415
	expectMetric(t, metrics, "tickRate", "available", &rate)
	expectMetric(t, metrics, "tickP95Ms", "available", &p95)
}

func TestServerJSONOmitsLegacyMetricsOnlyWhenMetricsPresent(t *testing.T) {
	for _, live := range []bool{false, true} {
		server := Server{ID: "world", Players: 9, ServerPassword: "private-password", AdminPassword: "private-admin"}
		if live {
			server.Metrics = emptyMetrics()
			setReading(server.Metrics, "cpuCores", 0, time.Now())
		}
		data, err := json.Marshal(server)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"players", "uptimeSeconds", "tickRate", "cpuPercent", "memoryUsedBytes", "memoryLimitBytes", "diskPercent", "networkBytesPerSecond"} {
			_, exists := fields[key]
			if exists == live {
				t.Fatalf("live=%t legacy field %s presence=%t", live, key, exists)
			}
		}
		if strings.Contains(string(data), "private-") {
			t.Fatal("JSON exposed passwords")
		}
		if live {
			var decoded Server
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatal(err)
			}
			expectMetric(t, decoded.Metrics, "cpuCores", "available", number(0))
			expectMetric(t, decoded.Metrics, "players", "unavailable", nil)
			expectMetric(t, decoded.Metrics, "tickRate", "unavailable", nil)
		}
	}
}

func TestHTTPHistoryAndReadOnlyContract(t *testing.T) {
	app, runner, server := fixtureApp(t)
	app.authToken = "admin-token"
	before, err := os.ReadFile(app.store.path)
	if err != nil {
		t.Fatal(err)
	}
	app.collectTelemetry(context.Background())
	cache := app.observations()
	cache.mu.Lock()
	previous := cache.history[server.ID][0].network
	previous.at = previous.at.Add(-15 * time.Second)
	cache.mu.Unlock()
	runner.mu.Lock()
	runner.rx, runner.tx = 1300, 2600
	runner.mu.Unlock()
	app.collectTelemetry(context.Background())
	httpServer := httptest.NewServer(app)
	defer httpServer.Close()
	for _, path := range []string{"/api/bootstrap", "/api/servers/world/telemetry?range=1h"} {
		runner.mu.Lock()
		callsBeforeRead := len(runner.calls)
		runner.mu.Unlock()
		req, _ := http.NewRequest(http.MethodGet, httpServer.URL+path, nil)
		req.Header.Set("Authorization", "Bearer admin-token")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("HTTP %d %s", res.StatusCode, body)
		}
		runner.mu.Lock()
		callsAfterRead := len(runner.calls)
		runner.mu.Unlock()
		if callsAfterRead != callsBeforeRead {
			t.Fatal("authenticated browser read triggered source collection")
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(path, "telemetry") {
			var telemetry struct {
				Metrics map[string]MetricReading     `json:"metrics"`
				Samples []map[string]json.RawMessage `json:"samples"`
				Server  Server                       `json:"server"`
				Checks  []HealthCheck                `json:"healthChecks"`
			}
			if err := json.Unmarshal(body, &telemetry); err != nil {
				t.Fatal(err)
			}
			expectMetric(t, telemetry.Metrics, "players", "available", number(2))
			expectMetric(t, telemetry.Metrics, "tickRate", "available", number(30))
			if len(telemetry.Metrics) != 20 || len(telemetry.Samples) != 2 || !reflect.DeepEqual(telemetry.Metrics, telemetry.Server.Metrics) {
				t.Fatalf("wire contract %s", body)
			}
			for _, sample := range telemetry.Samples {
				for _, item := range metricCatalog {
					if _, ok := sample[item.key]; !ok {
						t.Fatalf("missing nullable key %s", item.key)
					}
				}
				if string(sample["tickRate"]) != "30" || sample["observedAt"] == nil || sample["status"] == nil {
					t.Fatalf("sample contract %+v", sample)
				}
			}
			if string(telemetry.Samples[0]["networkBytesPerSecond"]) != "null" || string(telemetry.Samples[1]["networkBytesPerSecond"]) == "null" {
				t.Fatalf("network warmup not reflected %s", body)
			}
			if len(telemetry.Checks) != 1 || telemetry.Checks[0].Name != "Game API engine readiness" {
				t.Fatalf("unperformed health checks %+v", telemetry.Checks)
			}
		} else {
			var servers []Server
			if err := json.Unmarshal(payload["servers"], &servers); err != nil {
				t.Fatal(err)
			}
			if len(servers) != 1 || len(servers[0].Metrics) != 20 {
				t.Fatalf("bootstrap contract %s", body)
			}
		}
	}
	after, _ := os.ReadFile(app.store.path)
	if string(before) != string(after) {
		t.Fatal("collection or reads changed persisted settings")
	}
	restarted := &App{store: app.store, orchestrator: app.orchestrator}
	fresh := restarted.telemetryFor(server, "1h")
	if len(fresh.Samples) != 0 || fresh.MetricsAvailable {
		t.Fatal("restart retained observations or fabricated history")
	}
}

func TestRetentionRangeAndDeletion(t *testing.T) {
	app, _, server := fixtureApp(t)
	cache := app.observations()
	now := time.Now().UTC()
	cache.mu.Lock()
	for i := 500; i > 0; i-- {
		at := now.Add(-time.Duration(i) * 15 * time.Second)
		metrics := emptyMetrics()
		setReading(metrics, "players", 0, at)
		cache.history[server.ID] = append(cache.history[server.ID], observation{at: at, metrics: metrics})
	}
	cache.history["deleted"] = []observation{{at: now}}
	cache.mu.Unlock()
	app.collectTelemetry(context.Background())
	cache.mu.RLock()
	history := cache.history[server.ID]
	if len(history) != 240 || cap(history) != 240 || len(cache.history) != 1 {
		t.Fatalf("retention %d cap=%d servers=%d", len(history), cap(history), len(cache.history))
	}
	if history[0].at.Before(now.Add(-time.Hour)) {
		t.Fatal("expired observation retained")
	}
	cache.mu.RUnlock()
	for _, tc := range []struct {
		window string
		max    int
	}{{"60s", 4}, {"5m", 20}, {"1h", 240}, {"bad", 4}} {
		got := app.telemetryFor(server, tc.window)
		if len(got.Samples) != tc.max {
			t.Fatalf("%s has %d samples want %d", tc.window, len(got.Samples), tc.max)
		}
	}
}

type blockingTelemetryRunner struct {
	base    *telemetryRunner
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *blockingTelemetryRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if strings.Contains(strings.Join(args, " "), "get deployment") {
		r.once.Do(func() { close(r.started) })
		select {
		case <-r.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return r.base.Run(ctx, name, args...)
}

func TestConcurrentDesiredSettingsSurviveCollection(t *testing.T) {
	app, runner, server := fixtureApp(t)
	blocking := &blockingTelemetryRunner{base: runner, started: make(chan struct{}), release: make(chan struct{})}
	app.orchestrator.(*kubeOrchestrator).runner = blocking
	done := make(chan struct{})
	go func() { app.collectTelemetry(context.Background()); close(done) }()
	<-blocking.started
	changed := server
	changed.DesiredImage = "example/server:changed"
	changed.MemoryLimitMiB = 3456
	changed.CPULimitMillis = 1234
	changed.WorldName = "New world"
	changed.AdditionalArgs = "-log"
	if err := app.store.Update(func(state *State) error { state.Servers[server.ID] = changed; return nil }); err != nil {
		t.Fatal(err)
	}
	close(blocking.release)
	<-done
	if !reflect.DeepEqual(app.store.Snapshot().Servers[server.ID], changed) {
		t.Fatal("collector overwrote desired settings")
	}
	got := app.telemetryFor(app.store.Snapshot().Servers[server.ID], "60s").Server
	if got.DesiredImage != changed.DesiredImage || got.MemoryLimitMiB != 3456 || got.CPULimitMillis != 1234 || got.WorldName != "New world" || !got.UpdateAvailable {
		t.Fatalf("read join lost desired settings %+v", got)
	}
	expectMetric(t, got.Metrics, "cpuLimitCores", "available", number(.5))
}

func TestConcurrentReadsCollectionAndUpdates(t *testing.T) {
	app, _, server := fixtureApp(t)
	var wg sync.WaitGroup
	for worker := 0; worker < 6; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				switch worker % 3 {
				case 0:
					app.collectTelemetry(context.Background())
				case 1:
					app.store.Update(func(state *State) error {
						s := state.Servers[server.ID]
						s.DesiredImage = fmt.Sprintf("server:%d", i)
						state.Servers[server.ID] = s
						return nil
					})
				case 2:
					req := httptest.NewRequest("GET", "/api/servers/world/telemetry?range=1h", nil)
					res := httptest.NewRecorder()
					app.ServeHTTP(res, req)
					if res.Code != 200 {
						t.Errorf("read failed %d", res.Code)
					}
				}
			}
		}(worker)
	}
	wg.Wait()
}

type concurrencyRunner struct{ active, max atomic.Int32 }

func (r *concurrencyRunner) Run(ctx context.Context, _ string, _ ...string) ([]byte, error) {
	active := r.active.Add(1)
	defer r.active.Add(-1)
	for {
		old := r.max.Load()
		if active <= old || r.max.CompareAndSwap(old, active) {
			break
		}
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(10 * time.Millisecond):
		return nil, fmt.Errorf("unavailable")
	}
}

func TestCollectorBoundsWorkersAndStops(t *testing.T) {
	app := newTestApp(t, false)
	runner := &concurrencyRunner{}
	app.orchestrator = &kubeOrchestrator{runner: runner}
	app.store.Update(func(state *State) error {
		for i := 0; i < 12; i++ {
			id := fmt.Sprint(i)
			state.Servers[id] = Server{ID: id}
		}
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { app.runCollector(ctx, 15*time.Millisecond); close(done) }()
	deadline := time.After(2 * time.Second)
	for runner.max.Load() < 4 {
		select {
		case <-deadline:
			t.Fatal("collector did not start without browser request")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("collector did not stop")
	}
	if runner.max.Load() > 4 || runner.active.Load() != 0 {
		t.Fatalf("workers max=%d active=%d", runner.max.Load(), runner.active.Load())
	}
}

func TestTickHistoryPreservesProducerTimestampAndGaps(t *testing.T) {
	app, runner, server := fixtureApp(t)
	producerNow := time.Now().UTC().Truncate(time.Millisecond)
	data := tickFixture(producerNow)
	runner.override = func(call string) ([]byte, error, bool) {
		if strings.Contains(call, "/api/metrics") {
			return []byte(data), nil, true
		}
		return nil, nil, false
	}
	app.collectTelemetry(context.Background())
	first := app.telemetryFor(server, "1h")
	wants := map[string]float64{"tickRate": 30, "tickP50Ms": 1, "tickP95Ms": 4, "tickP99Ms": 8, "tickWindowSeconds": 10, "tickSampleCount": 300}
	observed := producerNow.Add(-250 * time.Millisecond)
	for key, want := range wants {
		expectMetric(t, first.Metrics, key, "available", number(want))
		if !first.Metrics[key].ObservedAt.Equal(observed) {
			t.Fatal("receipt time replaced producer time")
		}
	}
	data = strings.Replace(data, `"status":"ready"`, `"status":"unsupported"`, 1)
	app.collectTelemetry(context.Background())
	got := app.telemetryFor(server, "1h")
	if len(got.Samples) != 2 {
		t.Fatalf("history length=%d", len(got.Samples))
	}
	for key, want := range wants {
		expectMetric(t, got.Metrics, key, "unsupported", nil)
		old := got.Samples[0][key].(*float64)
		gap := got.Samples[1][key].(*float64)
		if old == nil || *old != want || gap != nil {
			t.Fatalf("%s lost history or fabricated gap", key)
		}
		if !got.Samples[0]["observedAt"].(map[string]*time.Time)[key].Equal(observed) {
			t.Fatal("history retimestamped producer observation")
		}
	}
	if _, err := json.Marshal(got); err != nil {
		t.Fatal(err)
	}
}

func TestTickCacheUsesExistingFreshnessWithoutRenewingSource(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	collected := now.Add(-15 * time.Second)
	metrics := emptyMetrics()
	applyTicks(metrics, []byte(tickFixture(collected)), collected, 0)
	app, _, server := fixtureApp(t)
	app.observations().history[server.ID] = []observation{{at: collected, metrics: metrics}}
	got := app.telemetryFor(server, "1h")
	for _, key := range tickKeys {
		if got.Metrics[key].Status != "available" || got.Metrics[key].Value == nil {
			t.Fatalf("15-second cache expired: %+v", got.Metrics[key])
		}
		if !got.Metrics[key].ObservedAt.Equal(collected.Add(-250 * time.Millisecond)) {
			t.Fatal("cached source observation renewed")
		}
		expectMetric(t, freshMetrics(metrics, collected.Add(time.Minute)), key, "stale", nil)
		if got.Samples[0][key].(*float64) == nil {
			t.Fatal("valid historic tick sample erased")
		}
	}
}
