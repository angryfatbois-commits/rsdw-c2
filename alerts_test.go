package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

func alertFixture(t *testing.T) (*State, Server, time.Time) {
	t.Helper()
	app := newTestApp(t, true)
	s := app.store.Snapshot()
	s.Events = nil
	s.Integrations["bot"] = integrationFixture()
	server := s.Servers["scuffedtards"]
	server.MaxPlayers = 4
	s.Servers[server.ID] = server
	return &s, server, time.Now().UTC()
}

func alertSample(at time.Time, health, runtime string, count int) observation {
	o := observation{at: at, health: health, runtime: runtime, metrics: emptyMetrics()}
	if count >= 0 {
		setReading(o.metrics, "players", float64(count), at)
	}
	return o
}

func eventKinds(s *State) []EventKind {
	kinds := []EventKind{}
	for _, e := range s.Events {
		kinds = append(kinds, e.Kind)
	}
	return kinds
}

func TestPlayerAlertsAreFreshApproximateAndBaselined(t *testing.T) {
	s, server, now := alertFixture(t)
	step := func(offset int, health, runtime string, count int) {
		at := now.Add(time.Duration(offset) * time.Second)
		observeAlerts(s, server, alertSample(at, health, runtime, count), at)
	}
	step(0, "healthy", "one", 4)
	if len(s.Events) != 0 {
		t.Fatal("initial full count emitted alerts")
	}
	step(15, "healthy", "one", 2)
	step(30, "healthy", "one", 4)
	step(45, "healthy", "one", 4)
	if !slices.Equal(eventKinds(s), []EventKind{PlayerJoined, PlayerLimitReached}) {
		t.Fatal(eventKinds(s))
	}
	join := s.Events[0]
	if join.Accuracy != "approximate" || !strings.Contains(join.Message, "approximate") || !strings.Contains(join.Details, "2 to 4") || join.Source == "" {
		t.Fatal(join)
	}
	step(60, "healthy", "two", 8)
	step(75, "", "two", 2)
	step(90, "healthy", "two", 4)
	step(150, "healthy", "two", 8)
	if len(s.Events) != 2 {
		t.Fatal("replacement, unknown, or gap emitted joins", eventKinds(s))
	}
	stale := alertSample(now.Add(165*time.Second), "healthy", "two", 12)
	at := now.Add(100 * time.Second)
	reading := stale.metrics["players"]
	reading.ObservedAt = &at
	stale.metrics["players"] = reading
	observeAlerts(s, server, stale, stale.at)
	step(180, "healthy", "two", 12)
	if len(s.Events) != 2 {
		t.Fatal("stale count emitted joins")
	}
	step(195, "healthy", "two", 1)
	step(210, "healthy", "two", 4)
	if !slices.Equal(eventKinds(s), []EventKind{PlayerJoined, PlayerLimitReached, PlayerJoined, PlayerLimitReached}) {
		t.Fatal("limit did not rearm", eventKinds(s))
	}
	observeAlerts(s, server, alertSample(now.Add(210*time.Second), "healthy", "two", 8), now.Add(210*time.Second))
	if len(s.Events) != 4 {
		t.Fatal("duplicate observation emitted events")
	}
	for _, d := range s.Deliveries {
		if d.Event.ID == "" || d.ID == "" {
			t.Fatal("unstable event contract")
		}
	}
}

func TestDownRecoveryDebounceAndStartup(t *testing.T) {
	s, server, now := alertFixture(t)
	step := func(offset int, health string) {
		at := now.Add(time.Duration(offset) * time.Second)
		observeAlerts(s, server, alertSample(at, health, "runtime", -1), at)
	}
	for _, at := range []int{0, 15, 30, 45} {
		step(at, "unhealthy")
	}
	if len(s.Events) != 0 {
		t.Fatal("startup emitted down")
	}
	step(60, "healthy")
	step(75, "unhealthy")
	step(90, "")
	step(105, "unhealthy")
	step(120, "unhealthy")
	if len(s.Events) != 0 {
		t.Fatal("unknown did not reset streak")
	}
	step(135, "unhealthy")
	step(150, "unhealthy")
	if !slices.Equal(eventKinds(s), []EventKind{ServerDown}) {
		t.Fatal(eventKinds(s))
	}
	step(165, "healthy")
	step(166, "healthy")
	if len(s.Events) != 1 {
		t.Fatal("recovery lacked 15 seconds")
	}
	step(180, "healthy")
	step(195, "healthy")
	if !slices.Equal(eventKinds(s), []EventKind{ServerDown, ServerRecovered}) {
		t.Fatal(eventKinds(s))
	}
	step(210, "unhealthy")
	step(225, "unhealthy")
	s.recoverAlerts()
	step(240, "unhealthy")
	step(255, "unhealthy")
	if len(s.Events) != 2 {
		t.Fatal("process restart retained short debounce streak")
	}
	step(270, "unhealthy")
	s.recoverAlerts()
	step(285, "healthy")
	step(300, "healthy")
	if !slices.Equal(eventKinds(s), []EventKind{ServerDown, ServerRecovered, ServerDown, ServerRecovered}) {
		t.Fatal("established outage did not survive restart", eventKinds(s))
	}
}

type commandFunc func(context.Context, string, ...string) ([]byte, error)

func (f commandFunc) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return f(ctx, name, args...)
}

func TestRestartLifecycleAndUncertainCommand(t *testing.T) {
	app := newTestApp(t, true)
	app.demo = false
	var operation string
	app.orchestrator = &kubeOrchestrator{kubectl: "kubectl", runner: commandFunc(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		s := app.store.Snapshot()
		op := s.Producers["scuffedtards"].Restart
		if op == nil || len(s.Events) != 5 {
			t.Fatal("command ran before durable operation and event")
		}
		operation = op.ID
		if !strings.Contains(strings.Join(args, " "), restartAnnotation) || !strings.Contains(strings.Join(args, " "), operation) {
			t.Fatal("missing Pod operation marker", args)
		}
		return nil, errors.New("RAW-SECRET command timeout")
	})}
	res := requestJSON(t, app, "POST", "/api/servers/scuffedtards/actions/restart", "")
	if res.Code != 202 || strings.Contains(res.Body.String(), "RAW-SECRET") {
		t.Fatal(res.Code, res.Body.String())
	}
	if again := requestJSON(t, app, "POST", "/api/servers/scuffedtards/actions/restart", ""); again.Code != 409 {
		t.Fatal("duplicate restart dispatched")
	}
	s := app.store.Snapshot()
	server := s.Servers["scuffedtards"]
	op := s.Producers[server.ID].Restart
	if !op.CommandUncertain {
		t.Fatal("command failure not reconciled")
	}
	observe := func(o observation) { observeAlerts(&s, server, o, o.at) }
	for _, seconds := range []int{15, 30, 45, 60} {
		observe(alertSample(op.RequestedAt.Add(time.Duration(seconds)*time.Second), "unhealthy", "old", -1))
	}
	if len(s.Events) != 5 {
		t.Fatal("restart emitted false down")
	}
	o := alertSample(op.RequestedAt.Add(75*time.Second), "healthy", "new", 4)
	o.runtimeStarted = op.RequestedAt.Add(time.Second)
	observe(o)
	if s.Producers[server.ID].Restart == nil {
		t.Fatal("unmarked runtime completed restart")
	}
	o.at = op.RequestedAt.Add(90 * time.Second)
	o.restartOperation = operation
	observe(o)
	if s.Producers[server.ID].Restart != nil || s.Events[len(s.Events)-1].Kind != RestartCompleted || s.Events[len(s.Events)-1].OperationID != operation {
		t.Fatal("marked replacement did not reconcile", eventKinds(&s))
	}
	observe(alertSample(op.RequestedAt.Add(105*time.Second), "healthy", "new", 8))
	if len(s.Events) != 6 {
		t.Fatal("restart count baseline emitted false join")
	}
}

func TestRestartFailureRequiresFreshDefinitiveObservation(t *testing.T) {
	s, server, now := alertFixture(t)
	s.Producers[server.ID] = AlertProducer{HealthyBaseline: true, Restart: &RestartOperation{ID: "op", Runtime: "old", RequestedAt: now}}
	observeAlerts(s, server, alertSample(now.Add(6*time.Minute), "", "", -1), now.Add(6*time.Minute))
	if s.Producers[server.ID].Restart == nil || len(s.Events) != 0 {
		t.Fatal("transport unknown failed restart")
	}
	observeAlerts(s, server, alertSample(now.Add(6*time.Minute+time.Second), "healthy", "old", 1), now.Add(6*time.Minute+time.Second))
	if s.Producers[server.ID].Restart != nil || !slices.Equal(eventKinds(s), []EventKind{RestartFailed}) || s.Events[0].OperationID != "op" {
		t.Fatal(eventKinds(s))
	}
	for _, kind := range []EventKind{BackupStarted, BackupCompleted, BackupFailed} {
		if event := emitAlert(s, server, kind, "op", "backup", now); event.ID == "" {
			t.Fatal("available backup producer dropped event", kind)
		}
	}
}

func TestRestartCompletionTimestampPrecision(t *testing.T) {
	second := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	requested := second.Add(900 * time.Millisecond)
	for _, tc := range []struct {
		name, runtime, marker string
		started               time.Time
		want                  EventKind
	}{
		{"same second", "new", "op", second, RestartCompleted},
		{"later second", "new", "op", second.Add(time.Second), RestartCompleted},
		{"older second", "new", "op", second.Add(-time.Second), RestartFailed},
		{"just before second", "new", "op", second.Add(-time.Nanosecond), RestartFailed},
		{"missing start", "new", "op", time.Time{}, RestartFailed},
		{"old runtime", "old", "op", second, RestartFailed},
		{"missing runtime", "", "op", second, RestartFailed},
		{"missing marker", "new", "", second, RestartFailed},
		{"wrong marker", "new", "other", second, RestartFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, server, _ := alertFixture(t)
			s.Producers[server.ID] = AlertProducer{Restart: &RestartOperation{ID: "op", Runtime: "old", RequestedAt: requested}}
			o := alertSample(requested.Add(15*time.Second), "healthy", tc.runtime, -1)
			o.runtimeStarted, o.restartOperation = tc.started, tc.marker
			observeAlerts(s, server, o, o.at)
			if tc.want == RestartCompleted {
				if !slices.Equal(eventKinds(s), []EventKind{RestartCompleted}) || s.Producers[server.ID].Restart != nil || s.Servers[server.ID].Status != StatusOnline {
					t.Fatalf("same-or-later-second marked replacement did not complete: events=%v pending=%t status=%s", eventKinds(s), s.Producers[server.ID].Restart != nil, s.Servers[server.ID].Status)
				}
			} else if len(s.Events) != 0 || s.Producers[server.ID].Restart == nil {
				t.Fatalf("invalid replacement completed restart: %v", eventKinds(s))
			}
			o.at = requested.Add(restartTimeout)
			observeAlerts(s, server, o, o.at)
			if !slices.Equal(eventKinds(s), []EventKind{tc.want}) || s.Events[0].OperationID != "op" || s.Producers[server.ID].Restart != nil {
				t.Fatalf("events=%v, want [%s] for op", eventKinds(s), tc.want)
			}
		})
	}
}

func TestCollectorAlertEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, needle, body, health string
		fail                       bool
	}{
		{"healthy", "", "", "healthy", false},
		{"not ready", "/api/health", `{"engineReady":false}`, "unhealthy", false},
		{"health transport", "/api/health", "", "", true},
		{"pod transport", "get pods", "", "", true},
		{"invalid pod list", "get pods", `{}`, "", false},
		{"invalid ReplicaSet list", "get replicasets", `{}`, "", false},
		{"readiness changed", "get pod world-pod", strings.ReplaceAll(fixturePod(), `"ready":true`, `"ready":false`), "unhealthy", false},
		{"pod not ready", "get pod world-pod", strings.ReplaceAll(fixturePod(), `"type":"Ready","status":"True"`, `"type":"Ready","status":"False"`), "unhealthy", false},
		{"pod readiness unknown", "get pod world-pod", strings.ReplaceAll(fixturePod(), `"type":"Ready","status":"True"`, `"type":"Ready","status":"Unknown"`), "unhealthy", false},
		{"pod readiness missing", "get pod world-pod", strings.ReplaceAll(fixturePod(), `"type":"Ready"`, `"type":"ContainersReady"`), "unhealthy", false},
		{"scaled zero", "get deployment", `{"metadata":{"name":"world-rsdragonwilds","namespace":"games","uid":"deployment-uid"},"spec":{"replicas":0}}`, "unhealthy", false},
		{"no pod", "get pods", `{"items":[]}`, "unhealthy", false},
		{"ambiguous pods", "get pods", `{"items":[` + fixturePod() + `,` + fixturePod() + `]}`, "", false},
		{"identity changed", "get pod world-pod", strings.ReplaceAll(fixturePod(), "container-id", "other"), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, runner, server := collectorFixture()
			runner.override = func(call string) ([]byte, error, bool) {
				if tc.needle != "" && strings.Contains(call, tc.needle) {
					if tc.fail {
						return nil, errors.New("unavailable"), true
					}
					return []byte(tc.body), nil, true
				}
				return nil, nil, false
			}
			o := k.collectObservation(context.Background(), server, nil)
			if o.health != tc.health {
				t.Fatalf("health %q expected %q", o.health, tc.health)
			}
			if tc.name == "healthy" && o.runtime != "pod-uid/container-id" {
				t.Fatal("runtime identity missing", o.runtime)
			}
		})
	}
}

func TestCollectorPodReadinessDrivesAlerts(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart=%t", restart), func(t *testing.T) {
			s, server, now := alertFixture(t)
			k, runner, target := collectorFixture()
			podReady := true
			runner.override = func(call string) ([]byte, error, bool) {
				pod := strings.Replace(fixturePod(), `"metadata":{`, `"metadata":{"annotations":{"rsdw-c2/restart-operation":"op"},`, 1)
				if strings.Contains(call, "get pods") {
					return []byte(`{"items":[` + pod + `]}`), nil, true
				}
				if strings.Contains(call, "get pod world-pod") {
					if !podReady {
						pod = strings.ReplaceAll(pod, `"type":"Ready","status":"True"`, `"type":"Ready","status":"False"`)
					}
					return []byte(pod), nil, true
				}
				return nil, nil, false
			}
			server.Namespace, server.Release = target.Namespace, target.Release
			step := func(seconds int) {
				o := k.collectObservation(context.Background(), server, nil)
				o.at = now.Add(time.Duration(seconds) * time.Second)
				observeAlerts(s, server, o, o.at)
			}
			if restart {
				now = time.Date(2025, 12, 31, 23, 59, 59, 0, time.UTC)
				s.Producers[server.ID] = AlertProducer{Restart: &RestartOperation{ID: "op", Runtime: "old", RequestedAt: now}}
			} else {
				step(0)
			}
			podReady = false
			for _, offset := range []int{15, 30, 45} {
				step(offset)
			}
			want := []EventKind{ServerDown}
			if restart {
				want = nil
				if s.Producers[server.ID].Restart == nil {
					t.Fatal("Pod Ready=False completed restart")
				}
			}
			if !slices.Equal(eventKinds(s), want) {
				t.Fatalf("Pod Ready=False events=%v, want %v", eventKinds(s), want)
			}
			podReady = true
			step(60)
			if !restart && !slices.Equal(eventKinds(s), want) {
				t.Fatal("recovery emitted before debounce", eventKinds(s))
			}
			step(75)
			want = []EventKind{ServerDown, ServerRecovered}
			if restart {
				want = []EventKind{RestartCompleted}
			}
			if !slices.Equal(eventKinds(s), want) {
				t.Fatalf("Pod Ready=True events=%v, want %v", eventKinds(s), want)
			}
		})
	}
}

func TestCollectorQueuesPlayerEvents(t *testing.T) {
	app, runner, server := fixtureApp(t)
	if err := app.store.Update(func(s *State) error {
		i := integrationFixture()
		i.ServerIDs = []string{server.ID}
		s.Integrations[i.ID] = i
		current := s.Servers[server.ID]
		current.MaxPlayers = 4
		s.Servers[server.ID] = current
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	app.collectTelemetry(context.Background())
	runner.override = func(call string) ([]byte, error, bool) {
		if strings.Contains(call, "/api/players") {
			return []byte(`{"count":4}`), nil, true
		}
		return nil, nil, false
	}
	app.collectTelemetry(context.Background())
	s := app.store.Snapshot()
	if !slices.Equal(eventKinds(&s), []EventKind{PlayerJoined, PlayerLimitReached}) || len(s.Deliveries) != 2 {
		t.Fatal(fmt.Sprint(eventKinds(&s)), len(s.Deliveries))
	}
}
