package main

import (
	"context"
	"log"
	"math"
	"sync"
	"time"
)

const (
	telemetryMaxAge     = 45 * time.Second
	telemetryRetention  = time.Hour
	telemetryMaxSamples = 240
	tickMaxAge          = 2 * time.Second
	tickTransportLimit  = 1500 * time.Millisecond
)

var tickKeys = []string{"tickRate", "tickP50Ms", "tickP95Ms", "tickP99Ms", "tickWindowSeconds", "tickSampleCount"}

const tickSource = "game API /api/metrics engine_tick_hook UDomGameEngine::Tick"

type MetricReading struct {
	Value      *float64   `json:"value"`
	Status     string     `json:"status"`
	Source     string     `json:"source"`
	Unit       string     `json:"unit"`
	ObservedAt *time.Time `json:"observedAt"`
	Reason     string     `json:"reason"`
}

type ConnectedPlayer struct {
	Name          string `json:"name"`
	CharacterName string `json:"characterName"`
}

type RosterStatus string

const (
	RosterAvailable   RosterStatus = "available"
	RosterUnavailable RosterStatus = "unavailable"
	RosterError       RosterStatus = "error"
)

type PlayerRoster struct {
	Status     RosterStatus      `json:"status,omitempty"`
	Reason     string            `json:"reason,omitempty"`
	FreshForMs int64             `json:"freshForMs,omitempty"`
	Players    []ConnectedPlayer `json:"players"`
}

func unavailableRoster(reason string) PlayerRoster {
	return PlayerRoster{Status: RosterUnavailable, Reason: reason}
}

func errorRoster(reason string) PlayerRoster {
	return PlayerRoster{Status: RosterError, Reason: reason}
}

var metricCatalog = []struct{ key, unit, source, description string }{
	{"players", "players", "game API /api/players", "Connected players reported by the authenticated game API."},
	{"uptimeSeconds", "seconds", "game API /api/health", "Game API process uptime."},
	{"engineReady", "boolean", "game API /api/health", "Engine readiness reported by the game API, 0 or 1."},
	{"cpuCores", "cores", "metrics.k8s.io", "CPU cores used by the selected server container."},
	{"cpuPercent", "percent", "metrics.k8s.io / Pod limits", "CPU usage divided by the selected container CPU limit."},
	{"cpuLimitCores", "cores", "Kubernetes Pod", "CPU limit of the observed server container."},
	{"memoryUsedBytes", "bytes", "metrics.k8s.io", "Working set memory of the selected server container."},
	{"memoryLimitBytes", "bytes", "Kubernetes Pod", "Memory limit of the observed server container."},
	{"diskUsedBytes", "bytes", "Pod df data mount", "Used space on the data mount backing filesystem."},
	{"diskCapacityBytes", "bytes", "Pod df data mount", "Total capacity of the data mount backing filesystem."},
	{"diskPercent", "percent", "Pod df data mount", "Used space divided by data filesystem capacity."},
	{"inboundBytesPerSecond", "bytes/second", "Pod /proc/net/dev", "Received bytes per second across non-loopback pod interfaces."},
	{"outboundBytesPerSecond", "bytes/second", "Pod /proc/net/dev", "Transmitted bytes per second across non-loopback pod interfaces."},
	{"networkBytesPerSecond", "bytes/second", "Pod /proc/net/dev", "Combined received and transmitted pod bytes per second."},
	{"tickRate", "ticks/second", tickSource, "Completed UDomGameEngine::Tick calls divided by the observed window."},
	{"tickP50Ms", "milliseconds", tickSource, "Median elapsed execution time inside UDomGameEngine::Tick."},
	{"tickP95Ms", "milliseconds", tickSource, "95th percentile elapsed execution time inside UDomGameEngine::Tick."},
	{"tickP99Ms", "milliseconds", tickSource, "99th percentile elapsed execution time inside UDomGameEngine::Tick."},
	{"tickWindowSeconds", "seconds", tickSource, "Actual observation window for the tick snapshot."},
	{"tickSampleCount", "ticks", tickSource, "Completed calls represented by the tick snapshot."},
}

func emptyMetrics() map[string]MetricReading {
	metrics := make(map[string]MetricReading, len(metricCatalog))
	for _, item := range metricCatalog {
		metrics[item.key] = MetricReading{Status: "unavailable", Source: item.source, Unit: item.unit, Reason: "No observation collected"}
	}
	return metrics
}

func setReading(metrics map[string]MetricReading, key string, value float64, at time.Time) {
	reading := metrics[key]
	reading.ObservedAt = &at
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		reading.Value, reading.Status, reading.Reason = nil, "error", "Source returned an invalid non-negative number"
	} else {
		reading.Value, reading.Status, reading.Reason = &value, "available", ""
	}
	metrics[key] = reading
}

func failReadings(metrics map[string]MetricReading, keys []string, status, reason string, at *time.Time) {
	for _, key := range keys {
		reading := metrics[key]
		reading.Value, reading.Status, reading.Reason, reading.ObservedAt = nil, status, reason, at
		metrics[key] = reading
	}
}

func freshMetrics(metrics map[string]MetricReading, now time.Time) map[string]MetricReading {
	result := emptyMetrics()
	for key, reading := range metrics {
		if reading.Status == "available" && (reading.ObservedAt == nil || now.Sub(*reading.ObservedAt) > telemetryMaxAge || reading.ObservedAt.After(now.Add(5*time.Second))) {
			reading.Value, reading.Status, reading.Reason = nil, "stale", "Source observation is outside the freshness window"
		}
		result[key] = reading
	}
	return result
}

type observation struct {
	runtime          string
	runtimeStarted   time.Time
	restartOperation string
	health           string
	at               time.Time
	metrics          map[string]MetricReading
	playerRoster     PlayerRoster
	status           Status
	image            string
	network          *networkCounters
}

type telemetryStore struct {
	mu         sync.RWMutex
	collecting sync.Mutex
	history    map[string][]observation
}

func (a *App) observations() *telemetryStore {
	a.telemetryOnce.Do(func() { a.telemetry = &telemetryStore{history: make(map[string][]observation)} })
	return a.telemetry
}

func (a *App) runCollector(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		a.collectTelemetry(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *App) collectTelemetry(ctx context.Context) {
	cache := a.observations()
	if !cache.collecting.TryLock() {
		return
	}
	defer cache.collecting.Unlock()
	snapshot := a.store.Snapshot()
	cache.mu.Lock()
	for id := range cache.history {
		if _, exists := snapshot.Servers[id]; !exists {
			delete(cache.history, id)
		}
	}
	cache.mu.Unlock()
	jobs := make(chan Server)
	var workers sync.WaitGroup
	for i := 0; i < min(4, len(snapshot.Servers)); i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for server := range jobs {
				cache.mu.RLock()
				history := cache.history[server.ID]
				historyLength := len(history)
				var historyAt time.Time
				if historyLength > 0 {
					historyAt = history[historyLength-1].at
				}
				var previous *networkCounters
				if historyLength > 0 {
					previous = history[historyLength-1].network
				}
				cache.mu.RUnlock()
				result := observation{metrics: emptyMetrics(), status: StatusUnknown}
				if k, ok := a.orchestrator.(*kubeOrchestrator); ok && !a.demo {
					result = k.collectObservation(ctx, server, previous)
				}
				result.at = time.Now().UTC()
				if a.demo {
					result.runtime = "demo/" + server.ID + "/" + server.LastRestart
					result.status = server.Status
					for key, value := range map[string]int64{"memoryUsedBytes": server.MemoryUsedBytes, "memoryLimitBytes": server.MemoryLimitBytes} {
						setReading(result.metrics, key, float64(value), result.at)
						reading := result.metrics[key]
						reading.Source = "Demo seeded server"
						result.metrics[key] = reading
					}
				}
				if !a.lifecycleMu.TryLock() {
					continue
				}
				current := a.store.Snapshot()
				if _, ok := current.Servers[server.ID]; !ok || current.deleting(server.ID) {
					a.lifecycleMu.Unlock()
					continue
				}
				cache.mu.Lock()
				history = cache.history[server.ID]
				if len(history) != historyLength || historyLength > 0 && !history[len(history)-1].at.Equal(historyAt) {
					cache.mu.Unlock()
					a.lifecycleMu.Unlock()
					continue
				}
				cache.history[server.ID] = retainObservations(history, result)
				cache.mu.Unlock()
				var dispatch rebootDispatch
				if err := a.store.Update(func(state *State) error {
					if current, ok := state.Servers[server.ID]; ok {
						now := time.Now().UTC()
						observeAlerts(state, current, result, now)
						var err error
						dispatch, err = a.memoryPressure.observe(state, current, result, now)
						if err == nil {
							cancelObsoleteWarnings(state, now)
						}
						return err
					}
					return nil
				}); err != nil {
					log.Print("could not persist alert observations")
				} else if dispatch.server.ID != "" {
					commandErr, persistenceErr := a.dispatchRestartOperation(ctx, dispatch)
					if persistenceErr != nil {
						log.Printf("memory pressure restart result for %s: %v", server.ID, persistenceErr)
					} else if commandErr != nil {
						log.Printf("memory pressure restart command for %s: %v", server.ID, commandErr)
					}
				}
				a.lifecycleMu.Unlock()
			}
		}()
	}
	for _, server := range snapshot.Servers {
		select {
		case jobs <- server:
		case <-ctx.Done():
			close(jobs)
			workers.Wait()
			return
		}
	}
	close(jobs)
	workers.Wait()
}

func joinObservation(server Server, result observation, now time.Time) Server {
	lifecycleStatus := server.Status
	server.Metrics = freshMetrics(result.metrics, now)
	server.MetricsAvailable = false
	server.Players, server.UptimeSeconds, server.MemoryUsedBytes, server.MemoryLimitBytes, server.NetworkBytesPerSecond = 0, 0, 0, 0, 0
	server.CPUPercent, server.DiskPercent, server.TickRate = 0, 0, 0
	for key, reading := range server.Metrics {
		if reading.Value == nil {
			continue
		}
		server.MetricsAvailable = true
		switch key {
		case "players":
			server.Players = int(*reading.Value)
		case "uptimeSeconds":
			server.UptimeSeconds = int64(*reading.Value)
		case "cpuPercent":
			server.CPUPercent = *reading.Value
		case "memoryUsedBytes":
			server.MemoryUsedBytes = int64(*reading.Value)
		case "memoryLimitBytes":
			server.MemoryLimitBytes = int64(*reading.Value)
		case "diskPercent":
			server.DiskPercent = *reading.Value
		case "networkBytesPerSecond":
			server.NetworkBytesPerSecond = int64(*reading.Value)
		case "tickRate":
			server.TickRate = *reading.Value
		}
	}
	server.Status = StatusUnknown
	server.LastSeen = ""
	if !result.at.IsZero() && now.Sub(result.at) <= telemetryMaxAge {
		server.Status = result.status
		server.LastSeen = result.at.Format(time.RFC3339Nano)
	}
	if lifecycleStatus == StatusDeleting || lifecycleStatus == StatusStale || lifecycleStatus == StatusStopped {
		server.Status = lifecycleStatus
	}
	if result.image != "" {
		server.CurrentImage = result.image
	}
	server.UpdateAvailable = server.CurrentImage != "" && server.DesiredImage != "" && server.CurrentImage != server.DesiredImage
	return server
}

func (a *App) markTelemetryPending(server Server) {
	pending := observation{at: time.Now().UTC(), metrics: emptyMetrics(), status: StatusStarting, image: server.CurrentImage}
	cache := a.observations()
	cache.mu.Lock()
	history := cache.history[server.ID]
	cache.history[server.ID] = retainObservations(history, pending)
	cache.mu.Unlock()
}

func retainObservations(history []observation, next observation) []observation {
	first := 0
	if !next.at.IsZero() {
		for first < len(history) && !history[first].at.After(next.at.Add(-telemetryRetention)) {
			first++
		}
	}
	history = history[first:]
	if len(history) >= telemetryMaxSamples {
		history = history[len(history)-telemetryMaxSamples+1:]
	}
	retained := make([]observation, len(history)+1)
	copy(retained, history)
	for i := range history {
		retained[i].playerRoster = PlayerRoster{}
	}
	retained[len(history)] = next
	return retained
}

func playerRosterFreshForMs(reading MetricReading, now time.Time) int64 {
	if reading.Status != "available" || reading.ObservedAt == nil {
		return 0
	}
	remaining := telemetryMaxAge - now.Sub(*reading.ObservedAt)
	if remaining <= 0 {
		return 0
	}
	if remaining > telemetryMaxAge {
		remaining = telemetryMaxAge
	}
	return remaining.Milliseconds()
}

func (a *App) telemetryFor(server Server, requestedRange string) Telemetry {
	now := time.Now().UTC()
	duration := time.Minute
	if requestedRange == "5m" {
		duration = 5 * time.Minute
	}
	if requestedRange == "1h" {
		duration = time.Hour
	}
	cache := a.observations()
	cache.mu.RLock()
	history := append([]observation(nil), cache.history[server.ID]...)
	cache.mu.RUnlock()
	current := observation{metrics: emptyMetrics(), status: StatusUnknown}
	if len(history) > 0 {
		current = history[len(history)-1]
	}
	server = joinObservation(server, current, now)
	result := Telemetry{Server: server, Metrics: server.Metrics, MetricsAvailable: server.MetricsAvailable, Samples: []MetricSample{}, HealthChecks: []HealthCheck{}, MetricDefinitions: []MetricDefinition{}}
	result.PlayerRoster = unavailableRoster("Player count is not currently available")
	if players := result.Metrics["players"]; players.Status == "available" && players.Value != nil {
		result.PlayerRoster = current.playerRoster
		if result.PlayerRoster.Status == "" {
			if result.PlayerRoster.Players == nil {
				result.PlayerRoster = unavailableRoster("Player roster details were not provided")
			} else {
				result.PlayerRoster.Status = RosterAvailable
			}
		}
		if result.PlayerRoster.Status == RosterAvailable {
			result.PlayerRoster.FreshForMs = playerRosterFreshForMs(players, now)
		} else {
			result.PlayerRoster.FreshForMs = 0
		}
	} else if players.Status == "stale" {
		result.PlayerRoster = unavailableRoster("Player count is stale")
	} else if players.Status == "error" {
		result.PlayerRoster = unavailableRoster("Player count collection failed")
	}
	for _, item := range metricCatalog {
		result.MetricDefinitions = append(result.MetricDefinitions, MetricDefinition{Metric: item.key, Description: item.description})
	}
	for _, item := range history {
		if item.at.Before(now.Add(-duration)) {
			continue
		}
		sample := MetricSample{"timestamp": item.at}
		times := map[string]*time.Time{}
		statuses := map[string]string{}
		reasons := map[string]string{}
		for key, reading := range freshMetrics(item.metrics, item.at) {
			sample[key] = reading.Value
			times[key], statuses[key], reasons[key] = reading.ObservedAt, reading.Status, reading.Reason
		}
		sample["observedAt"], sample["status"], sample["reason"] = times, statuses, reasons
		result.Samples = append(result.Samples, sample)
	}
	if ready := server.Metrics["engineReady"]; ready.ObservedAt != nil {
		status := ready.Status
		if ready.Value != nil {
			status = "starting"
			if *ready.Value == 1 {
				status = "healthy"
			}
		}
		result.HealthChecks = append(result.HealthChecks, HealthCheck{Name: "Game API engine readiness", Status: status, At: *ready.ObservedAt})
	}
	return result
}
