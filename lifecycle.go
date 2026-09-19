package main

import (
	"context"
	"errors"
	"net/http"
	"time"
)

var errServerStopped = errors.New("server is stopped; start it before this action")

const (
	stopSourceOperator = "operator"
	stopSourceObserved = "observed"
)

func (s State) stopped(id string) bool {
	server, ok := s.Servers[id]
	return ok && server.Status == StatusStopped
}

func (s State) operatorStopped(id string) bool {
	server, ok := s.Servers[id]
	return ok && server.Status == StatusStopped && server.StopSource != stopSourceObserved
}

func (s State) skipsAutomatedRestarts(id string) (bool, string) {
	if s.deleting(id) {
		return true, "Target server deletion is in progress or recorded"
	}
	if s.stopped(id) {
		return true, "Target server is stopped"
	}
	return false, ""
}

func cancelStoppedHealthDeliveries(state *State, id string) {
	for key, delivery := range state.Deliveries {
		if delivery.Event.ServerID != id || (delivery.Event.Kind != ServerDown && delivery.Event.Kind != ServerRecovered) {
			continue
		}
		if delivery.Status != DeliveryPending && delivery.Status != DeliveryRetry && delivery.Status != DeliverySending {
			continue
		}
		delivery.Status, delivery.Result, delivery.UpdatedAt = DeliveryFailed, "Cancelled because the operator stopped this server; an in-flight message may already have been sent", time.Now().UTC()
		state.Deliveries[key] = delivery
	}
}

func rejectStopped(w http.ResponseWriter, server Server) bool {
	if server.Status != StatusStopped {
		return false
	}
	writeError(w, http.StatusConflict, errServerStopped.Error())
	return true
}

func writeLifecycleError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errRebootConflict):
		writeError(w, http.StatusConflict, "a restart is already awaiting reconciliation")
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

func (a *App) persistStop(id string) (Server, error) {
	var server Server
	err := a.store.Update(func(state *State) error {
		current := state.Servers[id]
		if state.Producers[id].Restart != nil {
			return errRebootConflict
		}
		wasStopped := current.Status == StatusStopped
		current.Status = StatusStopped
		current.StopSource = stopSourceOperator
		if !wasStopped {
			p := state.Producers[id]
			p.Outage = false
			p.MemoryPressure = nil
			p.resetStreak()
			p.PlayerAt = time.Time{}
			p.HealthyBaseline = false
			state.Producers[id] = p
			cancelStoppedHealthDeliveries(state, current.ID)
			emitAlert(state, current, ServerStopped, "", "The operator parked this world; volume and inventory remain", time.Now().UTC())
		}
		state.Servers[id] = current
		server = current
		return nil
	})
	return server, err
}

func (a *App) persistStart(id string) (Server, bool, error) {
	var server Server
	started := false
	err := a.store.Update(func(state *State) error {
		current := state.Servers[id]
		if current.Status == StatusStopped {
			current.Status = StatusStarting
			current.StopSource = ""
			emitAlert(state, current, ServerStarted, "", "The operator started this world", time.Now().UTC())
			state.Servers[id] = current
			started = true
		}
		server = current
		return nil
	})
	return server, started, err
}

func (a *App) handleStop(w http.ResponseWriter, r *http.Request, server Server) {
	server, ok := a.lockServer(w, server.ID)
	if !ok {
		return
	}
	defer a.lifecycleMu.Unlock()
	server, err := a.persistStop(server.ID)
	if err != nil {
		writeLifecycleError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := a.orchestrator.Scale(ctx, server, 0); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, server)
}

func (a *App) handleStart(w http.ResponseWriter, r *http.Request, server Server) {
	server, ok := a.lockServer(w, server.ID)
	if !ok {
		return
	}
	defer a.lifecycleMu.Unlock()
	if a.store.Snapshot().Producers[server.ID].Restart != nil {
		writeError(w, http.StatusConflict, "a restart is already awaiting reconciliation")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := a.orchestrator.Scale(ctx, server, 1); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	server, started, err := a.persistStart(server.ID)
	if err != nil {
		writeLifecycleError(w, err)
		return
	}
	if started {
		a.markTelemetryPending(server)
	}
	if a.demo {
		if err := a.store.Update(func(state *State) error {
			current, ok := state.Servers[server.ID]
			if !ok {
				return nil
			}
			current.Status = StatusOnline
			current.LastSeen = time.Now().UTC().Format(time.RFC3339)
			state.Servers[server.ID] = current
			server = current
			return nil
		}); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, server)
}
