package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
)

const telemetryCommandTimeout = 1500 * time.Millisecond

type kubeMetadata struct {
	Name              string     `json:"name"`
	Namespace         string     `json:"namespace"`
	UID               string     `json:"uid"`
	DeletionTimestamp *time.Time `json:"deletionTimestamp"`
	OwnerReferences   []struct {
		Kind       string `json:"kind"`
		UID        string `json:"uid"`
		Controller bool   `json:"controller"`
	} `json:"ownerReferences"`
}

func (m kubeMetadata) ownedBy(kind, uid string) bool {
	if uid == "" {
		return false
	}
	controllers := 0
	matched := false
	for _, owner := range m.OwnerReferences {
		if owner.Controller {
			controllers++
			matched = owner.Kind == kind && owner.UID == uid
		}
	}
	return controllers == 1 && matched
}

type telemetryContainer struct {
	Name      string `json:"name"`
	Image     string `json:"image"`
	Resources struct {
		Limits map[string]string `json:"limits"`
	} `json:"resources"`
	VolumeMounts []struct {
		Name      string `json:"name"`
		MountPath string `json:"mountPath"`
	} `json:"volumeMounts"`
}

type telemetryPod struct {
	Metadata kubeMetadata `json:"metadata"`
	Spec     struct {
		Containers  []telemetryContainer `json:"containers"`
		HostNetwork bool                 `json:"hostNetwork"`
	} `json:"spec"`
	Status struct {
		Phase             string `json:"phase"`
		ContainerStatuses []struct {
			Name        string `json:"name"`
			ContainerID string `json:"containerID"`
			Ready       bool   `json:"ready"`
			State       struct {
				Running *struct {
					StartedAt time.Time `json:"startedAt"`
				} `json:"running"`
			} `json:"state"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

type podTarget struct {
	pod         telemetryPod
	container   telemetryContainer
	containerID string
	startedAt   time.Time
	ready       bool
}

func (k *kubeOrchestrator) kubeJSON(ctx context.Context, target any, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, telemetryCommandTimeout)
	defer cancel()
	data, err := k.runner.Run(ctx, k.kubectl, args...)
	if err != nil {
		return err
	}
	if err = json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("invalid Kubernetes JSON: %w", err)
	}
	return nil
}

func (k *kubeOrchestrator) resolvePod(ctx context.Context, server Server) (podTarget, Status, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var deployment struct {
		Metadata kubeMetadata `json:"metadata"`
		Spec     struct {
			Replicas *int `json:"replicas"`
		} `json:"spec"`
	}
	if err := k.kubeJSON(ctx, &deployment, "-n", server.Namespace, "get", "deployment", deploymentName(server.Release), "-o", "json"); err != nil {
		return podTarget{}, StatusUnknown, err
	}
	if deployment.Metadata.UID == "" || deployment.Metadata.Name != deploymentName(server.Release) || deployment.Metadata.Namespace != server.Namespace || deployment.Metadata.DeletionTimestamp != nil {
		return podTarget{}, StatusUnknown, errors.New("deployment identity is missing or invalid")
	}
	if deployment.Spec.Replicas != nil && *deployment.Spec.Replicas == 0 {
		return podTarget{}, StatusStopped, errors.New("deployment is scaled to zero")
	}
	var replicas struct {
		Items []struct {
			Metadata kubeMetadata `json:"metadata"`
		} `json:"items"`
	}
	if err := k.kubeJSON(ctx, &replicas, "-n", server.Namespace, "get", "replicasets", "-o", "json"); err != nil {
		return podTarget{}, StatusUnknown, err
	}
	owned := map[string]bool{}
	for _, rs := range replicas.Items {
		if rs.Metadata.Namespace == server.Namespace && rs.Metadata.ownedBy("Deployment", deployment.Metadata.UID) && rs.Metadata.UID != "" {
			owned[rs.Metadata.UID] = true
		}
	}
	var pods struct {
		Items []telemetryPod `json:"items"`
	}
	if err := k.kubeJSON(ctx, &pods, "-n", server.Namespace, "get", "pods", "-o", "json"); err != nil {
		return podTarget{}, StatusUnknown, err
	}
	candidates := []telemetryPod{}
	for _, pod := range pods.Items {
		if pod.Metadata.Namespace != server.Namespace || pod.Metadata.DeletionTimestamp != nil || pod.Status.Phase == "Succeeded" || pod.Status.Phase == "Failed" {
			continue
		}
		for uid := range owned {
			if pod.Metadata.ownedBy("ReplicaSet", uid) {
				candidates = append(candidates, pod)
				break
			}
		}
	}
	if len(candidates) != 1 {
		return podTarget{}, StatusStarting, fmt.Errorf("expected one active owned Pod, found %d", len(candidates))
	}
	pod := candidates[0]
	if pod.Metadata.UID == "" || pod.Metadata.Name == "" {
		return podTarget{}, StatusUnknown, errors.New("Pod identity is missing")
	}
	target := podTarget{pod: pod}
	count := 0
	for _, container := range pod.Spec.Containers {
		if container.Name == "server" {
			target.container = container
			count++
		}
	}
	if count != 1 {
		return podTarget{}, StatusUnknown, errors.New("expected exactly one server container")
	}
	count = 0
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == "server" {
			count++
			if status.State.Running != nil {
				target.startedAt = status.State.Running.StartedAt
				target.containerID = status.ContainerID
				target.ready = status.Ready
			}
		}
	}
	if count != 1 || target.containerID == "" || target.startedAt.IsZero() || target.startedAt.After(time.Now().Add(5*time.Second)) || pod.Status.Phase != "Running" {
		target.containerID = ""
		target.startedAt = time.Time{}
		return target, StatusStarting, nil
	}
	if target.ready {
		return target, StatusOnline, nil
	}
	return target, StatusStarting, nil
}

func (k *kubeOrchestrator) podExec(ctx context.Context, target podTarget, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, telemetryCommandTimeout)
	defer cancel()
	base := []string{"-n", target.pod.Metadata.Namespace, "exec", "pod/" + target.pod.Metadata.Name, "-c", "server", "--"}
	return k.runner.Run(ctx, k.kubectl, append(base, args...)...)
}

func (k *kubeOrchestrator) podGameAPI(ctx context.Context, target podTarget, endpoint string) ([]byte, error) {
	port, err := strconv.Atoi(defaultValue(k.gameAPIPort, "8080"))
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("invalid game API port")
	}
	if endpoint != "health" && endpoint != "players" {
		return nil, errors.New("unsupported game API endpoint")
	}
	script := fmt.Sprintf("curl -fsS --max-time 3 -H \"Authorization: Bearer $(cat /run/rsdwapi/token)\" http://127.0.0.1:%d/api/%s", port, endpoint)
	return k.podExec(ctx, target, "sh", "-ec", script)
}

var resourceKeys = []string{"cpuCores", "cpuPercent", "memoryUsedBytes"}
var networkKeys = []string{"inboundBytesPerSecond", "outboundBytesPerSecond", "networkBytesPerSecond"}
var diskKeys = []string{"diskUsedBytes", "diskCapacityBytes", "diskPercent"}

func (k *kubeOrchestrator) collectObservation(ctx context.Context, server Server, previous *networkCounters) observation {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	result := observation{metrics: emptyMetrics(), status: StatusUnknown}
	target, status, err := k.resolvePod(ctx, server)
	result.status = status
	if err != nil {
		for key := range result.metrics {
			if key != "tickRate" {
				failReadings(result.metrics, []string{key}, "unavailable", "Pod discovery: "+err.Error(), nil)
			}
		}
		return result
	}
	result.image = target.container.Image
	at := time.Now().UTC()
	for _, item := range []struct{ resource, key string }{{"cpu", "cpuLimitCores"}, {"memory", "memoryLimitBytes"}} {
		raw := target.container.Resources.Limits[item.resource]
		if raw == "" {
			failReadings(result.metrics, []string{item.key}, "unavailable", "Container has no explicit "+item.resource+" limit", &at)
			continue
		}
		value, err := quantityNumber(raw)
		if err != nil {
			failReadings(result.metrics, []string{item.key}, "error", "Invalid container limit", &at)
			continue
		}
		if value == 0 {
			failReadings(result.metrics, []string{item.key}, "unavailable", "Container has no effective "+item.resource+" limit", &at)
			continue
		}
		setReading(result.metrics, item.key, value, at)
	}
	if target.containerID == "" || target.startedAt.IsZero() {
		for key, reading := range result.metrics {
			if reading.Status == "unavailable" && key != "cpuLimitCores" && key != "memoryLimitBytes" {
				failReadings(result.metrics, []string{key}, "unavailable", "Server container is not running", nil)
			}
		}
		return result
	}
	deadline, _ := ctx.Deadline()
	sourceCtx, cancelSources := context.WithDeadline(ctx, deadline.Add(-telemetryCommandTimeout))
	defer cancelSources()
	k.collectResources(sourceCtx, target, result.metrics)
	k.collectGame(sourceCtx, target, result.metrics)
	k.collectDisk(sourceCtx, target, result.metrics)
	if target.pod.Spec.HostNetwork {
		failReadings(result.metrics, networkKeys, "unsupported", "Host-network counters cannot be attributed to this Pod", nil)
	} else {
		data, err := k.podExec(sourceCtx, target, "cat", "/proc/net/dev")
		at = time.Now().UTC()
		if err != nil {
			failReadings(result.metrics, networkKeys, "error", "Network counters: "+err.Error(), &at)
		} else {
			counters, err := parseNetwork(data, target.pod.Metadata.UID+"/"+target.containerID, at)
			if err != nil {
				failReadings(result.metrics, networkKeys, "error", err.Error(), &at)
			} else {
				result.network = &counters
				applyNetwork(result.metrics, previous, counters)
			}
		}
	}
	// kubectl exec pins a name, not a UID. Discard a collection spanning replacement.
	var after telemetryPod
	if err := k.kubeJSON(ctx, &after, "-n", target.pod.Metadata.Namespace, "get", "pod", target.pod.Metadata.Name, "-o", "json"); err != nil || !sameContainer(target, after) {
		reason := "Pod or server container changed during collection"
		if err != nil {
			reason = "Pod identity verification failed: " + err.Error()
		}
		for key := range result.metrics {
			if key != "tickRate" {
				failReadings(result.metrics, []string{key}, "unavailable", reason, nil)
			}
		}
		result.network = nil
		result.image = ""
		result.status = StatusUnknown
		return result
	}
	if ready := result.metrics["engineReady"]; ready.Value != nil {
		if *ready.Value == 1 {
			result.status = StatusOnline
		} else {
			result.status = StatusStarting
		}
	} else if result.status == StatusOnline {
		result.status = StatusAttention
	}
	return result
}

func sameContainer(target podTarget, pod telemetryPod) bool {
	if pod.Metadata.UID != target.pod.Metadata.UID || pod.Metadata.DeletionTimestamp != nil || pod.Status.Phase != "Running" {
		return false
	}
	count := 0
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == "server" {
			count++
			if status.ContainerID != target.containerID || status.State.Running == nil || !status.State.Running.StartedAt.Equal(target.startedAt) {
				return false
			}
		}
	}
	return count == 1
}

func quantityNumber(raw string) (float64, error) {
	if raw == "" {
		return 0, errors.New("missing quantity")
	}
	q, err := resource.ParseQuantity(raw)
	if err != nil || q.Sign() < 0 {
		return 0, errors.New("invalid quantity")
	}
	value := q.AsApproximateFloat64()
	if math.IsNaN(value) || math.IsInf(value, 0) || value > float64(math.MaxInt64) {
		return 0, errors.New("quantity out of range")
	}
	return value, nil
}

func (k *kubeOrchestrator) collectResources(ctx context.Context, target podTarget, metrics map[string]MetricReading) {
	path := "/apis/metrics.k8s.io/v1beta1/namespaces/" + url.PathEscape(target.pod.Metadata.Namespace) + "/pods/" + url.PathEscape(target.pod.Metadata.Name)
	var payload struct {
		Metadata   kubeMetadata `json:"metadata"`
		Timestamp  time.Time    `json:"timestamp"`
		Window     string       `json:"window"`
		Containers []struct {
			Name  string            `json:"name"`
			Usage map[string]string `json:"usage"`
		} `json:"containers"`
	}
	if err := k.kubeJSON(ctx, &payload, "get", "--raw", path); err != nil {
		failReadings(metrics, resourceKeys, "error", "Metrics API: "+err.Error(), nil)
		return
	}
	now := time.Now().UTC()
	window, err := time.ParseDuration(payload.Window)
	if payload.Metadata.Name != target.pod.Metadata.Name || payload.Metadata.Namespace != target.pod.Metadata.Namespace || (payload.Metadata.UID != "" && payload.Metadata.UID != target.pod.Metadata.UID) {
		failReadings(metrics, resourceKeys, "error", "Metrics API Pod identity mismatch", nil)
		return
	}
	if payload.Timestamp.IsZero() || payload.Timestamp.After(now.Add(5*time.Second)) || err != nil || window <= 0 || window > 5*time.Minute {
		failReadings(metrics, resourceKeys, "error", "Metrics API timestamp or window is invalid", nil)
		return
	}
	at := payload.Timestamp
	if now.Sub(at) > telemetryMaxAge {
		failReadings(metrics, resourceKeys, "stale", "Metrics API observation is older than 45 seconds", &at)
		return
	}
	if at.Add(-window).Before(target.startedAt) {
		failReadings(metrics, resourceKeys, "warming_up", "Metrics window predates this server container", &at)
		return
	}
	var usage map[string]string
	count := 0
	for _, container := range payload.Containers {
		if container.Name == "server" {
			count++
			usage = container.Usage
		}
	}
	if count != 1 {
		failReadings(metrics, resourceKeys, "error", "Metrics API must contain exactly one server container", &at)
		return
	}
	for _, item := range []struct{ resource, key string }{{"cpu", "cpuCores"}, {"memory", "memoryUsedBytes"}} {
		value, err := quantityNumber(usage[item.resource])
		if err != nil {
			failReadings(metrics, []string{item.key}, "error", "Invalid or missing "+item.resource+" usage", &at)
		} else {
			setReading(metrics, item.key, value, at)
		}
	}
	cpu, limit := metrics["cpuCores"], metrics["cpuLimitCores"]
	if cpu.Value != nil && limit.Value != nil && *limit.Value > 0 {
		setReading(metrics, "cpuPercent", 100**cpu.Value / *limit.Value, at)
	} else {
		reading := cpu
		if cpu.Value != nil {
			reading = limit
		}
		failReadings(metrics, []string{"cpuPercent"}, reading.Status, "CPU percentage requires valid usage and an observed nonzero limit", reading.ObservedAt)
	}
}

func (k *kubeOrchestrator) collectGame(ctx context.Context, target podTarget, metrics map[string]MetricReading) {
	for _, endpoint := range []string{"health", "players"} {
		keys := []string{"engineReady", "uptimeSeconds"}
		if endpoint == "players" {
			keys = []string{"players"}
		}
		data, err := k.podGameAPI(ctx, target, endpoint)
		at := time.Now().UTC()
		if err != nil {
			failReadings(metrics, keys, "error", "Game API "+endpoint+": "+err.Error(), &at)
			continue
		}
		if endpoint == "health" {
			var payload struct {
				EngineReady json.RawMessage `json:"engineReady"`
				Uptime      json.RawMessage `json:"uptimeSeconds"`
			}
			if json.Unmarshal(data, &payload) != nil {
				failReadings(metrics, keys, "error", "Invalid health payload", &at)
				continue
			}
			var readyValue *bool
			if json.Unmarshal(payload.EngineReady, &readyValue) != nil || readyValue == nil {
				failReadings(metrics, []string{"engineReady"}, "error", "Invalid or missing engineReady", &at)
			} else {
				ready := 0.0
				if *readyValue {
					ready = 1
				}
				setReading(metrics, "engineReady", ready, at)
			}
			var uptime *float64
			if json.Unmarshal(payload.Uptime, &uptime) != nil || uptime == nil {
				failReadings(metrics, []string{"uptimeSeconds"}, "error", "Invalid or missing uptimeSeconds", &at)
			} else {
				setReading(metrics, "uptimeSeconds", *uptime, at)
			}
		} else {
			var payload struct {
				Count *float64 `json:"count"`
			}
			if json.Unmarshal(data, &payload) != nil || payload.Count == nil || math.Trunc(*payload.Count) != *payload.Count || *payload.Count > float64(math.MaxInt32) {
				failReadings(metrics, keys, "error", "Invalid or missing player count", &at)
			} else {
				setReading(metrics, "players", *payload.Count, at)
			}
		}
	}
}

func (k *kubeOrchestrator) collectDisk(ctx context.Context, target podTarget, metrics map[string]MetricReading) {
	mount := ""
	count := 0
	for _, volume := range target.container.VolumeMounts {
		if volume.Name == "data" {
			mount = volume.MountPath
			count++
		}
	}
	if count != 1 || !strings.HasPrefix(mount, "/") {
		failReadings(metrics, diskKeys, "unavailable", "Expected one absolute data volume mount", nil)
		return
	}
	data, err := k.podExec(ctx, target, "env", "LC_ALL=C", "df", "-Pk", "--", mount)
	at := time.Now().UTC()
	if err != nil {
		failReadings(metrics, diskKeys, "error", "Data filesystem: "+err.Error(), &at)
		return
	}
	used, capacity, err := parseDisk(data)
	if err != nil {
		failReadings(metrics, diskKeys, "error", err.Error(), &at)
		return
	}
	setReading(metrics, "diskUsedBytes", used, at)
	setReading(metrics, "diskCapacityBytes", capacity, at)
	setReading(metrics, "diskPercent", 100*used/capacity, at)
}

func parseDisk(data []byte) (float64, float64, error) {
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "1024-blocks") {
		return 0, 0, errors.New("Invalid POSIX df output")
	}
	fields := strings.Fields(lines[1])
	if len(fields) < 6 {
		return 0, 0, errors.New("Invalid df filesystem row")
	}
	capacity, err1 := strconv.ParseUint(fields[1], 10, 64)
	used, err2 := strconv.ParseUint(fields[2], 10, 64)
	_, err3 := strconv.ParseUint(fields[3], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || capacity == 0 || used > capacity || capacity > math.MaxInt64/1024 {
		return 0, 0, errors.New("Invalid df capacity or usage")
	}
	return float64(used) * 1024, float64(capacity) * 1024, nil
}

type interfaceCounters struct{ rx, tx uint64 }
type networkCounters struct {
	identity   string
	at         time.Time
	interfaces map[string]interfaceCounters
}

func parseNetwork(data []byte, identity string, at time.Time) (networkCounters, error) {
	result := networkCounters{identity: identity, at: at, interfaces: map[string]interfaceCounters{}}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 3 || !strings.Contains(lines[0], "Receive") || !strings.Contains(lines[1], "bytes") {
		return result, errors.New("Invalid /proc/net/dev headers")
	}
	for _, line := range lines[2:] {
		name, raw, ok := strings.Cut(line, ":")
		name = strings.TrimSpace(name)
		fields := strings.Fields(raw)
		if !ok || name == "" || len(fields) != 16 {
			return result, errors.New("Invalid interface counter row")
		}
		values := [16]uint64{}
		for i, field := range fields {
			value, err := strconv.ParseUint(field, 10, 64)
			if err != nil {
				return result, errors.New("Invalid interface counter")
			}
			values[i] = value
		}
		if name == "lo" {
			continue
		}
		if _, exists := result.interfaces[name]; exists {
			return result, errors.New("Duplicate network interface")
		}
		result.interfaces[name] = interfaceCounters{values[0], values[8]}
	}
	if len(result.interfaces) == 0 {
		return result, errors.New("No non-loopback interfaces")
	}
	return result, nil
}

func applyNetwork(metrics map[string]MetricReading, previous *networkCounters, current networkCounters) {
	reason := "Waiting for a second counter sample"
	if previous != nil {
		elapsed := current.at.Sub(previous.at).Seconds()
		if previous.identity != current.identity {
			reason = "Pod or server container changed"
		} else if elapsed <= 0 || elapsed > telemetryMaxAge.Seconds() {
			reason = "Counter sampling interval is invalid or stale"
		} else if len(previous.interfaces) != len(current.interfaces) {
			reason = "Pod network interfaces changed"
		} else {
			rx, tx := 0.0, 0.0
			valid := true
			for name, counters := range current.interfaces {
				before, exists := previous.interfaces[name]
				if !exists || counters.rx < before.rx || counters.tx < before.tx {
					valid = false
					reason = "Network counters reset or interfaces changed"
					break
				}
				rx += float64(counters.rx-before.rx) / elapsed
				tx += float64(counters.tx-before.tx) / elapsed
			}
			if valid {
				setReading(metrics, "inboundBytesPerSecond", rx, current.at)
				setReading(metrics, "outboundBytesPerSecond", tx, current.at)
				setReading(metrics, "networkBytesPerSecond", rx+tx, current.at)
				return
			}
		}
	}
	failReadings(metrics, networkKeys, "warming_up", reason, &current.at)
}
