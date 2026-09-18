package main

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

const restartAnnotation = "rsdw-c2/restart-operation"
const restartTimeout = 5 * time.Minute

type RestartOperation struct {
	ID               string    `json:"id"`
	Runtime          string    `json:"runtime"`
	RequestedAt      time.Time `json:"requestedAt"`
	CommandUncertain bool      `json:"commandUncertain"`
}

type AlertProducer struct {
	Runtime         string               `json:"runtime"`
	LastAt          time.Time            `json:"lastAt"`
	PlayerAt        time.Time            `json:"playerAt"`
	Players         int                  `json:"players"`
	PlayerLimit     int                  `json:"playerLimit"`
	HealthyBaseline bool                 `json:"healthyBaseline"`
	Outage          bool                 `json:"outage"`
	Streak          string               `json:"streak"`
	StreakCount     int                  `json:"streakCount"`
	StreakSince     time.Time            `json:"streakSince"`
	Restart         *RestartOperation    `json:"restart,omitempty"`
	MemoryPressure  *MemoryPressureState `json:"memoryPressure,omitempty"`
}

func (p *AlertProducer) resetStreak() {
	p.Streak, p.StreakCount, p.StreakSince = "", 0, time.Time{}
}

func observeAlerts(state *State, server Server, o observation, now time.Time) {
	if _, ok := state.Servers[server.ID]; !ok || state.deleting(server.ID) || state.stopped(server.ID) {
		return
	}
	p := state.Producers[server.ID]
	if o.at.IsZero() || !o.at.After(p.LastAt) {
		return
	}
	if now.Sub(o.at) > telemetryMaxAge || o.at.After(now) {
		return
	}
	if o.at.Sub(p.LastAt) > telemetryMaxAge {
		p.resetStreak()
		p.PlayerAt = time.Time{}
	}
	p.LastAt = o.at
	defer func() { state.Producers[server.ID] = p }()
	if p.Restart != nil {
		p.MemoryPressure = nil
		op := p.Restart
		p.resetStreak()
		p.PlayerAt = time.Time{}
		if !o.at.After(op.RequestedAt) {
			return
		}
		// Kubernetes container start times have second precision.
		if o.health == "healthy" && o.runtime != "" && o.runtime != op.Runtime && o.restartOperation == op.ID && !o.runtimeStarted.Before(op.RequestedAt.Truncate(time.Second)) {
			emitAlert(state, server, RestartCompleted, op.ID, "Replacement runtime is ready", o.at)
			finishRebootOccurrences(state, op.ID, rebootCompleted, "Replacement runtime is ready", o.at)
			p.Restart, p.Runtime, p.HealthyBaseline = nil, o.runtime, true
			current := state.Servers[server.ID]
			current.Status = StatusOnline
			state.Servers[server.ID] = current
		} else if o.at.Sub(op.RequestedAt) >= restartTimeout && o.health != "" {
			emitAlert(state, server, RestartFailed, op.ID, "No ready marked replacement confirmed within the restart deadline", o.at)
			finishRebootOccurrences(state, op.ID, rebootFailed, "No ready marked replacement confirmed within the restart deadline", o.at)
			p.Restart = nil
			current := state.Servers[server.ID]
			current.Status = StatusAttention
			state.Servers[server.ID] = current
		} else {
			return
		}
		return
	}
	if o.runtime != p.Runtime {
		p.PlayerAt = time.Time{}
		p.Runtime = o.runtime
	}
	reading := freshMetrics(o.metrics, now)["players"]
	if o.health == "healthy" && o.runtime != "" && reading.Value != nil && reading.Status == "available" && reading.ObservedAt != nil {
		at, count := *reading.ObservedAt, int(*reading.Value)
		if !p.PlayerAt.IsZero() && at.After(p.PlayerAt) && at.Sub(p.PlayerAt) <= telemetryMaxAge {
			if count > p.Players {
				emitAlert(state, server, PlayerJoined, "", fmt.Sprintf("Approximate increase of %d players (%d to %d); identities and joins between polls are unknown", count-p.Players, p.Players, count), o.at)
			}
			if p.PlayerLimit == server.MaxPlayers && server.MaxPlayers > 0 && p.Players < server.MaxPlayers && count >= server.MaxPlayers {
				emitAlert(state, server, PlayerLimitReached, "", fmt.Sprintf("Player count reached %d / %d", count, server.MaxPlayers), o.at)
			}
		}
		if at.After(p.PlayerAt) {
			p.Players, p.PlayerLimit, p.PlayerAt = count, server.MaxPlayers, at
		}
	} else {
		p.PlayerAt = time.Time{}
	}
	if o.health == "" {
		p.resetStreak()
		return
	}
	if o.health == "healthy" && !p.HealthyBaseline {
		p.HealthyBaseline = true
		p.resetStreak()
		return
	}
	if !p.HealthyBaseline {
		return
	}
	if p.Streak != o.health {
		p.Streak, p.StreakCount, p.StreakSince = o.health, 0, o.at
	}
	p.StreakCount++
	if !p.Outage && o.health == "unhealthy" && p.StreakCount >= 3 && o.at.Sub(p.StreakSince) >= 30*time.Second {
		p.Outage = true
		p.resetStreak()
		emitAlert(state, server, ServerDown, "", "Confirmed unhealthy after a healthy baseline", o.at)
	} else if p.Outage && o.health == "healthy" && p.StreakCount >= 2 && o.at.Sub(p.StreakSince) >= 15*time.Second {
		p.Outage = false
		p.resetStreak()
		emitAlert(state, server, ServerRecovered, "", "Confirmed healthy after an established outage", o.at)
	}
}

func (a *App) handleRestart(w http.ResponseWriter, r *http.Request, server Server) {
	server, ok := a.lockServer(w, server.ID)
	if !ok {
		return
	}
	defer a.lifecycleMu.Unlock()
	if rejectStopped(w, server) {
		return
	}
	dispatch, err := a.claimManualRestart(server.ID, a.rebootNow())
	if err != nil {
		if errors.Is(err, errRebootConflict) {
			writeError(w, http.StatusConflict, "a restart is already awaiting reconciliation")
		} else if errors.Is(err, errRebootNotFound) {
			writeError(w, http.StatusNotFound, "server not found")
		} else {
			writeError(w, http.StatusInternalServerError, "could not persist restart operation")
		}
		return
	}
	commandErr, persistenceErr := a.dispatchRestartOperation(r.Context(), dispatch)
	if persistenceErr != nil {
		writeError(w, http.StatusInternalServerError, "restart is recorded; result persistence failed and requires reconciliation")
		return
	}
	if commandErr != nil {
		writeJSON(w, http.StatusAccepted, map[string]any{"operationId": dispatch.op.ID, "status": "awaiting_reconciliation", "message": "Restart command outcome is unknown; fresh observations will reconcile it"})
		return
	}
	writeJSON(w, http.StatusOK, dispatch.server)
}
