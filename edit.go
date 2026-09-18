package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

type EditServerRequest struct {
	Name             *string `json:"name"`
	WorldName        *string `json:"worldName"`
	MaxPlayers       *int    `json:"maxPlayers"`
	MemoryLimitMiB   *int    `json:"memoryLimitMiB"`
	CPULimitMillis   *int    `json:"cpuLimitMillis"`
	Confirm          bool    `json:"confirm"`
	ConfirmWorldName bool    `json:"confirmWorldName"`
}

func (a *App) handleEditSettings(w http.ResponseWriter, r *http.Request, id string) {
	var request EditServerRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid settings JSON: "+err.Error())
		return
	}
	if decoder.Decode(new(any)) != io.EOF {
		writeError(w, http.StatusBadRequest, "expected one JSON object")
		return
	}
	if !request.Confirm || (request.Name == nil && request.WorldName == nil && request.MaxPlayers == nil && request.MemoryLimitMiB == nil && request.CPULimitMillis == nil) {
		writeError(w, http.StatusBadRequest, "at least one setting and confirm: true are required")
		return
	}
	for _, field := range []struct {
		name  string
		value *string
		max   int
	}{{"name", request.Name, 48}, {"worldName", request.WorldName, 2048}} {
		if field.value != nil && (strings.TrimSpace(*field.value) == "" || len(*field.value) > field.max || strings.ContainsAny(*field.value, "\x00\r\n")) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("%s must be a nonempty single line of at most %d bytes", field.name, field.max))
			return
		}
	}
	for _, field := range []struct {
		name     string
		value    *int
		min, max int
	}{{"maxPlayers", request.MaxPlayers, 1, 64}, {"memoryLimitMiB", request.MemoryLimitMiB, 256, 67584}, {"cpuLimitMillis", request.CPULimitMillis, 100, 64000}} {
		if field.value != nil && (*field.value < field.min || *field.value > field.max) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("%s must be between %d and %d", field.name, field.min, field.max))
			return
		}
	}
	server, ok := a.lockServer(w, id)
	if !ok {
		return
	}
	defer a.lifecycleMu.Unlock()
	if server.Status == StatusStopped {
		writeStoppedConflict(w)
		return
	}
	if request.WorldName != nil && strings.TrimSpace(*request.WorldName) != defaultValue(server.WorldName, server.Name) && !request.ConfirmWorldName {
		writeError(w, http.StatusBadRequest, "changing worldName requires confirmWorldName: true; C2 does not rename or migrate saved world data and this game build's save-selection behavior is unverified")
		return
	}
	if request.Name != nil {
		if request.WorldName == nil && server.WorldName == "" {
			server.WorldName = server.Name
		}
		server.Name = strings.TrimSpace(*request.Name)
	}
	if request.WorldName != nil {
		server.WorldName = strings.TrimSpace(*request.WorldName)
	}
	if request.MaxPlayers != nil {
		server.MaxPlayers = *request.MaxPlayers
	}
	if request.MemoryLimitMiB != nil {
		server.MemoryLimitMiB = *request.MemoryLimitMiB
	}
	if request.CPULimitMillis != nil {
		server.CPULimitMillis = *request.CPULimitMillis
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	observed, err := a.orchestrator.Refresh(ctx, server)
	if err != nil || observed.CurrentImage == "" {
		if err == nil {
			err = fmt.Errorf("currently running image is unavailable")
		}
		log.Printf("settings image lookup failed for server %s, release %s/%s: %v", server.ID, server.Namespace, server.Release, err)
		writeError(w, http.StatusBadGateway, fmt.Sprintf("Could not determine the currently running image for server %s, release %s/%s. Settings were not applied; inspect the existing release and retry.", server.ID, server.Namespace, server.Release))
		return
	}
	server.CurrentImage = observed.CurrentImage
	server.UpdateAvailable = server.DesiredImage != "" && server.CurrentImage != server.DesiredImage
	deployment := server
	deployment.DesiredImage = server.CurrentImage
	if err := a.orchestrator.Deploy(ctx, deployment); err != nil {
		log.Printf("settings apply failed for server %s, release %s/%s: %v", server.ID, server.Namespace, server.Release, err)
		writeError(w, http.StatusBadGateway, fmt.Sprintf("Settings apply failed for server %s, release %s/%s. Cluster outcome may be uncertain; inspect the existing release before retrying. C2 settings were not saved.", server.ID, server.Namespace, server.Release))
		return
	}
	server.Status = StatusStarting
	if err := a.store.Update(func(state *State) error {
		state.Servers[id] = server
		appendEvent(state, server, "system", "info", "Settings apply requested", "Helm accepted the settings; rollout readiness is not verified")
		return nil
	}); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("Could not persist settings for server %s, release %s/%s. The cluster may already contain the submitted values. Inspect the existing release and reconcile C2 state before retrying.", server.ID, server.Namespace, server.Release))
		return
	}
	a.markTelemetryPending(server)
	writeJSON(w, http.StatusOK, server)
}
