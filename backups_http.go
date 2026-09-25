package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"
)

type backupRunRequest struct {
	ServerID       string `json:"serverId"`
	DefinitionID   string `json:"definitionId"`
	BackendID      string `json:"backendId"`
	ScheduleID     string `json:"scheduleId,omitempty"`
	Actor          string `json:"actor,omitempty"`
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
	Acknowledge    bool   `json:"acknowledge"`
	Scheduled      bool   `json:"scheduled,omitempty"`
}

type backupScheduleRequest struct {
	ServerID          string     `json:"serverId"`
	DefinitionID      string     `json:"definitionId"`
	BackendID         string     `json:"backendId"`
	Enabled           *bool      `json:"enabled"`
	Mode              rebootMode `json:"mode"`
	Cron              string     `json:"cron,omitempty"`
	IntervalValue     int        `json:"intervalValue,omitempty"`
	IntervalUnit      string     `json:"intervalUnit,omitempty"`
	DailyTimes        []string   `json:"dailyTimes,omitempty"`
	ExecutionTimezone string     `json:"executionTimezone"`
	RetainCount       int        `json:"retainCount,omitempty"`
	RetainDays        int        `json:"retainDays,omitempty"`
	Acknowledge       bool       `json:"acknowledge"`
}

type backupRunView struct {
	BackupRun
	ServerName     string               `json:"serverName"`
	ProfileName    string               `json:"profileName"`
	SourceLabel    string               `json:"sourceLabel,omitempty"`
	ItemCount      int                  `json:"itemCount"`
	BundleSize     int64                `json:"bundleSize"`
	BundleKey      string               `json:"bundleKey,omitempty"`
	CreatedAtLabel string               `json:"createdAtLabel,omitempty"`
	Items          []BackupManifestItem `json:"items,omitempty"`
}

type backupScheduleView struct {
	BackupSchedule
	ServerName  string `json:"serverName"`
	ProfileName string `json:"profileName"`
	Timing      string `json:"timing"`
}

func decodeBackupJSON(w http.ResponseWriter, r *http.Request, target any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid backup JSON: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("invalid backup JSON: request must contain one object")
	}
	return nil
}

func (a *App) handleBackups(w http.ResponseWriter, r *http.Request) {
	if requestPrincipal(r).Role != RoleAdmin {
		writeError(w, http.StatusForbidden, "permission denied")
		return
	}
	trimmed := strings.TrimPrefix(r.URL.Path, "/api/backups")
	if trimmed == "" || trimmed == "/" {
		if r.Method == http.MethodGet {
			a.listBackups(w, r)
			return
		}
		if r.Method == http.MethodPost {
			a.runBackupHTTP(w, r, "")
			return
		}
	}
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) == 1 && parts[0] == "profiles" {
		switch r.Method {
		case http.MethodGet:
			a.listBackups(w, r)
		case http.MethodPost:
			a.saveBackupProfile(w, r, "")
		default:
			methodNotAllowed(w, "GET, POST")
		}
		return
	}
	if len(parts) == 2 && parts[0] == "profiles" && parts[1] == "import" && r.Method == http.MethodPost {
		a.importBackupProfile(w, r)
		return
	}
	if len(parts) == 3 && parts[0] == "profiles" && parts[2] == "export" && r.Method == http.MethodGet {
		a.exportBackupProfile(w, r, parts[1])
		return
	}
	if len(parts) == 2 && parts[0] == "profiles" {
		switch r.Method {
		case http.MethodPut:
			a.saveBackupProfile(w, r, parts[1])
		case http.MethodDelete:
			a.deleteBackupProfile(w, r, parts[1])
		default:
			methodNotAllowed(w, "PUT, DELETE")
		}
		return
	}
	if len(parts) == 1 && parts[0] == "runs" {
		if r.Method == http.MethodPost {
			a.runBackupHTTP(w, r, "")
		} else if r.Method == http.MethodGet {
			a.listBackups(w, r)
		} else {
			methodNotAllowed(w, "GET, POST")
		}
		return
	}
	if len(parts) == 2 && parts[0] == "runs" {
		switch r.Method {
		case http.MethodGet:
			a.getBackupRun(w, parts[1])
		case http.MethodDelete:
			a.deleteBackupRun(w, r, parts[1])
		default:
			methodNotAllowed(w, "GET, DELETE")
		}
		return
	}
	if len(parts) == 3 && parts[0] == "runs" && parts[2] == "bundle" && r.Method == http.MethodGet {
		a.downloadBackupBundle(w, r, parts[1])
		return
	}
	if len(parts) == 1 && parts[0] == "schedules" {
		switch r.Method {
		case http.MethodGet:
			a.listBackups(w, r)
		case http.MethodPost:
			a.saveBackupSchedule(w, r, "")
		default:
			methodNotAllowed(w, "GET, POST")
		}
		return
	}
	if len(parts) == 3 && parts[0] == "schedules" && parts[2] == "run" && r.Method == http.MethodPost {
		a.runScheduledBackupNow(w, r, parts[1])
		return
	}
	if len(parts) == 2 && parts[0] == "schedules" {
		switch r.Method {
		case http.MethodPut:
			a.saveBackupSchedule(w, r, parts[1])
		case http.MethodDelete:
			a.deleteBackupSchedule(w, r, parts[1])
		default:
			methodNotAllowed(w, "PUT, DELETE")
		}
		return
	}
	if len(parts) == 2 && parts[1] == "create-server" && r.Method == http.MethodPost {
		a.createServerFromBackup(w, r, parts[0])
		return
	}
	if len(parts) == 1 && parts[0] == "backends" {
		switch r.Method {
		case http.MethodPost:
			a.addStorageBackendHTTP(w, r)
		default:
			methodNotAllowed(w, "POST")
		}
		return
	}
	if len(parts) == 2 && parts[0] == "backends" {
		switch r.Method {
		case http.MethodDelete:
			a.removeStorageBackendHTTP(w, r, parts[1])
		default:
			methodNotAllowed(w, "DELETE")
		}
		return
	}
	writeError(w, http.StatusNotFound, "backup route not found")
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeError(w, http.StatusMethodNotAllowed, "method not allowed")
}

func (a *App) listBackups(w http.ResponseWriter, r *http.Request) {
	snapshot := a.store.Snapshot()
	snapshot.initBackups()
	usage, storageErr := a.backupStorageUsage(r.Context())
	status := StorageBackendDisconnected
	var usedBytes *int64
	storageError := ""
	if storageErr == nil {
		status = StorageBackendConnected
		usedBytes = &usage
	} else {
		storageError = storageErr.Error()
	}
	backend := NewLocalStorageBackend(status)
	storage := []StorageBackend{backend}
	backendIDs := make([]string, 0, len(snapshot.StorageBackends))
	for id := range snapshot.StorageBackends {
		if id == backupLocalBackendID {
			continue
		}
		backendIDs = append(backendIDs, id)
	}
	sort.Strings(backendIDs)
	for _, id := range backendIDs {
		record := snapshot.StorageBackends[id]
		if _, connected := a.backups.repositories[id]; !connected {
			record.Status = StorageBackendDisconnected
		}
		storage = append(storage, record)
	}
	profiles := make([]BackupDefinition, 0, len(snapshot.BackupDefinitions))
	for _, definition := range snapshot.BackupDefinitions {
		profiles = append(profiles, definition)
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].Name < profiles[j].Name })
	schedules := make([]backupScheduleView, 0, len(snapshot.BackupSchedules))
	for _, schedule := range snapshot.BackupSchedules {
		schedules = append(schedules, a.backupScheduleView(schedule, snapshot))
	}
	sort.Slice(schedules, func(i, j int) bool { return schedules[i].ServerName < schedules[j].ServerName })
	runs := a.backupRunViews(snapshot)
	writeJSON(w, http.StatusOK, map[string]any{
		"available":    storageErr == nil,
		"storageError": storageError,
		"demo":         a.demo,
		"storage":      storage,
		"profiles":     profiles,
		"schedules":    schedules,
		"runs":         runs,
		"limits":       map[string]any{"repositoryBytes": a.backupLimit(), "usedBytes": usedBytes, "maxItemBytes": a.backupItemLimit(), "maxBundleBytes": a.backupBundleLimit()},
	})
}

func (a *App) storageBackendView(id string) StorageBackend {
	backend := a.store.Snapshot().StorageBackends[id]
	if _, connected := a.backups.repositories[id]; !connected {
		backend.Status = StorageBackendDisconnected
	}
	return backend
}

func (a *App) backupLimit() int64 {
	if a.backups == nil {
		return 0
	}
	return a.backups.maxBytes
}

func (a *App) backupItemLimit() int64 {
	if a.backups == nil {
		return 0
	}
	return a.backups.maxItem
}

func (a *App) backupRunViews(snapshot State) []backupRunView {
	definitions := snapshot.BackupDefinitions
	views := make([]backupRunView, 0, len(snapshot.BackupRuns))
	for _, run := range snapshot.BackupRuns {
		server := snapshot.Servers[run.ServerID]
		definition := definitions[run.DefinitionID]
		view := backupRunView{BackupRun: run, ServerName: worldLabel(server), ProfileName: definition.Name, SourceLabel: backupSourceLabel(run.Source), ItemCount: run.ItemCount, BundleSize: run.Bundle.Size, BundleKey: run.Bundle.Key}
		view.Items = snapshot.BackupManifests[run.ManifestID].Items
		if view.ItemCount == 0 && run.ManifestID != "" {
			view.ItemCount = len(snapshot.BackupManifests[run.ManifestID].Items)
		}
		views = append(views, view)
	}
	sort.SliceStable(views, func(i, j int) bool {
		if views[i].CreatedAt.Equal(views[j].CreatedAt) {
			return views[i].ID > views[j].ID
		}
		return views[i].CreatedAt.After(views[j].CreatedAt)
	})
	return views
}

func backupSourceLabel(source BackupSource) string {
	switch source {
	case BackupSourceRunningBAK:
		return "Running .sav.backup"
	case BackupSourceStoppedSAV:
		return "Stopped .sav"
	default:
		return "Unavailable"
	}
}

func (a *App) backupScheduleView(schedule BackupSchedule, snapshot State) backupScheduleView {
	server := snapshot.Servers[schedule.ServerID]
	definition := snapshot.BackupDefinitions[schedule.DefinitionID]
	return backupScheduleView{BackupSchedule: schedule, ServerName: worldLabel(server), ProfileName: definition.Name, Timing: backupScheduleTiming(schedule)}
}

func backupScheduleTiming(schedule BackupSchedule) string {
	switch schedule.Mode {
	case rebootModeDaily:
		return "Daily at " + strings.Join(schedule.DailyTimes, ", ")
	case rebootModeInterval:
		return fmt.Sprintf("Every %d %s", schedule.IntervalValue, schedule.IntervalUnit)
	default:
		if schedule.Cron != "" {
			return "Custom cron · " + schedule.Cron
		}
		return schedule.Expression
	}
}

func (a *App) runBackupHTTP(w http.ResponseWriter, r *http.Request, scheduleID string) {
	if !a.lifecycleMu.TryLock() {
		writeError(w, http.StatusConflict, "another server lifecycle operation is in progress; try again shortly")
		return
	}
	defer a.lifecycleMu.Unlock()
	var request backupRunRequest
	if err := decodeBackupJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if scheduleID != "" {
		request.ScheduleID = scheduleID
	}
	request.Actor = requestPrincipal(r).Subject
	request.Scheduled = false
	run, err := a.executeBackup(r.Context(), request)
	if err != nil {
		a.writeBackupOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, run)
}

func (a *App) executeBackup(ctx context.Context, request backupRunRequest) (BackupRun, error) {
	if !a.backupAvailable() {
		return BackupRun{}, errors.New("backups are unavailable because persistent C2 storage is disabled or disconnected")
	}
	snapshot := a.store.Snapshot()
	server, ok := snapshot.Servers[request.ServerID]
	if !ok {
		return BackupRun{}, errors.New("server not found")
	}
	if snapshot.deleting(server.ID) {
		return BackupRun{}, errors.New("server is being deleted")
	}
	definition, ok := snapshot.BackupDefinitions[request.DefinitionID]
	if !ok {
		return BackupRun{}, errors.New("backup profile not found")
	}
	if err := definition.Validate(); err != nil {
		return BackupRun{}, fmt.Errorf("backup profile is invalid: %w", err)
	}
	if request.BackendID == "" {
		request.BackendID = backupLocalBackendID
	}
	if _, err := a.repositoryFor(request.BackendID); err != nil {
		return BackupRun{}, errors.New("selected storage backend is unavailable")
	}
	if snapshot.StorageBackends[request.BackendID].Status != StorageBackendConnected {
		return BackupRun{}, errors.New("selected storage backend is unavailable")
	}
	_, source, err := backupSourceForStatus(server.Status)
	if err != nil {
		return BackupRun{}, err
	}
	if !request.Acknowledge {
		return BackupRun{}, errors.New("confirm that C2 may read the server world storage")
	}
	if request.Actor == "" {
		request.Actor = "admin"
	}
	if request.IdempotencyKey == "" {
		request.IdempotencyKey = newBackupID("request")
	}
	idempotency, err := NewIdempotencyRequest(request.Actor, "backup.run", request.IdempotencyKey, struct {
		ServerID, DefinitionID, BackendID, ScheduleID string
		Scheduled                                     bool
	}{request.ServerID, request.DefinitionID, request.BackendID, request.ScheduleID, request.Scheduled})
	if err != nil {
		return BackupRun{}, err
	}
	for _, existing := range snapshot.BackupRuns {
		if existing.Idempotency.Identity() != idempotency.Identity() {
			continue
		}
		if err := CheckIdempotency(existing.Idempotency, idempotency); err != nil {
			return BackupRun{}, err
		}
		if existing.Status == BackupRunStatusSucceeded || existing.Status == BackupRunStatusRunning {
			return existing, nil
		}
	}
	now := a.backupNow()
	run := BackupRun{ID: newBackupID("bkp"), ServerID: server.ID, DefinitionID: definition.ID, ScheduleID: request.ScheduleID, BackendID: request.BackendID, Source: source, Phase: BackupRunQueued, Status: BackupRunStatusRunning, Idempotency: idempotency, CreatedAt: now, UpdatedAt: now}
	if err := a.store.Update(func(state *State) error {
		state.initBackups()
		state.BackupRuns = append(state.BackupRuns, run)
		emitAlert(state, server, BackupStarted, run.ID, definition.Name+" · "+backupSourceLabel(source), now)
		return nil
	}); err != nil {
		return BackupRun{}, err
	}
	setFailure := func(cause error) (BackupRun, error) {
		run.Phase, run.Status, run.Error, run.UpdatedAt = BackupRunCapturing, BackupRunStatusFailed, cause.Error(), a.backupNow()
		_ = a.store.Update(func(state *State) error {
			updateBackupRun(state, run)
			emitAlert(state, server, BackupFailed, run.ID, definition.Name+" · "+backupSourceLabel(source)+": "+cause.Error(), run.UpdatedAt)
			if request.Scheduled && request.ScheduleID != "" {
				if schedule, exists := state.BackupSchedules[request.ScheduleID]; exists {
					schedule.LastRun, schedule.LastResult, schedule.LastError = cloneTimePtr(&run.UpdatedAt), BackupRunStatusFailed, cause.Error()
					next, scheduleErr := nextBackupScheduleRun(schedule, run.UpdatedAt)
					if scheduleErr != nil {
						schedule.Enabled, schedule.NextRun = false, nil
						schedule.LastError = "Schedule could not be recalculated after a failed run: " + scheduleErr.Error()
					} else {
						schedule.NextRun = cloneTimePtr(&next)
					}
					schedule.Revision++
					state.BackupSchedules[request.ScheduleID] = schedule
				}
			}
			return nil
		})
		return run, cause
	}
	if err := a.store.Update(func(state *State) error {
		run.Phase = BackupRunCapturing
		run.UpdatedAt = a.backupNow()
		updateBackupRun(state, run)
		return nil
	}); err != nil {
		return setFailure(err)
	}
	items, cleanup, err := a.collectBackup(ctx, server, definition, source)
	defer cleanup()
	if err != nil {
		return setFailure(err)
	}
	if len(items) == 0 {
		return setFailure(errors.New("backup profile resolved no items"))
	}
	run.Phase = BackupRunPublishing
	if err := a.store.Update(func(state *State) error { run.UpdatedAt = a.backupNow(); updateBackupRun(state, run); return nil }); err != nil {
		return setFailure(err)
	}
	repository, err := a.repositoryFor(run.BackendID)
	if err != nil {
		return setFailure(err)
	}
	publication, err := repository.Publish(ctx, PublishBackupRequest{Idempotency: idempotency, Manifest: BackupManifestDraft{ID: run.ID, DefinitionID: definition.ID, DefinitionRevision: definition.Revision, ServerID: server.ID, ServerType: definition.ServerType, Source: source, CreatedAt: now}, Items: items})
	if err != nil {
		return setFailure(err)
	}
	run.Phase, run.Status, run.ManifestID, run.Bundle, run.ItemCount, run.UpdatedAt = BackupRunPublishing, BackupRunStatusSucceeded, publication.Manifest.ID, publication.Bundle, len(publication.Manifest.Items), a.backupNow()
	if err := a.store.Update(func(state *State) error {
		state.initBackups()
		state.BackupManifests[publication.Manifest.ID] = publication.Manifest
		updateBackupRun(state, run)
		if request.Scheduled && request.ScheduleID != "" {
			if schedule, exists := state.BackupSchedules[request.ScheduleID]; exists {
				next, scheduleErr := nextBackupScheduleRun(schedule, run.UpdatedAt)
				if scheduleErr != nil {
					schedule.Enabled, schedule.NextRun, schedule.LastResult, schedule.LastError = false, nil, BackupRunStatusFailed, scheduleErr.Error()
				} else {
					schedule.NextRun, schedule.LastRun, schedule.LastResult, schedule.LastError = cloneTimePtr(&next), cloneTimePtr(&run.UpdatedAt), BackupRunStatusSucceeded, ""
				}
				schedule.Revision++
				state.BackupSchedules[request.ScheduleID] = schedule
			}
		}
		emitAlert(state, server, BackupCompleted, run.ID, definition.Name+" · "+backupSourceLabel(source), run.UpdatedAt)
		return nil
	}); err != nil {
		return setFailure(err)
	}
	if request.ScheduleID != "" {
		a.pruneBackupRuns(ctx, request.ScheduleID)
	}
	return run, nil
}

func updateBackupRun(state *State, run BackupRun) {
	for index := range state.BackupRuns {
		if state.BackupRuns[index].ID == run.ID {
			state.BackupRuns[index] = run
			return
		}
	}
	state.BackupRuns = append(state.BackupRuns, run)
}

func (a *App) pruneBackupRuns(ctx context.Context, scheduleID string) {
	snapshot := a.store.Snapshot()
	schedule, ok := snapshot.BackupSchedules[scheduleID]
	if !ok || (schedule.RetainCount <= 0 && schedule.RetainDays <= 0) {
		return
	}
	candidates := make([]BackupRun, 0, len(snapshot.BackupRuns))
	for _, run := range snapshot.BackupRuns {
		if run.ScheduleID == scheduleID && run.Status == BackupRunStatusSucceeded {
			candidates = append(candidates, run)
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].CreatedAt.After(candidates[j].CreatedAt) })
	keep := map[string]bool{}
	if schedule.RetainCount > 0 {
		for i := 0; i < len(candidates) && i < schedule.RetainCount; i++ {
			keep[candidates[i].ID] = true
		}
	}
	if schedule.RetainDays > 0 {
		cutoff := a.backupNow().AddDate(0, 0, -schedule.RetainDays)
		for _, run := range candidates {
			if run.CreatedAt.After(cutoff) {
				keep[run.ID] = true
			}
		}
	}
	repository, err := a.repositoryFor(schedule.BackendID)
	if err != nil {
		return
	}
	var pruned []BackupRun
	for _, run := range candidates {
		if keep[run.ID] || run.ManifestID == "" {
			continue
		}
		if err := repository.Delete(ctx, run.ManifestID); err != nil {
			continue
		}
		pruned = append(pruned, run)
	}
	if len(pruned) == 0 {
		return
	}
	_ = a.store.Update(func(state *State) error {
		remove := make(map[string]bool, len(pruned))
		for _, run := range pruned {
			remove[run.ID] = true
			delete(state.BackupManifests, run.ManifestID)
		}
		next := make([]BackupRun, 0, len(state.BackupRuns))
		for _, run := range state.BackupRuns {
			if !remove[run.ID] {
				next = append(next, run)
			}
		}
		state.BackupRuns = next
		return nil
	})
}

func nextBackupScheduleRun(schedule BackupSchedule, after time.Time) (time.Time, error) {
	definition := rebootDefinition{Mode: schedule.Mode, Cron: schedule.Cron, IntervalValue: schedule.IntervalValue, IntervalUnit: schedule.IntervalUnit, DailyTimes: append([]string(nil), schedule.DailyTimes...), ExecutionTimezone: schedule.Timezone}
	if definition.Mode == "" {
		definition.Mode = rebootModeCron
		definition.Cron = schedule.Expression
	}
	return nextReboot(definition, derefTime(schedule.IntervalAnchor), after)
}

func derefTime(value *time.Time) time.Time {
	if value == nil {
		return time.Time{}
	}
	return *value
}

func (a *App) writeBackupOperationError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, ErrBackupQuotaExceeded) {
		status = http.StatusInsufficientStorage
	} else if strings.Contains(err.Error(), "unavailable") || strings.Contains(err.Error(), "disabled") {
		status = http.StatusServiceUnavailable
	} else if strings.Contains(err.Error(), "not found") {
		status = http.StatusNotFound
	} else if strings.Contains(err.Error(), "transition") || strings.Contains(err.Error(), "stopped") {
		status = http.StatusConflict
	}
	writeError(w, status, err.Error())
}

func (a *App) saveBackupProfile(w http.ResponseWriter, r *http.Request, id string) {
	var definition BackupDefinition
	if err := decodeBackupJSON(w, r, &definition); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if id != "" {
		definition.ID = id
	}
	if definition.APIVersion == "" {
		definition.APIVersion = BackupDefinitionAPIVersion
	}
	if definition.Kind == "" {
		definition.Kind = BackupDefinitionKind
	}
	if definition.Revision == 0 {
		definition.Revision = 1
	}
	if err := definition.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if definition.ID == "dragonwilds-world-save" {
		writeError(w, http.StatusConflict, "the built-in Dragonwilds profile cannot be replaced")
		return
	}
	err := a.store.Update(func(state *State) error {
		state.initBackups()
		if id == "" && state.BackupDefinitions[definition.ID].ID != "" {
			return errors.New("backup profile already exists; edit it or choose a different ID")
		}
		if id != "" {
			if _, ok := state.BackupDefinitions[id]; !ok {
				return errors.New("backup profile not found")
			}
			if definition.Revision != state.BackupDefinitions[id].Revision {
				return errors.New("backup profile revision changed; reload before editing")
			}
			definition.Revision = state.BackupDefinitions[id].Revision + 1
		}
		state.BackupDefinitions[definition.ID] = definition
		return nil
	})
	if err != nil {
		status := http.StatusConflict
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, definition)
}

func (a *App) deleteBackupProfile(w http.ResponseWriter, _ *http.Request, id string) {
	if id == "dragonwilds-world-save" {
		writeError(w, http.StatusConflict, "the built-in Dragonwilds profile cannot be removed")
		return
	}
	err := a.store.Update(func(state *State) error {
		if _, ok := state.BackupDefinitions[id]; !ok {
			return errors.New("backup profile not found")
		}
		for _, schedule := range state.BackupSchedules {
			if schedule.DefinitionID == id {
				return errors.New("backup profile is used by a schedule; change or remove the schedule first")
			}
		}
		delete(state.BackupDefinitions, id)
		return nil
	})
	if err != nil {
		status := http.StatusConflict
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

type addStorageBackendRequest struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Endpoint   string          `json:"endpoint"`
	Bucket     string          `json:"bucket"`
	Region     string          `json:"region,omitempty"`
	PathPrefix string          `json:"pathPrefix,omitempty"`
	UseSSL     bool            `json:"useSSL"`
	SecretRef  SecretReference `json:"secretRef"`
}

func (a *App) addStorageBackendHTTP(w http.ResponseWriter, r *http.Request) {
	var request addStorageBackendRequest
	if err := decodeBackupJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	backend := StorageBackend{
		ID:         request.ID,
		Name:       request.Name,
		Endpoint:   request.Endpoint,
		Bucket:     request.Bucket,
		Region:     request.Region,
		PathPrefix: request.PathPrefix,
		UseSSL:     request.UseSSL,
		SecretRef:  request.SecretRef,
	}
	if err := a.addS3StorageBackend(r.Context(), backend); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, a.storageBackendView(backend.ID))
}

func (a *App) removeStorageBackendHTTP(w http.ResponseWriter, _ *http.Request, id string) {
	if err := a.removeStorageBackend(id); err != nil {
		status := http.StatusConflict
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

func (a *App) importBackupProfile(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 128<<10))
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read profile YAML")
		return
	}
	if strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		var request struct {
			YAML string `json:"yaml"`
		}
		if json.Unmarshal(data, &request) != nil {
			writeError(w, http.StatusBadRequest, "invalid profile import request")
			return
		}
		data = []byte(request.YAML)
	}
	definition, err := ImportBackupDefinitionYAML(data)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if definition.ID == "dragonwilds-world-save" {
		writeError(w, http.StatusConflict, "the built-in Dragonwilds profile cannot be replaced")
		return
	}
	if err := a.store.Update(func(state *State) error {
		state.initBackups()
		if state.BackupDefinitions[definition.ID].ID != "" {
			return errors.New("backup profile already exists; edit it or import with a different ID")
		}
		state.BackupDefinitions[definition.ID] = definition
		return nil
	}); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, definition)
}

func (a *App) exportBackupProfile(w http.ResponseWriter, _ *http.Request, id string) {
	definition, err := a.backupDefinition(id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	data, err := ExportBackupDefinitionYAML(definition)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", definition.ID+".yaml"))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (a *App) saveBackupSchedule(w http.ResponseWriter, r *http.Request, id string) {
	var request backupScheduleRequest
	if err := decodeBackupJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if request.Enabled == nil {
		value := true
		request.Enabled = &value
	}
	if request.BackendID == "" {
		request.BackendID = backupLocalBackendID
	}
	if request.ServerID == "" || request.DefinitionID == "" {
		writeError(w, http.StatusBadRequest, "serverId and definitionId are required")
		return
	}
	if request.RetainCount < 0 || request.RetainDays < 0 {
		writeError(w, http.StatusBadRequest, "retainCount and retainDays must not be negative")
		return
	}
	if !*request.Enabled || request.Acknowledge {
		// A disabled schedule does not read storage now. An enabled schedule must be explicit.
	} else {
		writeError(w, http.StatusBadRequest, "confirm that scheduled backup collection may read server storage")
		return
	}
	definition, enabled, err := normalizeBackupSchedule(request)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if enabled && !a.backupAvailable() {
		writeError(w, http.StatusServiceUnavailable, "backups are unavailable because persistent C2 storage is disabled or disconnected")
		return
	}
	var saved BackupSchedule
	err = a.store.Update(func(state *State) error {
		state.initBackups()
		if _, ok := state.Servers[request.ServerID]; !ok {
			return errors.New("server not found")
		}
		if _, ok := state.BackupDefinitions[request.DefinitionID]; !ok {
			return errors.New("backup profile not found")
		}
		if id == "" {
			id = newBackupID("sch")
		}
		old, exists := state.BackupSchedules[id]
		if id != "" && !exists && r.Method == http.MethodPut {
			return errors.New("backup schedule not found")
		}
		now := a.backupNow()
		saved = old
		saved.ID, saved.ServerID, saved.DefinitionID, saved.BackendID = id, request.ServerID, request.DefinitionID, request.BackendID
		saved.Enabled, saved.Timezone, saved.Mode, saved.Cron, saved.IntervalValue, saved.IntervalUnit, saved.DailyTimes = enabled, definition.ExecutionTimezone, definition.Mode, definition.Cron, definition.IntervalValue, definition.IntervalUnit, append([]string(nil), definition.DailyTimes...)
		saved.RetainCount, saved.RetainDays = request.RetainCount, request.RetainDays
		saved.Expression = scheduleExpression(definition)
		saved.Revision = old.Revision + 1
		if saved.Revision == 1 {
			saved.Revision = 1
		}
		if definition.Mode == rebootModeInterval && (old.IntervalAnchor == nil || old.Mode != definition.Mode || old.IntervalValue != definition.IntervalValue || old.IntervalUnit != definition.IntervalUnit) {
			saved.IntervalAnchor = cloneTimePtr(&now)
		} else if definition.Mode != rebootModeInterval {
			saved.IntervalAnchor = nil
		}
		if saved.Enabled {
			next, err := nextBackupScheduleRun(saved, now)
			if err != nil {
				return err
			}
			saved.NextRun = cloneTimePtr(&next)
		} else {
			saved.NextRun = nil
		}
		state.BackupSchedules[id] = saved
		return nil
	})
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	snapshot := a.store.Snapshot()
	writeJSON(w, http.StatusOK, a.backupScheduleView(saved, snapshot))
}

func normalizeBackupSchedule(request backupScheduleRequest) (rebootDefinition, bool, error) {
	enabled := request.Enabled != nil && *request.Enabled
	mode := request.Mode
	if mode == "" {
		mode = rebootModeDaily
	}
	definition := rebootDefinition{ServerID: request.ServerID, Mode: mode, Cron: strings.TrimSpace(request.Cron), IntervalValue: request.IntervalValue, IntervalUnit: strings.TrimSpace(request.IntervalUnit), DailyTimes: append([]string(nil), request.DailyTimes...), ExecutionTimezone: strings.TrimSpace(request.ExecutionTimezone)}
	if definition.ExecutionTimezone == "" {
		definition.ExecutionTimezone = "UTC"
	}
	if _, err := parseRebootTiming(definition); err != nil {
		return rebootDefinition{}, false, err
	}
	return definition, enabled, nil
}

func scheduleExpression(definition rebootDefinition) string {
	switch definition.Mode {
	case rebootModeDaily:
		return strings.Join(definition.DailyTimes, ",")
	case rebootModeInterval:
		return fmt.Sprintf("%d %s", definition.IntervalValue, definition.IntervalUnit)
	default:
		return definition.Cron
	}
}

func (a *App) deleteBackupSchedule(w http.ResponseWriter, _ *http.Request, id string) {
	err := a.store.Update(func(state *State) error {
		if _, ok := state.BackupSchedules[id]; !ok {
			return errors.New("backup schedule not found")
		}
		delete(state.BackupSchedules, id)
		return nil
	})
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

func (a *App) runScheduledBackupNow(w http.ResponseWriter, r *http.Request, id string) {
	if !a.lifecycleMu.TryLock() {
		writeError(w, http.StatusConflict, "another server lifecycle operation is in progress")
		return
	}
	defer a.lifecycleMu.Unlock()
	var confirmation struct {
		Acknowledge bool `json:"acknowledge"`
	}
	if err := decodeBackupJSON(w, r, &confirmation); err != nil || !confirmation.Acknowledge {
		writeError(w, http.StatusBadRequest, "confirm that C2 may read the server world storage")
		return
	}
	schedule, ok := a.store.Snapshot().BackupSchedules[id]
	if !ok {
		writeError(w, http.StatusNotFound, "backup schedule not found")
		return
	}
	request := backupRunRequest{ServerID: schedule.ServerID, DefinitionID: schedule.DefinitionID, BackendID: schedule.BackendID, ScheduleID: id, Actor: requestPrincipal(r).Subject, IdempotencyKey: newBackupID("manual"), Acknowledge: true}
	run, err := a.executeBackup(r.Context(), request)
	if err != nil {
		a.writeBackupOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, run)
}

func (a *App) getBackupRun(w http.ResponseWriter, id string) {
	snapshot := a.store.Snapshot()
	for _, view := range a.backupRunViews(snapshot) {
		if view.ID == id {
			writeJSON(w, http.StatusOK, view)
			return
		}
	}
	writeError(w, http.StatusNotFound, "backup run not found")
}

func (a *App) deleteBackupRun(w http.ResponseWriter, r *http.Request, id string) {
	snapshot := a.store.Snapshot()
	var run BackupRun
	found := false
	for _, existing := range snapshot.BackupRuns {
		if existing.ID == id {
			run, found = existing, true
			break
		}
	}
	if !found {
		writeError(w, http.StatusNotFound, "backup run not found")
		return
	}
	if run.Status == BackupRunStatusRunning {
		writeError(w, http.StatusConflict, "backup run is still in progress")
		return
	}
	if run.ManifestID != "" {
		repository, err := a.repositoryFor(run.BackendID)
		if err != nil {
			writeError(w, http.StatusBadGateway, "selected storage backend is unavailable")
			return
		}
		if err := repository.Delete(r.Context(), run.ManifestID); err != nil {
			writeError(w, http.StatusBadGateway, "delete backup object: "+err.Error())
			return
		}
	}
	err := a.store.Update(func(state *State) error {
		next := make([]BackupRun, 0, len(state.BackupRuns))
		removed := false
		for _, existing := range state.BackupRuns {
			if existing.ID == id {
				removed = true
				continue
			}
			next = append(next, existing)
		}
		if !removed {
			return errors.New("backup run not found")
		}
		state.BackupRuns = next
		if run.ManifestID != "" {
			delete(state.BackupManifests, run.ManifestID)
		}
		return nil
	})
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

func (a *App) downloadBackupBundle(w http.ResponseWriter, r *http.Request, id string) {
	if !a.backupAvailable() {
		writeError(w, http.StatusServiceUnavailable, "backups are unavailable")
		return
	}
	snapshot := a.store.Snapshot()
	var run *BackupRun
	for index := range snapshot.BackupRuns {
		if snapshot.BackupRuns[index].ID == id {
			run = &snapshot.BackupRuns[index]
			break
		}
	}
	if run == nil || run.Status != BackupRunStatusSucceeded || run.ManifestID == "" {
		writeError(w, http.StatusNotFound, "completed backup bundle not found")
		return
	}
	backendID := run.BackendID
	if backendID == "" {
		backendID = backupLocalBackendID
	}
	repository, err := a.repositoryFor(backendID)
	if err != nil {
		writeError(w, http.StatusNotFound, "backup bundle is unavailable")
		return
	}
	reader, err := repository.OpenBundle(r.Context(), run.Bundle.Key)
	if err != nil {
		writeError(w, http.StatusNotFound, "backup bundle is unavailable")
		return
	}
	defer reader.Close()
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"backup-%s.zip\"", id))
	if run.Bundle.Size > 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", run.Bundle.Size))
	}
	_, _ = io.Copy(w, reader)
}

func (a *App) createServerFromBackup(w http.ResponseWriter, r *http.Request, id string) {
	if !a.lifecycleMu.TryLock() {
		writeError(w, http.StatusConflict, "another server lifecycle operation is in progress")
		return
	}
	defer a.lifecycleMu.Unlock()
	if !a.backupAvailable() {
		writeError(w, http.StatusServiceUnavailable, "backup storage is unavailable")
		return
	}
	var request struct {
		ServerName    string `json:"serverName"`
		OwnerName     string `json:"ownerName"`
		OwnerID       string `json:"ownerId"`
		ConfirmCreate bool   `json:"confirmCreate"`
	}
	if err := decodeBackupJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !request.ConfirmCreate {
		writeError(w, http.StatusBadRequest, "confirm that a new server and world will be created")
		return
	}
	if strings.TrimSpace(request.ServerName) == "" || len(request.ServerName) > 48 || strings.ContainsAny(request.ServerName, "\x00\r\n") {
		writeError(w, http.StatusBadRequest, "serverName must contain 1 to 48 characters")
		return
	}
	if strings.TrimSpace(request.OwnerName) == "" || len(request.OwnerName) > 48 || strings.ContainsAny(request.OwnerName, "\x00\r\n") {
		writeError(w, http.StatusBadRequest, "ownerName must contain 1 to 48 characters")
		return
	}
	ownerID, err := normalizePlayerID(request.OwnerID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "provide a valid owner EOS ID")
		return
	}
	snapshot := a.store.Snapshot()
	var run *BackupRun
	for index := range snapshot.BackupRuns {
		if snapshot.BackupRuns[index].ID == id && snapshot.BackupRuns[index].Status == BackupRunStatusSucceeded {
			run = &snapshot.BackupRuns[index]
			break
		}
	}
	if run == nil {
		writeError(w, http.StatusNotFound, "completed backup not found")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	files, err := a.backupRestoreFiles(ctx, run.ManifestID)
	defer cleanupRestoreFiles(files)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	source, ok := snapshot.Servers[run.ServerID]
	if !ok {
		writeError(w, http.StatusConflict, "source server settings are unavailable; restore into an existing stopped server")
		return
	}
	newID, err := a.newServerID(ctx, CreateServerRequest{Name: request.OwnerName, Namespace: source.Namespace, ServerSettings: ServerSettings{WorldName: request.ServerName}})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	token, err := randomToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not allocate server ownership")
		return
	}
	created := Server{ID: newID, Release: newID, Name: strings.TrimSpace(request.OwnerName), Namespace: source.Namespace, OwnerID: ownerID, OwnershipToken: token, CurrentImage: source.CurrentImage, DesiredImage: source.CurrentImage, MaxPlayers: source.MaxPlayers, MemoryLimitMiB: source.MemoryLimitMiB, CPULimitMillis: source.CPULimitMillis, Status: StatusStopped, ServerSettings: source.ServerSettings}
	created.WorldName = strings.TrimSpace(request.ServerName)
	created.AdminIDs = ""
	if err := validateCreate(CreateServerRequest{Name: created.Name, Namespace: created.Namespace, OwnerID: created.OwnerID, MaxPlayers: created.MaxPlayers, MemoryLimitMiB: created.MemoryLimitMiB, CPULimitMillis: created.CPULimitMillis, ServerSettings: created.ServerSettings}); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !validRestoreSaveName(created.WorldName + ".sav") {
		writeError(w, http.StatusBadRequest, "target world name cannot form a safe save filename")
		return
	}
	if k, ok := a.orchestrator.(*kubeOrchestrator); ok && !a.demo {
		err = k.DeployStopped(ctx, created)
	} else {
		err = a.orchestrator.Deploy(ctx, created)
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not provision stopped restore target: "+err.Error())
		return
	}
	if err := a.store.Update(func(state *State) error {
		state.Servers[created.ID] = created
		appendEvent(state, created, "system", "info", "Restore target created", "New server and world PVC remain stopped until restore completes")
		return nil
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "could not persist restored server")
		return
	}
	if err := a.restoreFiles(ctx, created, files, false); err != nil {
		_ = a.store.Update(func(state *State) error {
			appendEvent(state, created, "system", "error", "Restore failed", "Retry from Maintenance on "+created.ID+": "+err.Error())
			return nil
		})
		writeError(w, http.StatusBadGateway, "new server "+created.ID+" remains stopped; retry restore from Maintenance: "+err.Error())
		return
	}
	_ = a.store.Update(func(state *State) error {
		appendEvent(state, created, "system", "success", "Server restored from backup", run.ManifestID)
		return nil
	})
	writeJSON(w, http.StatusCreated, created)
}

type restoreRequest struct {
	Source             string `json:"source"`
	ManifestID         string `json:"manifestId,omitempty"`
	ConfirmEmpty       bool   `json:"confirmEmpty"`
	DestructiveConfirm bool   `json:"destructiveConfirm"`
}

func (a *App) handleRestore(w http.ResponseWriter, r *http.Request, serverID string) {
	if requestPrincipal(r).Role != RoleAdmin {
		writeError(w, http.StatusForbidden, "permission denied")
		return
	}
	if !a.lifecycleMu.TryLock() {
		writeError(w, http.StatusConflict, "another server lifecycle operation is in progress")
		return
	}
	defer a.lifecycleMu.Unlock()
	if !a.backupAvailable() {
		writeError(w, http.StatusServiceUnavailable, "backup storage is unavailable")
		return
	}
	snapshot := a.store.Snapshot()
	server, ok := snapshot.Servers[serverID]
	if !ok {
		writeError(w, http.StatusNotFound, "server not found")
		return
	}
	if server.Status != StatusStopped {
		writeError(w, http.StatusConflict, "stop the server and wait for C2 to verify it is stopped before restoring")
		return
	}
	if snapshot.deleting(serverID) {
		writeError(w, http.StatusConflict, "server is being deleted")
		return
	}
	request := restoreRequest{}
	var customName string
	var customData []byte
	contentType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if contentType == "multipart/form-data" {
		if err := readRestoreMultipart(w, r, &request, &customName, &customData); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	} else if err := decodeBackupJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !request.ConfirmEmpty {
		writeError(w, http.StatusBadRequest, "confirm that the target world is empty")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	var files []restoreFile
	if request.Source == "custom" {
		if !validRestoreSaveName(customName) || len(customData) == 0 {
			writeError(w, http.StatusBadRequest, "select a non-empty .sav file")
			return
		}
		files = []restoreFile{{Path: path.Join(backupWorldSaveRoot, customName), Data: customData}}
	} else if request.Source == "backup" {
		manifest, ok := snapshot.BackupManifests[request.ManifestID]
		if !ok || manifest.ID == "" {
			writeError(w, http.StatusNotFound, "completed backup not found")
			return
		}
		var err error
		files, err = a.backupRestoreFiles(ctx, request.ManifestID)
		defer cleanupRestoreFiles(files)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	} else {
		writeError(w, http.StatusBadRequest, "restore source must be backup or custom")
		return
	}
	if err := a.restoreFiles(ctx, server, files, request.DestructiveConfirm); err != nil {
		if errors.Is(err, errRestoreConfirmation) {
			writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "code": "restore_confirmation_required"})
			return
		}
		_ = a.store.Update(func(state *State) error {
			appendEvent(state, server, "system", "error", "Restore failed", err.Error())
			return nil
		})
		writeError(w, http.StatusBadGateway, "restore failed; target remains stopped and can be retried: "+err.Error())
		return
	}
	if err := a.store.Update(func(state *State) error {
		current := state.Servers[serverID]
		appendEvent(state, current, "system", "success", "World restored", request.Source+" "+customName)
		return nil
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "could not persist restore result")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"serverId": serverID, "source": request.Source, "status": "restored", "file": customName})
}

func readRestoreMultipart(w http.ResponseWriter, r *http.Request, request *restoreRequest, filename *string, data *[]byte) error {
	contentType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "multipart/form-data" || params["boundary"] == "" {
		return errors.New("invalid multipart restore upload")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxCreateBytes)
	reader := multipart.NewReader(r.Body, params["boundary"])
	seenRequest, seenSave := false, false
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return errors.New("invalid multipart restore upload")
		}
		_, disposition, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
		if err != nil {
			return errors.New("invalid upload part")
		}
		switch part.FormName() {
		case "request":
			if seenRequest || disposition["filename"] != "" {
				return errors.New("provide exactly one request part")
			}
			seenRequest = true
			metadata, err := io.ReadAll(io.LimitReader(part, (32<<10)+1))
			if err != nil || len(metadata) > 32<<10 {
				return errors.New("restore settings must be at most 32 KiB")
			}
			decoder := json.NewDecoder(bytes.NewReader(metadata))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(request); err != nil {
				return errors.New("invalid restore request")
			}
			if decoder.Decode(new(any)) != io.EOF {
				return errors.New("provide exactly one restore object")
			}
		case "save":
			if seenSave {
				return errors.New("provide exactly one .sav file")
			}
			seenSave = true
			*filename = disposition["filename"]
			if !validRestoreSaveName(*filename) {
				return errors.New("select a plain .sav filename without paths or control characters")
			}
			value, err := io.ReadAll(io.LimitReader(part, maxSaveBytes+1))
			if err != nil || len(value) > maxSaveBytes {
				return errors.New("save must be at most 32 MiB")
			}
			*data = value
		default:
			return errors.New("restore accepts only request and save parts")
		}
	}
	if !seenRequest {
		return errors.New("restore requires one request part")
	}
	if request.Source == "custom" && !seenSave {
		return errors.New("custom restore requires one save part")
	}
	if request.Source != "custom" && seenSave {
		return errors.New("tracked restore cannot include an uploaded save")
	}
	return nil
}

func validRestoreSaveName(name string) bool {
	return validSaveName(strings.TrimSuffix(name, path.Ext(name)) + strings.ToLower(path.Ext(name)))
}
