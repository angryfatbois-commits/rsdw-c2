package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type telemetryRunner struct {
	mu       sync.Mutex
	calls    []string
	override func(string) ([]byte, error, bool)
	at       time.Time
	rx, tx   uint64
}

type deadlineRunner struct {
	base           CommandRunner
	block          string
	identityBudget time.Duration
}

func (r *deadlineRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	call := strings.Join(args, " ")
	if strings.Contains(call, "get pod world-pod") {
		deadline, ok := ctx.Deadline()
		if ok {
			r.identityBudget = time.Until(deadline)
		}
	}
	if strings.Contains(call, r.block) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return r.base.Run(ctx, name, args...)
}

func TestSourceTimeoutPreservesOtherMetrics(t *testing.T) {
	for _, tc := range []struct{ command, key, healthy string }{
		{"get --raw", "cpuCores", "players"},
		{"/api/health", "engineReady", "players"},
		{"/api/players", "players", "engineReady"},
		{"df -Pk", "diskPercent", "players"},
		{"cat /proc/net/dev", "networkBytesPerSecond", "players"},
		{"exec ", "engineReady", "cpuCores"},
	} {
		t.Run(tc.key+"_"+tc.healthy, func(t *testing.T) {
			t.Parallel()
			k, base, server := collectorFixture()
			runner := &deadlineRunner{base: base, block: tc.command}
			k.runner = runner
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			got := k.collectObservation(ctx, server, nil)
			if got.metrics[tc.healthy].Status != "available" {
				t.Fatalf("source timeout erased healthy %s: %+v", tc.healthy, got.metrics[tc.healthy])
			}
			expectMetric(t, got.metrics, tc.key, "error", nil)
			if runner.identityBudget <= 0 {
				t.Fatal("identity lookup had no reserved budget")
			}
		})
	}
}

func TestIdentityLookupFailureReason(t *testing.T) {
	k, runner, server := collectorFixture()
	runner.override = func(call string) ([]byte, error, bool) {
		if strings.Contains(call, "get pod world-pod") {
			return nil, errors.New("fixture lookup denied"), true
		}
		return nil, nil, false
	}
	got := k.collectObservation(context.Background(), server, nil)
	if got.metrics["players"].Reason != "Pod identity verification failed: fixture lookup denied" {
		t.Fatalf("incorrect lookup failure reason: %q", got.metrics["players"].Reason)
	}
	if got.image != "" || got.network != nil || got.status != StatusUnknown {
		t.Fatalf("unverified identity retained: %+v", got)
	}
}

func fixturePod() string {
	return `{"metadata":{"name":"world-pod","namespace":"games","uid":"pod-uid","ownerReferences":[{"kind":"ReplicaSet","uid":"rs-uid","controller":true}]},"spec":{"containers":[{"name":"metrics","image":"exporter","resources":{"limits":{"cpu":"100","memory":"1Ti"}}},{"name":"server","image":"example/server:1","resources":{"limits":{"cpu":"500m","memory":"2Gi"}},"volumeMounts":[{"name":"data","mountPath":"/home/steam/rsdw-dedicated"}]}]},"status":{"phase":"Running","containerStatuses":[{"name":"server","containerID":"container-id","ready":true,"state":{"running":{"startedAt":"2026-01-01T00:00:00Z"}}}]}}`
}

func netFixture(rx, tx uint64) string {
	return fmt.Sprintf("Inter-| Receive | Transmit\n face |bytes packets errs drop fifo frame compressed multicast|bytes packets errs drop fifo colls carrier compressed\n lo: 999999 0 0 0 0 0 0 0 999999 0 0 0 0 0 0 0\n eth0: %d 0 0 0 0 0 0 0 %d 0 0 0 0 0 0 0\n", rx, tx)
}

func (r *telemetryRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	call := strings.Join(append([]string{name}, args...), " ")
	r.calls = append(r.calls, call)
	if r.override != nil {
		if data, err, matched := r.override(call); matched {
			return data, err
		}
	}
	switch {
	case strings.Contains(call, "get deployment"):
		return []byte(`{"metadata":{"name":"world-rsdragonwilds","namespace":"games","uid":"deployment-uid"},"spec":{"replicas":1}}`), nil
	case strings.Contains(call, "get replicasets"):
		return []byte(`{"items":[{"metadata":{"namespace":"games","uid":"rs-uid","ownerReferences":[{"kind":"Deployment","uid":"deployment-uid","controller":true}]}}]}`), nil
	case strings.Contains(call, "get pods"):
		return []byte(`{"items":[` + fixturePod() + `]}`), nil
	case strings.Contains(call, "get pod world-pod"):
		return []byte(fixturePod()), nil
	case strings.Contains(call, "get --raw"):
		at := r.at
		if at.IsZero() {
			at = time.Now().UTC().Add(-time.Second)
		}
		return []byte(fmt.Sprintf(`{"metadata":{"name":"world-pod","namespace":"games"},"timestamp":%q,"window":"15s","containers":[{"name":"metrics","usage":{"cpu":"99","memory":"1Ti"}},{"name":"server","usage":{"cpu":"125000000n","memory":"64Mi"}}]}`, at.Format(time.RFC3339Nano))), nil
	case strings.Contains(call, "/api/health"):
		return []byte(`{"engineReady":true,"uptimeSeconds":120}`), nil
	case strings.Contains(call, "/api/players"):
		return []byte(`{"count":2,"players":[{},{}]}`), nil
	case strings.Contains(call, "df -Pk"):
		return []byte("Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/data 1000 250 750 25% /data\n"), nil
	case strings.Contains(call, "cat /proc/net/dev"):
		return []byte(netFixture(r.rx, r.tx)), nil
	}
	return nil, fmt.Errorf("unexpected command %s", call)
}

func collectorFixture() (*kubeOrchestrator, *telemetryRunner, Server) {
	runner := &telemetryRunner{rx: 1000, tx: 2000}
	k := &kubeOrchestrator{runner: runner, kubectl: "kubectl"}
	return k, runner, Server{ID: "world", Release: "world", Namespace: "games", DesiredImage: "example/server:2", MemoryLimitMiB: 999, CPULimitMillis: 999}
}

func expectMetric(t *testing.T, metrics map[string]MetricReading, key, status string, value *float64) {
	t.Helper()
	got := metrics[key]
	if got.Status != status || (value == nil) != (got.Value == nil) {
		t.Fatalf("%s = %+v, want %s %v", key, got, status, value)
	}
	if value != nil && math.Abs(*got.Value-*value) > 1e-7 {
		t.Fatalf("%s = %g, want %g", key, *got.Value, *value)
	}
	if got.Source == "" || got.Unit == "" || (status == "available" && got.ObservedAt == nil) || (status != "available" && got.Reason == "") {
		t.Fatalf("%s missing provenance: %+v", key, got)
	}
}

func number(value float64) *float64 { return &value }

func TestCollectRealSourceContract(t *testing.T) {
	k, runner, server := collectorFixture()
	observed := k.collectObservation(context.Background(), server, nil)
	if observed.status != StatusOnline || observed.image != "example/server:1" {
		t.Fatalf("observation %+v", observed)
	}
	for key, value := range map[string]float64{"players": 2, "uptimeSeconds": 120, "engineReady": 1, "cpuCores": .125, "cpuPercent": 25, "cpuLimitCores": .5, "memoryUsedBytes": 67108864, "memoryLimitBytes": 2147483648, "diskUsedBytes": 256000, "diskCapacityBytes": 1024000, "diskPercent": 25} {
		expectMetric(t, observed.metrics, key, "available", number(value))
	}
	expectMetric(t, observed.metrics, "tickRate", "unsupported", nil)
	for _, key := range networkKeys {
		expectMetric(t, observed.metrics, key, "warming_up", nil)
	}
	if observed.network == nil {
		t.Fatal("missing network baseline")
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "exec ") && (!strings.Contains(call, "exec pod/world-pod -c server --") || strings.Contains(call, "bash")) {
			t.Fatalf("source not pinned or sh compatible: %s", call)
		}
		if strings.Contains(call, "get --raw") && !strings.HasSuffix(call, "/namespaces/games/pods/world-pod") {
			t.Fatalf("wrong metrics request %s", call)
		}
	}
	if !strings.Contains(strings.Join(runner.calls, "\n"), `-H "Authorization: Bearer $(cat /run/rsdwapi/token)"`) {
		t.Fatal("game API not authenticated")
	}
}

func TestMetricsFailuresAreIndependent(t *testing.T) {
	for _, tc := range []struct {
		name, replace, with, key, status string
		want                             *float64
	}{
		{"nan CPU", `"125000000n"`, `"NaN"`, "cpuCores", "error", nil},
		{"missing memory", `"memory":"64Mi"`, `"unrelated":"64Mi"`, "memoryUsedBytes", "error", nil},
		{"negative CPU", `"125000000n"`, `"-1m"`, "cpuCores", "error", nil},
		{"zero CPU", `"125000000n"`, `"0"`, "cpuCores", "available", number(0)},
		{"wrong pod", `"name":"world-pod"`, `"name":"other"`, "cpuCores", "error", nil},
		{"missing container", `"name":"server"`, `"name":"other"`, "cpuCores", "error", nil},
		{"ambiguous container", `"name":"metrics"`, `"name":"server"`, "cpuCores", "error", nil},
		{"missing timestamp", `"timestamp":`, `"ignored":`, "cpuCores", "error", nil},
		{"zero window", `"window":"15s"`, `"window":"0s"`, "cpuCores", "error", nil},
		{"invalid window", `"window":"15s"`, `"window":"wrong"`, "cpuCores", "error", nil},
		{"long window", `"window":"15s"`, `"window":"10m"`, "cpuCores", "error", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, r, server := collectorFixture()
			base := &telemetryRunner{}
			data, _ := base.Run(context.Background(), "kubectl", "get", "--raw")
			r.override = func(call string) ([]byte, error, bool) {
				if strings.Contains(call, "get --raw") {
					return []byte(strings.ReplaceAll(string(data), tc.replace, tc.with)), nil, true
				}
				return nil, nil, false
			}
			got := k.collectObservation(context.Background(), server, nil)
			expectMetric(t, got.metrics, tc.key, tc.status, tc.want)
			expectMetric(t, got.metrics, "players", "available", number(2))
			expectMetric(t, got.metrics, "diskPercent", "available", number(25))
		})
	}
	for _, age := range []time.Duration{-time.Hour, time.Hour} {
		k, r, server := collectorFixture()
		r.at = time.Now().Add(age)
		got := k.collectObservation(context.Background(), server, nil)
		status := "stale"
		if age > 0 {
			status = "error"
		}
		expectMetric(t, got.metrics, "cpuCores", status, nil)
		expectMetric(t, got.metrics, "cpuLimitCores", "available", number(.5))
	}
}

func TestOwnershipAndReplacement(t *testing.T) {
	for _, tc := range []struct{ name, command, replacement string }{
		{"no deployment UID", "get deployment", `{"metadata":{"name":"world-rsdragonwilds","namespace":"games"}}`},
		{"wrong deployment", "get deployment", `{"metadata":{"name":"other","namespace":"games","uid":"deployment-uid"}}`},
		{"wrong replica owner", "get replicasets", `{"items":[]}`},
		{"no pod", "get pods", `{"items":[]}`},
		{"two pods", "get pods", `{"items":[` + fixturePod() + `,` + fixturePod() + `]}`},
		{"unowned pod", "get pods", `{"items":[` + strings.ReplaceAll(fixturePod(), "rs-uid", "someone-else") + `]}`},
		{"missing server", "get pods", `{"items":[` + strings.ReplaceAll(fixturePod(), `"name":"server"`, `"name":"other"`) + `]}`},
		{"pod replaced", "get pod world-pod", strings.ReplaceAll(fixturePod(), "pod-uid", "new-uid")},
		{"container restarted", "get pod world-pod", strings.ReplaceAll(fixturePod(), "container-id", "new-container")},
		{"pod terminating", "get pod world-pod", strings.Replace(fixturePod(), `"metadata":{`, `"metadata":{"deletionTimestamp":"2026-01-01T00:00:00Z",`, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, r, server := collectorFixture()
			r.override = func(call string) ([]byte, error, bool) {
				if strings.Contains(call, tc.command) {
					return []byte(tc.replacement), nil, true
				}
				return nil, nil, false
			}
			result := k.collectObservation(context.Background(), server, nil)
			for key, reading := range result.metrics {
				if reading.Value != nil {
					t.Fatalf("%s exposed value for unsafe target %+v", key, reading)
				}
			}
			if result.network != nil {
				t.Fatal("unsafe baseline retained")
			}
		})
	}
}

func TestUnavailablePodAndLimits(t *testing.T) {
	for _, tc := range []struct{ name, old, new, key, status string }{
		{"no CPU limit", `"cpu":"500m"`, `"other":"500m"`, "cpuPercent", "unavailable"},
		{"zero CPU limit", `"cpu":"500m"`, `"cpu":"0"`, "cpuPercent", "unavailable"},
		{"bad memory limit", `"memory":"2Gi"`, `"memory":"broken"`, "memoryLimitBytes", "error"},
		{"no data", `"name":"data"`, `"name":"other"`, "diskPercent", "unavailable"},
		{"relative data", `"/home/steam/rsdw-dedicated"`, `"relative"`, "diskPercent", "unavailable"},
		{"pending server", `"phase":"Running"`, `"phase":"Pending"`, "players", "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, r, server := collectorFixture()
			r.override = func(call string) ([]byte, error, bool) {
				if strings.Contains(call, "get pods") {
					return []byte(`{"items":[` + strings.ReplaceAll(fixturePod(), tc.old, tc.new) + `]}`), nil, true
				}
				return nil, nil, false
			}
			result := k.collectObservation(context.Background(), server, nil)
			expectMetric(t, result.metrics, tc.key, tc.status, nil)
		})
	}
}

func TestPartialSourceFailuresAndGameValidation(t *testing.T) {
	for _, tc := range []struct {
		name, command, payload, key string
		failure                     bool
	}{
		{"metrics denied", "get --raw", "", "cpuCores", true},
		{"health timeout", "/api/health", "", "engineReady", true},
		{"players denied", "/api/players", "", "players", true},
		{"disk unavailable", "df -Pk", "", "diskPercent", true},
		{"network denied", "cat /proc/net/dev", "", "networkBytesPerSecond", true},
		{"health null", "/api/health", "null", "engineReady", false},
		{"health missing", "/api/health", "{}", "engineReady", false},
		{"health wrong type", "/api/health", `{"engineReady":"true","uptimeSeconds":5}`, "engineReady", false},
		{"negative uptime", "/api/health", `{"engineReady":true,"uptimeSeconds":-1}`, "uptimeSeconds", false},
		{"players missing", "/api/players", "{}", "players", false},
		{"players negative", "/api/players", `{"count":-1}`, "players", false},
		{"players fractional", "/api/players", `{"count":1.5}`, "players", false},
		{"players string", "/api/players", `{"count":"2"}`, "players", false},
		{"players huge", "/api/players", `{"count":1e30}`, "players", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, r, server := collectorFixture()
			r.override = func(call string) ([]byte, error, bool) {
				if strings.Contains(call, tc.command) {
					if tc.failure {
						return nil, errors.New("fixture source failure"), true
					}
					return []byte(tc.payload), nil, true
				}
				return nil, nil, false
			}
			result := k.collectObservation(context.Background(), server, nil)
			expectMetric(t, result.metrics, tc.key, "error", nil)
			expectMetric(t, result.metrics, "cpuLimitCores", "available", number(.5))
			if tc.command != "get --raw" {
				expectMetric(t, result.metrics, "cpuCores", "available", number(.125))
			}
		})
	}
}

func TestQuantityUnits(t *testing.T) {
	for raw, want := range map[string]float64{"0": 0, "125m": .125, "125000u": .125, "125000000n": .125, "1.5": 1.5, "64Ki": 65536, "64Mi": 67108864, "2Gi": 2147483648, "1G": 1e9, "1e6": 1e6, "1Ti": 1099511627776} {
		got, err := quantityNumber(raw)
		if err != nil || math.Abs(got-want) > 1e-8 {
			t.Fatalf("%q = %g %v, want %g", raw, got, err, want)
		}
	}
	for _, raw := range []string{"", "bad", "-1", "NaN", "Inf", "1e1000", "100000000000000000000000"} {
		if _, err := quantityNumber(raw); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
}

func TestNetworkRates(t *testing.T) {
	now := time.Now()
	before, _ := parseNetwork([]byte(netFixture(100, 200)), "pod/container", now.Add(-15*time.Second))
	current, _ := parseNetwork([]byte(netFixture(400, 800)), "pod/container", now)
	metrics := emptyMetrics()
	applyNetwork(metrics, &before, current)
	for key, value := range map[string]float64{"inboundBytesPerSecond": 20, "outboundBytesPerSecond": 40, "networkBytesPerSecond": 60} {
		expectMetric(t, metrics, key, "available", number(value))
	}
	zero := before
	zero.at = now
	applyNetwork(metrics, &before, zero)
	expectMetric(t, metrics, "networkBytesPerSecond", "available", number(0))
	for _, kind := range []string{"pod", "reset", "interfaces", "time", "stale"} {
		changed, _ := parseNetwork([]byte(netFixture(400, 800)), "pod/container", now)
		switch kind {
		case "pod":
			changed.identity = "replacement/container"
		case "reset":
			changed.interfaces["eth0"] = interfaceCounters{1, 1}
		case "interfaces":
			changed.interfaces = map[string]interfaceCounters{"eth1": {400, 800}}
		case "time":
			changed.at = before.at
		case "stale":
			changed.at = now.Add(time.Minute)
		}
		applyNetwork(metrics, &before, changed)
		expectMetric(t, metrics, "networkBytesPerSecond", "warming_up", nil)
	}
}

func TestNetworkAndDiskRejectMalformedPayloads(t *testing.T) {
	for _, raw := range []string{"", "{}", strings.Replace(netFixture(0, 0), "eth0: 0", "eth0: -1", 1), strings.Replace(netFixture(0, 0), "eth0: 0", "eth0: bad", 1), strings.Replace(netFixture(0, 0), "eth0: 0", "eth0: 18446744073709551616", 1)} {
		if _, err := parseNetwork([]byte(raw), "pod", time.Now()); err == nil {
			t.Fatalf("accepted counters %q", raw)
		}
	}
	for _, raw := range []string{"", "header\nrow", "Filesystem 1024-blocks\n/dev/data 0 0 0 0% /data", "Filesystem 1024-blocks\n/dev/data 100 101 0 100% /data", "Filesystem 1024-blocks\n/dev/data 100 -1 100 0% /data", "Filesystem 1024-blocks\n/dev/data 18446744073709551615 0 1 0% /data", "Filesystem 1024-blocks\n/dev/data 100 0 100 0% /data\n/dev/data 100 0 100 0% /data"} {
		if _, _, err := parseDisk([]byte(raw)); err == nil {
			t.Fatalf("accepted df %q", raw)
		}
	}
	used, capacity, err := parseDisk([]byte("Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/data 100 0 100 0% /data"))
	if err != nil || used != 0 || capacity != 102400 {
		t.Fatalf("zero disk = %g %g %v", used, capacity, err)
	}
}

func TestFreshnessPreservesSourceTimeAndHistory(t *testing.T) {
	at := time.Now().Add(-time.Minute)
	metrics := emptyMetrics()
	setReading(metrics, "players", 0, at)
	got := freshMetrics(metrics, time.Now())
	expectMetric(t, got, "players", "stale", nil)
	if !got["players"].ObservedAt.Equal(at) {
		t.Fatal("source timestamp overwritten")
	}
	expectMetric(t, metrics, "players", "available", number(0))
	setReading(metrics, "players", 0, time.Now().Add(time.Hour))
	expectMetric(t, freshMetrics(metrics, time.Now()), "players", "stale", nil)
	for _, value := range []float64{math.NaN(), math.Inf(1), -1} {
		setReading(metrics, "players", value, time.Now())
		expectMetric(t, metrics, "players", "error", nil)
	}
	if _, err := json.Marshal(metrics); err != nil {
		t.Fatal(err)
	}
}

func TestHealthFieldFailureDoesNotEraseOtherReading(t *testing.T) {
	k, r, server := collectorFixture()
	r.override = func(call string) ([]byte, error, bool) {
		if strings.Contains(call, "/api/health") {
			return []byte(`{"engineReady":true,"uptimeSeconds":"broken"}`), nil, true
		}
		return nil, nil, false
	}
	result := k.collectObservation(context.Background(), server, nil)
	expectMetric(t, result.metrics, "engineReady", "available", number(1))
	expectMetric(t, result.metrics, "uptimeSeconds", "error", nil)
}

func TestLocalKernelTelemetryParsers(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux kernel counters required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	runner := shellRunner{}
	data, err := runner.Run(ctx, "cat", "/proc/net/dev")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseNetwork(data, "local", time.Now()); err != nil {
		t.Fatal(err)
	}
	data, err = runner.Run(ctx, "env", "LC_ALL=C", "df", "-Pk", "--", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, capacity, err := parseDisk(data)
	if err != nil || capacity <= 0 {
		t.Fatalf("local data filesystem capacity=%g error=%v", capacity, err)
	}
}
