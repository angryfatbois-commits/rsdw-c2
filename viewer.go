package main

import (
	"math"
	"time"
)

type ViewerMetric struct {
	Value      *float64   `json:"value"`
	Status     string     `json:"status"`
	Source     string     `json:"source"`
	Unit       string     `json:"unit"`
	ObservedAt *time.Time `json:"observedAt"`
	Reason     string     `json:"reason"`
}

type ViewerHealthCheck struct {
	Name   string    `json:"name"`
	Status string    `json:"status"`
	At     time.Time `json:"at"`
}

func finiteMetric(value *float64) *float64 {
	if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 {
		return nil
	}
	return value
}

type ViewerServer struct {
	WorldName        string                  `json:"worldName"`
	ID               string                  `json:"id"`
	Name             string                  `json:"name"`
	Status           Status                  `json:"status"`
	MaxPlayers       int                     `json:"maxPlayers"`
	LastSeen         string                  `json:"lastSeen"`
	Metrics          map[string]ViewerMetric `json:"metrics"`
	MetricsAvailable bool                    `json:"metricsAvailable"`
}

func viewerServer(server Server) ViewerServer {
	status := StatusUnknown
	switch server.Status {
	case StatusOnline, StatusStarting, StatusAttention, StatusStopped, StatusDeleting, StatusStale:
		status = server.Status
	}
	return ViewerServer{ID: server.ID, Name: server.Name, WorldName: server.WorldName, Status: status, MaxPlayers: server.MaxPlayers, LastSeen: server.LastSeen, Metrics: viewerMetrics(server.Metrics), MetricsAvailable: server.MetricsAvailable}
}

func viewerMetricStatus(status string) string {
	switch status {
	case "available", "stale", "unsupported", "warming_up", "warming", "starting", "stopped", "unavailable":
		return status
	default:
		return "unavailable"
	}
}

func viewerMetrics(metrics map[string]MetricReading) map[string]ViewerMetric {
	result := make(map[string]ViewerMetric, len(metricCatalog))
	for _, definition := range metricCatalog {
		reading := metrics[definition.key]
		status := viewerMetricStatus(reading.Status)
		value, reason := finiteMetric(reading.Value), ""
		if value == nil && status == "available" {
			status = "unavailable"
		}
		if status != "available" {
			value, reason = nil, "Metric is not currently available"
		}
		result[definition.key] = ViewerMetric{Value: value, Status: status, Source: definition.source, Unit: definition.unit, ObservedAt: reading.ObservedAt, Reason: reason}
	}
	return result
}

type ViewerTelemetry struct {
	Server            ViewerServer            `json:"server"`
	PlayerRoster      PlayerRoster            `json:"playerRoster"`
	Metrics           map[string]ViewerMetric `json:"metrics"`
	MetricsAvailable  bool                    `json:"metricsAvailable"`
	Samples           []MetricSample          `json:"samples"`
	MetricDefinitions []MetricDefinition      `json:"metricDefinitions"`
	HealthChecks      []ViewerHealthCheck     `json:"healthChecks"`
}

func viewerPlayerRoster(roster PlayerRoster, players ViewerMetric) PlayerRoster {
	status := roster.Status
	if status == "" && roster.Players != nil {
		status = RosterAvailable
	}
	result := PlayerRoster{Status: status}
	switch status {
	case RosterAvailable:
		if players.Status != "available" || players.Value == nil || roster.Players == nil {
			return unavailableRoster("Player roster details are not currently available")
		}
		result.FreshForMs = roster.FreshForMs
		result.Players = make([]ConnectedPlayer, len(roster.Players))
		for i, player := range roster.Players {
			result.Players[i] = ConnectedPlayer{Name: player.Name, CharacterName: player.CharacterName}
		}
	case RosterError:
		result.Reason = "The game API returned an invalid player roster"
	case RosterUnavailable:
		result.Reason = "Player roster details are not currently available"
	default:
		return unavailableRoster("Player roster details are not currently available")
	}
	return result
}

func viewerTelemetry(telemetry Telemetry) ViewerTelemetry {
	result := ViewerTelemetry{Server: viewerServer(telemetry.Server), Metrics: viewerMetrics(telemetry.Metrics), MetricsAvailable: telemetry.MetricsAvailable, Samples: []MetricSample{}, MetricDefinitions: []MetricDefinition{}}
	result.PlayerRoster = viewerPlayerRoster(telemetry.PlayerRoster, result.Metrics["players"])
	result.HealthChecks = []ViewerHealthCheck{}
	if ready := result.Metrics["engineReady"]; ready.ObservedAt != nil {
		status := ready.Status
		if ready.Value != nil {
			status = "starting"
			if *ready.Value == 1 {
				status = "healthy"
			}
		}
		result.HealthChecks = append(result.HealthChecks, ViewerHealthCheck{Name: "Game API engine readiness", Status: status, At: *ready.ObservedAt})
	}
	for _, definition := range metricCatalog {
		result.MetricDefinitions = append(result.MetricDefinitions, MetricDefinition{Metric: definition.key, Description: definition.description})
	}
	for _, sample := range telemetry.Samples {
		at, _ := sample["timestamp"].(time.Time)
		view := MetricSample{"timestamp": at}
		times := map[string]*time.Time{}
		statuses := map[string]string{}
		reasons := map[string]string{}
		sourceTimes, _ := sample["observedAt"].(map[string]*time.Time)
		sourceStatuses, _ := sample["status"].(map[string]string)
		for _, definition := range metricCatalog {
			key := definition.key
			value, _ := sample[key].(*float64)
			value = finiteMetric(value)
			statuses[key] = viewerMetricStatus(sourceStatuses[key])
			if value == nil && statuses[key] == "available" {
				statuses[key] = "unavailable"
			}
			if statuses[key] != "available" {
				value = nil
				reasons[key] = "Metric is not currently available"
			}
			view[key], times[key] = value, sourceTimes[key]
		}
		view["observedAt"], view["status"], view["reason"] = times, statuses, reasons
		result.Samples = append(result.Samples, view)
	}
	return result
}
