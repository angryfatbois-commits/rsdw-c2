package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

const (
	rebootScanInterval = 15 * time.Second
	rebootLateWindow   = 60 * time.Second
	maxRebootSchedules = 256
	maxRebootHistory   = 500
	maxRebootActive    = 1024
)

type rebootResult string

const (
	rebootAwaiting  rebootResult = "awaiting_reconciliation"
	rebootCompleted rebootResult = "completed"
	rebootFailed    rebootResult = "failed"
	rebootSkipped   rebootResult = "skipped"
	rebootMissed    rebootResult = "missed"
	rebootUncertain rebootResult = "uncertain"
)

type rebootSchedule struct {
	ID               string           `json:"id"`
	Definition       rebootDefinition `json:"definition"`
	Enabled          bool             `json:"enabled"`
	Revision         uint64           `json:"revision"`
	CreatedAt        time.Time        `json:"createdAt"`
	UpdatedAt        time.Time        `json:"updatedAt"`
	IntervalAnchor   *time.Time       `json:"intervalAnchor,omitempty"`
	NextRun          *time.Time       `json:"nextRun,omitempty"`
	LastOccurrenceAt *time.Time       `json:"lastOccurrenceAt,omitempty"`
	LastResult       rebootResult     `json:"lastResult,omitempty"`
	LastReason       string           `json:"lastReason,omitempty"`
	LastOperationID  string           `json:"lastOperationId,omitempty"`
}

type rebootExecution struct {
	ID            string       `json:"id,omitempty"`
	OccurrenceID  string       `json:"occurrenceId"`
	ScheduleID    string       `json:"scheduleId"`
	Revision      uint64       `json:"revision"`
	ServerID      string       `json:"serverId"`
	OccurrenceAt  time.Time    `json:"occurrenceAt"`
	RecordedAt    time.Time    `json:"recordedAt"`
	OperationID   string       `json:"operationId,omitempty"`
	Result        rebootResult `json:"result"`
	Reason        string       `json:"reason,omitempty"`
	MissedThrough *time.Time   `json:"missedThrough,omitempty"`
}

type rebootScheduleView struct {
	ID                string       `json:"id"`
	ServerID          string       `json:"serverId"`
	ServerName        string       `json:"serverName"`
	Mode              rebootMode   `json:"mode"`
	Cron              string       `json:"cron,omitempty"`
	IntervalValue     int          `json:"intervalValue,omitempty"`
	IntervalUnit      string       `json:"intervalUnit,omitempty"`
	DailyTimes        []string     `json:"dailyTimes,omitempty"`
	ExecutionTimezone string       `json:"executionTimezone"`
	Enabled           bool         `json:"enabled"`
	Revision          uint64       `json:"revision"`
	CreatedAt         time.Time    `json:"createdAt"`
	UpdatedAt         time.Time    `json:"updatedAt"`
	NextRun           *time.Time   `json:"nextRun,omitempty"`
	LastOccurrenceAt  *time.Time   `json:"lastOccurrenceAt,omitempty"`
	LastResult        rebootResult `json:"lastResult,omitempty"`
	LastReason        string       `json:"lastReason,omitempty"`
	LastOperationID   string       `json:"lastOperationId,omitempty"`
}

type rebootExecutionView struct {
	rebootExecution
	ServerName string `json:"serverName"`
}

type rebootRequest struct {
	ServerID              string     `json:"serverId"`
	Enabled               *bool      `json:"enabled"`
	Mode                  rebootMode `json:"mode"`
	Cron                  *string    `json:"cron"`
	IntervalValue         *int       `json:"intervalValue"`
	IntervalUnit          *string    `json:"intervalUnit"`
	DailyTimes            *[]string  `json:"dailyTimes"`
	ExecutionTimezone     string     `json:"executionTimezone"`
	AcknowledgeDisconnect bool       `json:"acknowledgeDisconnect"`
}

type rebootPreviewResponse struct {
	AsOf              time.Time   `json:"asOf"`
	Anchor            time.Time   `json:"anchor"`
	ExecutionTimezone string      `json:"executionTimezone"`
	Runs              []time.Time `json:"runs"`
}

var (
	errRebootNotFound = errors.New("reboot schedule not found")
	errRebootConflict = errors.New("reboot schedule changed or a restart is already pending")
	errRebootInvalid  = errors.New("invalid reboot definition")
)

type rebootDefinitionError struct{ cause error }

func (e rebootDefinitionError) Error() string { return e.cause.Error() }
func (e rebootDefinitionError) Unwrap() error { return errRebootInvalid }

func (s *State) initReboots() {
	if s.RebootSchedules == nil {
		s.RebootSchedules = map[string]rebootSchedule{}
	}
}

func (a *App) rebootNow() time.Time {
	if a.clock != nil {
		return a.clock().UTC()
	}
	return time.Now().UTC()
}

func (a *App) rebootSchedulingEnabled() bool {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("RSDW_REBOOTS_ENABLED")), "false") {
		return false
	}
	if a.demo {
		return true
	}
	return a.store != nil && a.store.path != "" && !strings.EqualFold(strings.TrimSpace(os.Getenv("RSDW_STATE_PERSISTENT")), "false")
}

func rebootExecutionActive(result rebootResult) bool {
	return result == rebootAwaiting || result == rebootUncertain
}

func (s *State) recoverReboots(now time.Time) {
	s.initReboots()
	ids := make([]string, 0, len(s.RebootSchedules))
	for id := range s.RebootSchedules {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		schedule := s.RebootSchedules[id]
		if !schedule.Enabled || schedule.NextRun == nil || schedule.NextRun.After(now) {
			continue
		}
		missedAt := *schedule.NextRun
		next, err := nextScheduleRun(schedule, now)
		if err != nil {
			schedule.Enabled = false
			schedule.NextRun = nil
			schedule.LastResult = rebootSkipped
			schedule.LastReason = "Schedule could not be recalculated during startup recovery: " + err.Error()
			s.RebootSchedules[id] = schedule
			continue
		}
		recordMissedReboot(s, schedule, missedAt, "C2 was unavailable; the overdue occurrence was skipped during startup recovery", now)
		schedule.LastResult = rebootMissed
		schedule.LastReason = "Skipped during startup recovery; schedules never catch up after downtime"
		schedule.LastOperationID = ""
		schedule.LastOccurrenceAt = cloneTimePtr(&missedAt)
		schedule.NextRun = cloneTimePtr(&next)
		schedule.UpdatedAt = now
		s.RebootSchedules[id] = schedule
	}
}

func nextScheduleRun(schedule rebootSchedule, after time.Time) (time.Time, error) {
	anchor := time.Time{}
	if schedule.IntervalAnchor != nil {
		anchor = *schedule.IntervalAnchor
	}
	return nextReboot(schedule.Definition, anchor, after)
}

func scheduleView(schedule rebootSchedule, servers map[string]Server) rebootScheduleView {
	view := rebootScheduleView{
		ID: schedule.ID, ServerID: schedule.Definition.ServerID, Mode: schedule.Definition.Mode,
		Cron: schedule.Definition.Cron, IntervalValue: schedule.Definition.IntervalValue,
		IntervalUnit: schedule.Definition.IntervalUnit, DailyTimes: append([]string(nil), schedule.Definition.DailyTimes...),
		ExecutionTimezone: schedule.Definition.ExecutionTimezone, Enabled: schedule.Enabled, Revision: schedule.Revision,
		CreatedAt: schedule.CreatedAt, UpdatedAt: schedule.UpdatedAt, NextRun: cloneTimePtr(schedule.NextRun),
		LastOccurrenceAt: cloneTimePtr(schedule.LastOccurrenceAt), LastResult: schedule.LastResult,
		LastReason: schedule.LastReason, LastOperationID: schedule.LastOperationID,
	}
	if server, ok := servers[view.ServerID]; ok {
		view.ServerName = worldLabel(server)
	}
	return view
}

func executionViews(history []rebootExecution, servers map[string]Server) []rebootExecutionView {
	result := make([]rebootExecutionView, 0, len(history))
	for _, execution := range history {
		view := rebootExecutionView{rebootExecution: execution}
		view.ServerName = worldLabel(servers[execution.ServerID])
		result = append(result, view)
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].RecordedAt.Equal(result[j].RecordedAt) {
			return result[i].ID > result[j].ID
		}
		return result[i].RecordedAt.After(result[j].RecordedAt)
	})
	return result
}

func (a *App) handleReboots(w http.ResponseWriter, r *http.Request) {
	if requestPrincipal(r).Role != RoleAdmin {
		writeError(w, http.StatusForbidden, "permission denied")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/reboots")
	if path == "" || path == "/" {
		switch r.Method {
		case http.MethodGet:
			a.listReboots(w)
		case http.MethodPost:
			a.createReboot(w, r)
		default:
			w.Header().Set("Allow", "GET, POST")
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return
	}
	if path == "/preview" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		a.previewReboot(w, r)
		return
	}
	id := strings.TrimPrefix(path, "/")
	if id == "" || strings.Contains(id, "/") {
		writeError(w, http.StatusNotFound, "reboot schedule not found")
		return
	}
	switch r.Method {
	case http.MethodPut:
		a.updateReboot(w, r, id)
	case http.MethodDelete:
		a.deleteReboot(w, r, id)
	default:
		w.Header().Set("Allow", "PUT, DELETE")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (a *App) listReboots(w http.ResponseWriter) {
	snapshot := a.store.Snapshot()
	schedules := make([]rebootScheduleView, 0, len(snapshot.RebootSchedules))
	for _, schedule := range snapshot.RebootSchedules {
		schedules = append(schedules, scheduleView(schedule, snapshot.Servers))
	}
	sort.Slice(schedules, func(i, j int) bool {
		if schedules[i].ServerName == schedules[j].ServerName {
			return schedules[i].ID < schedules[j].ID
		}
		return schedules[i].ServerName < schedules[j].ServerName
	})
	now := a.rebootNow()
	writeJSON(w, http.StatusOK, map[string]any{
		"schedules": schedules,
		"history":   executionViews(snapshot.RebootHistory, snapshot.Servers),
		"demo":      a.demo,
		"available": a.rebootSchedulingEnabled(),
		"now":       now,
	})
}

func decodeRebootJSON(w http.ResponseWriter, r *http.Request, request *rebootRequest) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(request); err != nil {
		if strings.Contains(err.Error(), "request body too large") {
			return fmt.Errorf("request body too large")
		}
		return fmt.Errorf("invalid reboot JSON: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("invalid reboot JSON: request must contain one object")
	}
	return nil
}

func normalizeRebootRequest(request rebootRequest, requireServer bool) (rebootDefinition, bool, error) {
	definition := rebootDefinition{ServerID: strings.TrimSpace(request.ServerID), Mode: request.Mode, ExecutionTimezone: strings.TrimSpace(request.ExecutionTimezone)}
	if requireServer && definition.ServerID == "" {
		return rebootDefinition{}, false, errors.New("serverId is required")
	}
	if request.Enabled == nil {
		return rebootDefinition{}, false, errors.New("enabled is required")
	}
	switch request.Mode {
	case rebootModeCron:
		if request.Cron == nil || strings.TrimSpace(*request.Cron) == "" {
			return rebootDefinition{}, false, errors.New("cron is required for cron schedules")
		}
		if request.IntervalValue != nil || request.IntervalUnit != nil || request.DailyTimes != nil {
			return rebootDefinition{}, false, errors.New("cron schedules must not include interval or daily fields")
		}
		definition.Cron = strings.TrimSpace(*request.Cron)
	case rebootModeInterval:
		if request.IntervalValue == nil || request.IntervalUnit == nil {
			return rebootDefinition{}, false, errors.New("intervalValue and intervalUnit are required for interval schedules")
		}
		if request.Cron != nil || request.DailyTimes != nil {
			return rebootDefinition{}, false, errors.New("interval schedules must not include cron or daily fields")
		}
		definition.IntervalValue, definition.IntervalUnit = *request.IntervalValue, strings.TrimSpace(*request.IntervalUnit)
	case rebootModeDaily:
		if request.DailyTimes == nil || len(*request.DailyTimes) == 0 {
			return rebootDefinition{}, false, errors.New("dailyTimes is required for daily schedules")
		}
		if request.Cron != nil || request.IntervalValue != nil || request.IntervalUnit != nil {
			return rebootDefinition{}, false, errors.New("daily schedules must not include cron or interval fields")
		}
		definition.DailyTimes = append([]string(nil), (*request.DailyTimes)...)
	default:
		return rebootDefinition{}, false, errors.New("mode must be cron, interval, or daily")
	}
	timing, err := parseRebootTiming(definition)
	if err != nil {
		return rebootDefinition{}, false, err
	}
	if definition.Mode == rebootModeDaily {
		definition.DailyTimes = make([]string, 0, len(timing.daily))
		for _, minute := range timing.daily {
			definition.DailyTimes = append(definition.DailyTimes, fmt.Sprintf("%02d:%02d", int(minute)/60, int(minute)%60))
		}
	}
	return definition, *request.Enabled, nil
}

func (a *App) previewReboot(w http.ResponseWriter, r *http.Request) {
	var request rebootRequest
	if err := decodeRebootJSON(w, r, &request); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "too large") {
			status = http.StatusRequestEntityTooLarge
		}
		writeError(w, status, err.Error())
		return
	}
	if request.Enabled == nil {
		enabled := false
		request.Enabled = &enabled
	}
	definition, _, err := normalizeRebootRequest(request, false)
	if err != nil {
		writeRebootValidationError(w, err)
		return
	}
	if definition.ServerID != "" {
		snapshot := a.store.Snapshot()
		if snapshot.deleting(definition.ServerID) {
			writeRebootFieldError(w, "serverId", "serverId identifies a server with a deletion in progress or a recorded deletion")
			return
		}
		if _, ok := snapshot.Servers[definition.ServerID]; !ok {
			writeRebootFieldError(w, "serverId", "serverId does not identify an existing server")
			return
		}
	}
	now := a.rebootNow()
	runs, err := rebootPreview(definition, now, now, 5)
	if err != nil {
		writeRebootValidationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rebootPreviewResponse{AsOf: now, Anchor: now, ExecutionTimezone: definition.ExecutionTimezone, Runs: runs})
}

func (a *App) createReboot(w http.ResponseWriter, r *http.Request) {
	var request rebootRequest
	if err := decodeRebootJSON(w, r, &request); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "too large") {
			status = http.StatusRequestEntityTooLarge
		}
		writeError(w, status, err.Error())
		return
	}
	definition, enabled, err := normalizeRebootRequest(request, true)
	if err != nil {
		writeRebootValidationError(w, err)
		return
	}
	if enabled && !request.AcknowledgeDisconnect {
		writeRebootFieldError(w, "acknowledgeDisconnect", "Confirm that connected players may be disconnected before enabling a schedule")
		return
	}
	if enabled && !a.rebootSchedulingEnabled() {
		writeError(w, http.StatusServiceUnavailable, "scheduled reboots are disabled for this deployment")
		return
	}
	actor := requestPrincipal(r).Subject
	schedule, err := a.saveReboot("", definition, enabled, actor)
	if err != nil {
		a.writeRebootSaveError(w, err)
		return
	}
	snapshot := a.store.Snapshot()
	writeJSON(w, http.StatusCreated, scheduleView(schedule, snapshot.Servers))
}

func (a *App) updateReboot(w http.ResponseWriter, r *http.Request, id string) {
	var request rebootRequest
	if err := decodeRebootJSON(w, r, &request); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "too large") {
			status = http.StatusRequestEntityTooLarge
		}
		writeError(w, status, err.Error())
		return
	}
	definition, enabled, err := normalizeRebootRequest(request, true)
	if err != nil {
		writeRebootValidationError(w, err)
		return
	}
	if enabled && !request.AcknowledgeDisconnect {
		writeRebootFieldError(w, "acknowledgeDisconnect", "Confirm that connected players may be disconnected before enabling a schedule")
		return
	}
	if enabled && !a.rebootSchedulingEnabled() {
		writeError(w, http.StatusServiceUnavailable, "scheduled reboots are disabled for this deployment")
		return
	}
	schedule, err := a.saveReboot(id, definition, enabled, requestPrincipal(r).Subject)
	if err != nil {
		a.writeRebootSaveError(w, err)
		return
	}
	snapshot := a.store.Snapshot()
	writeJSON(w, http.StatusOK, scheduleView(schedule, snapshot.Servers))
}

func (a *App) deleteReboot(w http.ResponseWriter, r *http.Request, id string) {
	err := a.store.Update(func(state *State) error {
		state.initReboots()
		schedule, ok := state.RebootSchedules[id]
		if !ok {
			return errRebootNotFound
		}
		delete(state.RebootSchedules, id)
		server := state.Servers[schedule.Definition.ServerID]
		appendRebootAudit(state, server, requestPrincipal(r).Subject, id, "Reboot schedule deleted", "A claimed operation, if any, continues; deletion does not cancel external work")
		return nil
	})
	if err != nil {
		a.writeRebootSaveError(w, err)
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

func (a *App) saveReboot(id string, definition rebootDefinition, enabled bool, actor string) (rebootSchedule, error) {
	var result rebootSchedule
	err := a.store.Update(func(state *State) error {
		state.initReboots()
		now := a.rebootNow()
		server, ok := state.Servers[definition.ServerID]
		if !ok {
			return fmt.Errorf("server not found")
		}
		if state.deleting(definition.ServerID) {
			return fmt.Errorf("server is being deleted")
		}
		if id == "" && len(state.RebootSchedules) >= maxRebootSchedules {
			return rebootDefinitionError{cause: errors.New("the deployment has reached the maximum of 256 reboot schedules")}
		}
		old, exists := state.RebootSchedules[id]
		if id != "" && !exists {
			return errRebootNotFound
		}
		if id == "" {
			id = randomID()
			for state.RebootSchedules[id].ID != "" {
				id = randomID()
			}
		}
		changed := !exists || !sameRebootDefinition(old.Definition, definition)
		resetAnchor := !exists || changed || !old.Enabled && enabled
		candidate := old
		candidate.ID = id
		candidate.Definition = definition
		candidate.Enabled = enabled
		candidate.UpdatedAt = now
		if !exists {
			candidate.CreatedAt, candidate.Revision = now, 1
		} else {
			candidate.Revision++
		}
		if definition.Mode == rebootModeInterval {
			if resetAnchor || candidate.IntervalAnchor == nil {
				candidate.IntervalAnchor = cloneTimePtr(&now)
			}
		} else {
			candidate.IntervalAnchor = nil
		}
		next, err := nextScheduleRun(candidate, now)
		if err != nil {
			return rebootDefinitionError{cause: err}
		}
		if enabled && (!exists || changed || old.NextRun == nil) {
			candidate.NextRun = cloneTimePtr(&next)
		} else if !enabled {
			candidate.NextRun = nil
		}
		state.RebootSchedules[id] = candidate
		message := "Reboot schedule created"
		if exists {
			message = "Reboot schedule updated"
		}
		appendRebootAudit(state, server, actor, id, message, rebootDefinitionSummary(definition))
		result = candidate
		return nil
	})
	return result, err
}

func sameRebootDefinition(a, b rebootDefinition) bool {
	if a.ServerID != b.ServerID || a.Mode != b.Mode || a.Cron != b.Cron || a.IntervalValue != b.IntervalValue || a.IntervalUnit != b.IntervalUnit || a.ExecutionTimezone != b.ExecutionTimezone || len(a.DailyTimes) != len(b.DailyTimes) {
		return false
	}
	for i := range a.DailyTimes {
		if a.DailyTimes[i] != b.DailyTimes[i] {
			return false
		}
	}
	return true
}

func appendRebootAudit(state *State, server Server, actor, scheduleID, message, details string) {
	addEvent(state, Event{ID: randomID(), Timestamp: time.Now().UTC(), ServerID: server.ID, ServerName: server.Name, Category: "maintenance", Severity: "success", Message: message, Details: details, Kind: RebootScheduleChanged, Source: "C2 operator", Accuracy: "observed", ScheduleID: scheduleID, Actor: actor})
}

func rebootDefinitionSummary(definition rebootDefinition) string {
	switch definition.Mode {
	case rebootModeCron:
		return fmt.Sprintf("Cron %q in %s", definition.Cron, definition.ExecutionTimezone)
	case rebootModeInterval:
		return fmt.Sprintf("Every %d %s in %s", definition.IntervalValue, definition.IntervalUnit, definition.ExecutionTimezone)
	default:
		return fmt.Sprintf("Daily %s in %s", strings.Join(definition.DailyTimes, ", "), definition.ExecutionTimezone)
	}
}

func writeRebootValidationError(w http.ResponseWriter, err error) {
	field := "mode"
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "timezone"):
		field = "executionTimezone"
	case strings.Contains(message, "cron"):
		field = "cron"
	case strings.Contains(message, "interval"):
		field = "intervalValue"
	case strings.Contains(message, "daily"):
		field = "dailyTimes"
	case strings.Contains(message, "server"):
		field = "serverId"
	case strings.Contains(message, "enabled"):
		field = "enabled"
	}
	writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "fields": map[string]string{field: err.Error()}})
}

func writeRebootFieldError(w http.ResponseWriter, field, message string) {
	writeJSON(w, http.StatusBadRequest, map[string]any{"error": message, "fields": map[string]string{field: message}})
}

func (a *App) writeRebootSaveError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errRebootNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, errRebootConflict):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, errRebootInvalid):
		writeRebootValidationError(w, err)
	case strings.Contains(err.Error(), "server not found"), strings.Contains(err.Error(), "server is being deleted"):
		writeRebootFieldError(w, "serverId", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "could not persist reboot schedule")
	}
}

func recordRebootExecution(state *State, schedule rebootSchedule, occurrence time.Time, operationID string, result rebootResult, reason string, recordedAt time.Time) rebootExecution {
	id := randomID()
	execution := rebootExecution{ID: id, OccurrenceID: id, ScheduleID: schedule.ID, Revision: schedule.Revision, ServerID: schedule.Definition.ServerID, OccurrenceAt: occurrence.UTC(), RecordedAt: recordedAt.UTC(), OperationID: operationID, Result: result, Reason: reason}
	state.RebootHistory = append(state.RebootHistory, execution)
	trimRebootHistory(state)
	return execution
}

func recordMissedReboot(state *State, schedule rebootSchedule, occurrence time.Time, reason string, recordedAt time.Time) rebootExecution {
	execution := recordRebootExecution(state, schedule, occurrence, "", rebootMissed, reason, recordedAt)
	if recordedAt.After(execution.OccurrenceAt) {
		for index := range state.RebootHistory {
			if state.RebootHistory[index].ID == execution.ID {
				state.RebootHistory[index].MissedThrough = cloneTimePtr(&recordedAt)
				break
			}
		}
		execution.MissedThrough = cloneTimePtr(&recordedAt)
	}
	return execution
}

func trimRebootHistory(state *State) {
	for len(state.RebootHistory) > maxRebootHistory {
		index := -1
		for i, execution := range state.RebootHistory {
			if !rebootExecutionActive(execution.Result) {
				index = i
				break
			}
		}
		if index < 0 {
			return
		}
		copy(state.RebootHistory[index:], state.RebootHistory[index+1:])
		state.RebootHistory = state.RebootHistory[:len(state.RebootHistory)-1]
	}
}

func countActiveReboots(state *State) int {
	count := 0
	for _, execution := range state.RebootHistory {
		if rebootExecutionActive(execution.Result) {
			count++
		}
	}
	return count
}

func updateScheduleResult(state *State, execution rebootExecution) {
	schedule, ok := state.RebootSchedules[execution.ScheduleID]
	if !ok {
		return
	}
	if schedule.LastOccurrenceAt == nil || !execution.OccurrenceAt.Before(*schedule.LastOccurrenceAt) {
		schedule.LastOccurrenceAt = cloneTimePtr(&execution.OccurrenceAt)
		schedule.LastResult = execution.Result
		schedule.LastReason = execution.Reason
		schedule.LastOperationID = execution.OperationID
		state.RebootSchedules[execution.ScheduleID] = schedule
	}
}

func finishRebootOccurrences(state *State, operationID string, result rebootResult, reason string, at time.Time) {
	if operationID == "" {
		return
	}
	for index := range state.RebootHistory {
		execution := &state.RebootHistory[index]
		if execution.OperationID != operationID || !rebootExecutionActive(execution.Result) {
			continue
		}
		execution.Result, execution.Reason, execution.RecordedAt = result, reason, at.UTC()
		updateScheduleResult(state, *execution)
	}
	trimRebootHistory(state)
}

type rebootDispatch struct {
	server Server
	op     RestartOperation
}

func reserveRestart(state *State, serverID string, now time.Time) (rebootDispatch, error) {
	server, ok := state.Servers[serverID]
	if !ok {
		return rebootDispatch{}, errRebootNotFound
	}
	p := state.Producers[serverID]
	if p.Restart != nil {
		return rebootDispatch{}, errRebootConflict
	}
	op := RestartOperation{ID: randomID(), Runtime: p.Runtime, RequestedAt: now.UTC()}
	p.Restart = &op
	p.resetStreak()
	p.PlayerAt = time.Time{}
	state.Producers[serverID] = p
	server.Status, server.LastRestart, server.RestartOperation = StatusStarting, op.RequestedAt.Format(time.RFC3339Nano), op.ID
	state.Servers[serverID] = server
	emitAlert(state, server, RestartRequested, op.ID, "Restart recorded; completion requires a ready marked replacement runtime", op.RequestedAt)
	return rebootDispatch{server: server, op: op}, nil
}

func (a *App) claimManualRestart(serverID string, now time.Time) (rebootDispatch, error) {
	var dispatch rebootDispatch
	err := a.store.Update(func(state *State) error {
		var err error
		dispatch, err = reserveRestart(state, serverID, now)
		return err
	})
	return dispatch, err
}

func (a *App) dispatchRestartOperation(ctx context.Context, dispatch rebootDispatch) (commandErr, persistenceErr error) {
	commandCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	commandErr = a.orchestrator.Restart(commandCtx, dispatch.server)
	if a.demo {
		persistenceErr = a.store.Update(func(state *State) error {
			p := state.Producers[dispatch.server.ID]
			if p.Restart == nil || p.Restart.ID != dispatch.op.ID {
				return nil
			}
			p.Restart = nil
			state.Producers[dispatch.server.ID] = p
			server := state.Servers[dispatch.server.ID]
			server.Status, server.RestartOperation = StatusOnline, ""
			state.Servers[dispatch.server.ID] = server
			emitAlert(state, server, RestartCompleted, dispatch.op.ID, "Demo simulated restart; no Kubernetes operation", a.rebootNow())
			finishRebootOccurrences(state, dispatch.op.ID, rebootCompleted, "Demo simulated restart; no Kubernetes operation", a.rebootNow())
			return nil
		})
		return commandErr, persistenceErr
	}
	if commandErr != nil {
		persistenceErr = a.store.Update(func(state *State) error {
			p := state.Producers[dispatch.server.ID]
			if p.Restart != nil && p.Restart.ID == dispatch.op.ID {
				p.Restart.CommandUncertain = true
				state.Producers[dispatch.server.ID] = p
				finishRebootOccurrences(state, dispatch.op.ID, rebootUncertain, "Restart command outcome is unknown; fresh observations will reconcile it", a.rebootNow())
			}
			return nil
		})
	}
	return commandErr, persistenceErr
}

func (a *App) scanReboots(ctx context.Context, now time.Time) {
	if !a.rebootSchedulingEnabled() {
		return
	}
	snapshot := a.store.Snapshot()
	groups := map[string][]string{}
	for id, schedule := range snapshot.RebootSchedules {
		if schedule.Enabled && schedule.NextRun != nil && !schedule.NextRun.After(now) {
			groups[schedule.Definition.ServerID] = append(groups[schedule.Definition.ServerID], id)
		}
	}
	serverIDs := make([]string, 0, len(groups))
	for serverID := range groups {
		serverIDs = append(serverIDs, serverID)
	}
	sort.Strings(serverIDs)
	for _, serverID := range serverIDs {
		if ctx.Err() != nil {
			return
		}
		sort.Strings(groups[serverID])
		if !a.lifecycleMu.TryLock() {
			continue
		}
		dispatch, err := a.claimScheduledReboots(serverID, groups[serverID])
		if err != nil {
			a.lifecycleMu.Unlock()
			if !errors.Is(err, errRebootConflict) && !errors.Is(err, errRebootNotFound) {
				log.Printf("scheduled reboot claim for %s: %v", serverID, err)
			}
			continue
		}
		if dispatch.server.ID == "" {
			a.lifecycleMu.Unlock()
			continue
		}
		commandErr, persistenceErr := a.dispatchRestartOperation(ctx, dispatch)
		a.lifecycleMu.Unlock()
		if persistenceErr != nil {
			log.Printf("scheduled reboot result for %s: %v", serverID, persistenceErr)
		} else if commandErr != nil {
			log.Printf("scheduled reboot command for %s: %v", serverID, commandErr)
		}
	}
}

func (a *App) claimScheduledReboots(serverID string, ids []string) (rebootDispatch, error) {
	var dispatch rebootDispatch
	err := a.store.Update(func(state *State) error {
		state.initReboots()
		claimNow := a.rebootNow()
		_, ok := state.Servers[serverID]
		if state.deleting(serverID) {
			for _, id := range ids {
				if schedule, exists := state.RebootSchedules[id]; exists && schedule.Enabled && schedule.NextRun != nil && !schedule.NextRun.After(claimNow) {
					recordAndAdvanceReboot(state, schedule, rebootSkipped, "Target server deletion is in progress or recorded", claimNow)
				}
			}
			return nil
		}
		if !ok {
			for _, id := range ids {
				if schedule, exists := state.RebootSchedules[id]; exists && schedule.Enabled && schedule.NextRun != nil && !schedule.NextRun.After(claimNow) {
					recordAndAdvanceReboot(state, schedule, rebootSkipped, "Target server no longer exists", claimNow)
				}
			}
			return nil
		}
		due := make([]rebootSchedule, 0, len(ids))
		for _, id := range ids {
			schedule, exists := state.RebootSchedules[id]
			if !exists || !schedule.Enabled || schedule.NextRun == nil || schedule.NextRun.After(claimNow) {
				continue
			}
			if claimNow.Sub(*schedule.NextRun) > rebootLateWindow {
				recordAndAdvanceReboot(state, schedule, rebootMissed, "Occurrence was more than 60 seconds late; catch-up is disabled", claimNow)
				continue
			}
			if _, err := parseRebootTiming(schedule.Definition); err != nil {
				schedule.Enabled = false
				schedule.NextRun = nil
				schedule.LastResult, schedule.LastReason = rebootSkipped, "Schedule became invalid and was disabled: "+err.Error()
				state.RebootSchedules[schedule.ID] = schedule
				continue
			}
			due = append(due, schedule)
		}
		if len(due) == 0 {
			return nil
		}
		if pending := state.Producers[serverID].Restart; pending != nil {
			for _, schedule := range due {
				recordAndAdvanceReboot(state, schedule, rebootSkipped, "A restart is already awaiting reconciliation; this occurrence was not attached", claimNow)
			}
			return nil
		}
		if countActiveReboots(state)+len(due) > maxRebootActive {
			for _, schedule := range due {
				recordAndAdvanceReboot(state, schedule, rebootSkipped, "The active reboot audit limit was reached; try again at the next occurrence", claimNow)
			}
			return nil
		}
		var err error
		dispatch, err = reserveRestart(state, serverID, claimNow)
		if err != nil {
			return err
		}
		for _, schedule := range due {
			execution := recordRebootExecution(state, schedule, *schedule.NextRun, dispatch.op.ID, rebootAwaiting, "Restart requested; waiting for a fresh ready replacement runtime", claimNow)
			schedule.LastOccurrenceAt = cloneTimePtr(&execution.OccurrenceAt)
			schedule.LastResult, schedule.LastReason, schedule.LastOperationID = rebootAwaiting, execution.Reason, dispatch.op.ID
			advanceRebootSchedule(state, &schedule, claimNow)
		}
		return nil
	})
	return dispatch, err
}

func recordAndAdvanceReboot(state *State, schedule rebootSchedule, result rebootResult, reason string, now time.Time) {
	if schedule.NextRun == nil {
		return
	}
	var execution rebootExecution
	if result == rebootMissed {
		execution = recordMissedReboot(state, schedule, *schedule.NextRun, reason, now)
	} else {
		execution = recordRebootExecution(state, schedule, *schedule.NextRun, "", result, reason, now)
	}
	schedule.LastOccurrenceAt = cloneTimePtr(&execution.OccurrenceAt)
	schedule.LastResult, schedule.LastReason, schedule.LastOperationID = result, reason, ""
	advanceRebootSchedule(state, &schedule, now)
}

func advanceRebootSchedule(state *State, schedule *rebootSchedule, now time.Time) {
	if !schedule.Enabled {
		schedule.NextRun = nil
		schedule.UpdatedAt = now
		state.RebootSchedules[schedule.ID] = *schedule
		return
	}
	next, err := nextScheduleRun(*schedule, now)
	if err != nil {
		schedule.Enabled, schedule.NextRun = false, nil
		schedule.LastResult, schedule.LastReason = rebootSkipped, "No future occurrence could be calculated; schedule disabled: "+err.Error()
	} else {
		schedule.NextRun = cloneTimePtr(&next)
	}
	schedule.UpdatedAt = now
	state.RebootSchedules[schedule.ID] = *schedule
}

func (a *App) runRebootScheduler(ctx context.Context) {
	ticker := time.NewTicker(rebootScanInterval)
	defer ticker.Stop()
	for {
		a.scanReboots(ctx, a.rebootNow())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
