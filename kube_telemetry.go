package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	telemetryCommandTimeout = 1500 * time.Millisecond
	maxPlayerResponseBytes  = 256 << 10
	maxPlayerRosterEntries  = 256
	maxPlayerFieldBytes     = 4096
)

type kubeMetadata struct {
	Labels            map[string]string `json:"labels"`
	Annotations       map[string]string `json:"annotations"`
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	UID               string            `json:"uid"`
	ResourceVersion   string            `json:"resourceVersion"`
	DeletionTimestamp *time.Time        `json:"deletionTimestamp"`
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
		Phase      string `json:"phase"`
		Conditions []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
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

type telemetryService struct {
	Spec struct {
		Type  string `json:"type"`
		Ports []struct {
			Name     string `json:"name"`
			Port     int    `json:"port"`
			NodePort int    `json:"nodePort"`
			Protocol string `json:"protocol"`
		} `json:"ports"`
	} `json:"spec"`
	Status struct {
		LoadBalancer struct {
			Ingress []struct {
				IP       string `json:"ip"`
				Hostname string `json:"hostname"`
			} `json:"ingress"`
		} `json:"loadBalancer"`
	} `json:"status"`
}

type telemetryNode struct {
	Status struct {
		Addresses []struct {
			Type    string `json:"type"`
			Address string `json:"address"`
		} `json:"addresses"`
	} `json:"status"`
}

// resolveEndpoint reads the chart's game Service and returns the address a
// player should connect to. It never errors: a resolution failure just
// leaves the endpoint unset, matching the other best-effort telemetry
// sources in this file.
func (k *kubeOrchestrator) resolveEndpoint(ctx context.Context, server Server) string {
	ctx, cancel := context.WithTimeout(ctx, telemetryCommandTimeout)
	defer cancel()
	var service telemetryService
	if err := k.kubeJSON(ctx, &service, "-n", server.Namespace, "get", "service", deploymentName(server.Release), "-o", "json"); err != nil {
		return ""
	}
	var port int
	for _, candidate := range service.Spec.Ports {
		if candidate.Name == "game" {
			port = candidate.Port
			break
		}
	}
	if port == 0 {
		return ""
	}
	host := ""
	for _, ingress := range service.Status.LoadBalancer.Ingress {
		if ingress.IP != "" {
			host = ingress.IP
			break
		}
		if ingress.Hostname != "" {
			host = ingress.Hostname
			break
		}
	}
	if host == "" && service.Spec.Type == "NodePort" {
		nodePort := 0
		for _, candidate := range service.Spec.Ports {
			if candidate.Name == "game" {
				nodePort = candidate.NodePort
				break
			}
		}
		if nodePort == 0 {
			return ""
		}
		var nodes struct {
			Items []telemetryNode `json:"items"`
		}
		if err := k.kubeJSON(ctx, &nodes, "get", "nodes", "-o", "json"); err != nil || len(nodes.Items) == 0 {
			return ""
		}
		for _, address := range nodes.Items[0].Status.Addresses {
			if address.Type == "ExternalIP" {
				host = address.Address
				break
			}
		}
		if host == "" {
			for _, address := range nodes.Items[0].Status.Addresses {
				if address.Type == "InternalIP" {
					host = address.Address
					break
				}
			}
		}
		port = nodePort
	}
	if host == "" {
		return ""
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
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
		return podTarget{}, StatusStopped, errScaledZero
	}
	var replicas struct {
		Items []struct {
			Metadata kubeMetadata `json:"metadata"`
		} `json:"items"`
	}
	if err := k.kubeJSON(ctx, &replicas, "-n", server.Namespace, "get", "replicasets", "-o", "json"); err != nil {
		return podTarget{}, StatusUnknown, err
	}
	if replicas.Items == nil {
		return podTarget{}, StatusUnknown, errors.New("ReplicaSet list is missing items")
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
	if pods.Items == nil {
		return podTarget{}, StatusUnknown, errors.New("Pod list is missing items")
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
		if len(candidates) == 0 {
			return podTarget{}, StatusStarting, errNoActivePod
		}
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
	if count > 1 {
		return podTarget{}, StatusUnknown, errors.New("duplicate server container status")
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
	if endpoint != "health" && endpoint != "players" && endpoint != "metrics" {
		return nil, errors.New("unsupported game API endpoint")
	}
	limit := ""
	if endpoint == "players" {
		limit = fmt.Sprintf(" --max-filesize %d", maxPlayerResponseBytes)
	}
	script := fmt.Sprintf("curl -fsS --max-time 3%s -H \"Authorization: Bearer $(cat /run/rsdwapi/token)\" http://127.0.0.1:%d/api/%s", limit, port, endpoint)
	return k.podExec(ctx, target, "sh", "-ec", script)
}

var resourceKeys = []string{"cpuCores", "cpuPercent", "memoryUsedBytes"}
var networkKeys = []string{"inboundBytesPerSecond", "outboundBytesPerSecond", "networkBytesPerSecond"}
var diskKeys = []string{"diskUsedBytes", "diskCapacityBytes", "diskPercent"}

var errScaledZero = errors.New("deployment is scaled to zero")
var errNoActivePod = errors.New("no active owned Pod")

func (k *kubeOrchestrator) collectObservation(ctx context.Context, server Server, previous *networkCounters) observation {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	result := observation{metrics: emptyMetrics(), playerRoster: unavailableRoster("Player roster was not collected"), status: StatusUnknown}
	target, status, err := k.resolvePod(ctx, server)
	result.status = status
	if err != nil {
		if errors.Is(err, errScaledZero) || errors.Is(err, errNoActivePod) {
			result.health = "unhealthy"
		}
		for key := range result.metrics {
			failReadings(result.metrics, []string{key}, "unavailable", "Pod discovery: "+err.Error(), nil)
		}
		return result
	}
	result.image = target.container.Image
	result.endpoint = k.resolveEndpoint(ctx, server)
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
		result.health = "unhealthy"
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
	result.playerRoster = k.collectGame(sourceCtx, target, result.metrics)
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
	k.collectTicks(sourceCtx, target, result.metrics)
	// kubectl exec pins a name, not a UID. Discard a collection spanning replacement.
	var after telemetryPod
	if err := k.kubeJSON(ctx, &after, "-n", target.pod.Metadata.Namespace, "get", "pod", target.pod.Metadata.Name, "-o", "json"); err != nil || !sameContainer(target, after) {
		reason := "Pod or server container changed during collection"
		if err != nil {
			reason = "Pod identity verification failed: " + err.Error()
		}
		for key := range result.metrics {
			failReadings(result.metrics, []string{key}, "unavailable", reason, nil)
		}
		result.network = nil
		result.playerRoster = unavailableRoster("Pod identity verification failed")
		result.image = ""
		result.status = StatusUnknown
		return result
	}
	for _, current := range after.Status.ContainerStatuses {
		if current.Name == "server" {
			target.ready = current.Ready
		}
	}
	podReady := false
	for _, condition := range after.Status.Conditions {
		if condition.Type == "Ready" {
			podReady = condition.Status == "True"
			break
		}
	}
	if ready := result.metrics["engineReady"]; ready.Value != nil {
		result.health = "unhealthy"
		if *ready.Value == 1 && target.ready && podReady {
			result.health = "healthy"
			result.status = StatusOnline
		} else {
			result.status = StatusStarting
		}
	} else if result.status == StatusOnline {
		result.status = StatusAttention
	}
	if !target.ready || !podReady {
		result.health = "unhealthy"
	}
	result.runtime, result.runtimeStarted = target.pod.Metadata.UID+"/"+target.containerID, target.startedAt
	result.restartOperation = target.pod.Metadata.Annotations[restartAnnotation]
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

func (k *kubeOrchestrator) collectGame(ctx context.Context, target podTarget, metrics map[string]MetricReading) PlayerRoster {
	roster := unavailableRoster("Player roster was not collected")
	for _, endpoint := range []string{"health", "players"} {
		keys := []string{"engineReady", "uptimeSeconds"}
		if endpoint == "players" {
			keys = []string{"players"}
		}
		data, err := k.podGameAPI(ctx, target, endpoint)
		at := time.Now().UTC()
		if err != nil {
			failReadings(metrics, keys, "error", "Game API "+endpoint+": "+err.Error(), &at)
			if endpoint == "players" {
				roster = unavailableRoster("The game API player roster could not be collected")
			}
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
			if len(data) > maxPlayerResponseBytes {
				failReadings(metrics, keys, "error", "Game API players response exceeded the size limit", &at)
				roster = errorRoster("The game API player roster exceeded the size limit")
				continue
			}
			var payload struct {
				Count   *float64        `json:"count"`
				Players json.RawMessage `json:"players"`
			}
			if json.Unmarshal(data, &payload) != nil || payload.Count == nil || math.Trunc(*payload.Count) != *payload.Count || *payload.Count > float64(math.MaxInt32) {
				failReadings(metrics, keys, "error", "Invalid or missing player count", &at)
				roster = unavailableRoster("The game API player count was not available")
			} else {
				setReading(metrics, "players", *payload.Count, at)
				if metrics["players"].Status == "available" {
					roster = decodePlayerRoster(payload.Players)
				}
			}
		}
	}
	return roster
}

func decodePlayerRoster(data json.RawMessage) PlayerRoster {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return unavailableRoster("The game API did not provide player roster details")
	}
	var players []*ConnectedPlayer
	if json.Unmarshal(trimmed, &players) != nil || players == nil {
		return errorRoster("The game API returned an invalid player roster")
	}
	if len(players) > maxPlayerRosterEntries {
		return errorRoster("The game API player roster exceeded the entry limit")
	}
	roster := PlayerRoster{Status: RosterAvailable, Players: make([]ConnectedPlayer, len(players))}
	for i, player := range players {
		if player == nil {
			return errorRoster("The game API returned an invalid player roster")
		}
		player.Name = strings.TrimSpace(player.Name)
		player.CharacterName = strings.TrimSpace(player.CharacterName)
		if len([]byte(player.Name)) > maxPlayerFieldBytes || len([]byte(player.CharacterName)) > maxPlayerFieldBytes {
			return errorRoster("The game API player roster field exceeded the size limit")
		}
		roster.Players[i] = *player
	}
	return roster
}

func (k *kubeOrchestrator) collectTicks(ctx context.Context, target podTarget, metrics map[string]MetricReading) {
	started := time.Now()
	data, err := k.podGameAPI(ctx, target, "metrics")
	received := time.Now()
	if err != nil {
		status := "error"
		if strings.Contains(err.Error(), "requested URL returned error: 404") {
			status = "unsupported"
		}
		failReadings(metrics, tickKeys, status, "Game API metrics: "+err.Error(), nil)
		return
	}
	applyTicks(metrics, data, received.UTC(), received.Sub(started))
}

func applyTicks(metrics map[string]MetricReading, data []byte, now time.Time, transport time.Duration) {
	var payload struct {
		SchemaVersion int `json:"schemaVersion"`
		Tick          *struct {
			Status     string   `json:"status"`
			Source     string   `json:"source"`
			Scope      string   `json:"scope"`
			Reason     string   `json:"reason"`
			Window     *float64 `json:"windowSeconds"`
			Count      *uint64  `json:"sampleCount"`
			Age        *float64 `json:"lastSampleAgeSeconds"`
			ObservedAt *int64   `json:"observedAtUnixMs"`
			Rate       *float64 `json:"rateHz"`
			Execution  *struct {
				P50 *float64 `json:"p50"`
				P95 *float64 `json:"p95"`
				P99 *float64 `json:"p99"`
			} `json:"executionMs"`
		} `json:"tick"`
	}
	fail := func(status, reason string, at *time.Time) {
		failReadings(metrics, tickKeys, status, reason, at)
	}
	if json.Unmarshal(data, &payload) != nil || payload.SchemaVersion != 1 || payload.Tick == nil {
		fail("error", "Invalid or missing schemaVersion 1 tick snapshot", nil)
		return
	}
	tick := payload.Tick
	if tick.Source != "engine_tick_hook" || tick.Scope != "UDomGameEngine::Tick" {
		fail("error", "Unverified tick source or scope", nil)
		return
	}
	switch tick.Status {
	case "ready":
	case "warming", "unsupported", "stale", "faulted", "stopped", "starting":
		fail(tick.Status, defaultValue(tick.Reason, "Tick source is "+tick.Status), nil)
		return
	default:
		fail("error", "Invalid or missing tick status", nil)
		return
	}
	if tick.Execution == nil || tick.Count == nil || *tick.Count == 0 || *tick.Count > 8192 || tick.ObservedAt == nil || *tick.ObservedAt <= 0 {
		fail("error", "Missing or invalid tick count, timestamp or execution snapshot", nil)
		return
	}
	for _, value := range []*float64{tick.Window, tick.Rate, tick.Age, tick.Execution.P50, tick.Execution.P95, tick.Execution.P99} {
		if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 {
			fail("error", "Missing or invalid non-negative tick measurement", nil)
			return
		}
	}
	expectedRate := float64(*tick.Count) / *tick.Window
	if *tick.Window < 1 || math.IsInf(expectedRate, 0) || math.Abs(expectedRate-*tick.Rate) > math.Max(0.00001, expectedRate*0.00001) || *tick.Execution.P50 > *tick.Execution.P95 || *tick.Execution.P95 > *tick.Execution.P99 || *tick.Execution.P99 > *tick.Window*1000+0.00001 {
		fail("error", "Incoherent tick rate, window or percentile order", nil)
		return
	}
	rank := func(percent uint64) uint64 { return (percent**tick.Count + 99) / 100 }
	if (rank(50) == rank(95) && *tick.Execution.P50 != *tick.Execution.P95) || (rank(95) == rank(99) && *tick.Execution.P95 != *tick.Execution.P99) {
		fail("error", "Coincident percentile ranks have different durations", nil)
		return
	}
	at := time.UnixMilli(*tick.ObservedAt).UTC()
	age := now.Sub(at).Seconds()
	if transport < 0 || transport > tickTransportLimit || *tick.Age > tickMaxAge.Seconds() || age > (tickMaxAge+transport).Seconds()+0.001 {
		fail("stale", "Tick observation exceeds the two-second freshness window and bounded transport", &at)
		return
	}
	if age < -0.1 || age-*tick.Age < -0.1 || age-*tick.Age > transport.Seconds()+0.1 {
		fail("error", "Tick timestamp disagrees with source age", &at)
		return
	}
	for key, value := range map[string]float64{
		"tickRate": *tick.Rate, "tickP50Ms": *tick.Execution.P50,
		"tickP95Ms": *tick.Execution.P95, "tickP99Ms": *tick.Execution.P99,
		"tickWindowSeconds": *tick.Window, "tickSampleCount": float64(*tick.Count),
	} {
		setReading(metrics, key, value, at)
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
