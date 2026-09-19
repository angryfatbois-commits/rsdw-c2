package main

import (
	"fmt"
	"time"
)

func warningOccurrence(schedule rebootSchedule) string {
	if schedule.NextRun == nil {
		return ""
	}
	return fmt.Sprintf("%s/%d/%s", schedule.ID, schedule.Revision, schedule.NextRun.UTC().Format(time.RFC3339Nano))
}

func isScheduledRestartWarning(event Event) bool {
	return event.Kind == RestartWarning && event.Evidence.RestartWarning != nil && event.Evidence.RestartWarning.Trigger == "scheduled"
}

func memoryPressureWarningOccurrence(serverID string, pressure MemoryPressureState) string {
	return fmt.Sprintf("memory-pressure/%s/%s/%s", serverID, pressure.Runtime, pressure.Since.UTC().Format(time.RFC3339Nano))
}

func restartWarningEnabled(state *State, serverID string) bool {
	for _, integration := range state.Integrations {
		if !integration.Enabled || !integration.Rules[RestartWarning] || integration.target().validate() != nil {
			continue
		}
		for _, targetID := range integration.ServerIDs {
			if targetID == serverID {
				return true
			}
		}
	}
	return false
}

func warningEligible(state *State, schedule rebootSchedule, now time.Time) bool {
	server, exists := state.Servers[schedule.Definition.ServerID]
	return exists && !state.deleting(server.ID) && server.Status != StatusStopped && state.Producers[server.ID].Restart == nil && schedule.Enabled && schedule.Definition.WarningMinutes > 0 && schedule.NextRun != nil && now.Before(*schedule.NextRun)
}

func warningDeliveryValid(state *State, delivery Delivery, now time.Time) bool {
	if delivery.Event.Kind != RestartWarning {
		return true
	}
	if warning := delivery.Event.Evidence.RestartWarning; warning != nil && warning.Trigger == "memory-pressure" {
		server, ok := state.Servers[delivery.Event.ServerID]
		producer, producerOK := state.Producers[delivery.Event.ServerID]
		return ok && producerOK && !state.deleting(server.ID) && server.Status != StatusStopped && producer.Restart == nil && producer.MemoryPressure != nil && producer.MemoryPressure.WarningOccurrence == delivery.Event.OccurrenceID && now.Before(warning.RestartAt)
	}
	schedule, ok := state.RebootSchedules[delivery.Event.ScheduleID]
	return ok && warningEligible(state, schedule, now) && delivery.Event.OccurrenceID == warningOccurrence(schedule)
}

func cancelObsoleteWarnings(state *State, now time.Time) {
	for id, delivery := range state.Deliveries {
		if (delivery.Status == DeliveryPending || delivery.Status == DeliveryRetry) && !warningDeliveryValid(state, delivery, now) {
			delivery.Status, delivery.Result, delivery.UpdatedAt = DeliveryFailed, "Restart warning cancelled because the occurrence is no longer eligible", now
			state.Deliveries[id] = delivery
		}
	}
}

func (a *App) scanRestartWarnings(now time.Time) error {
	return a.store.Update(func(state *State) error {
		cancelObsoleteWarnings(state, now)
		for id, schedule := range state.RebootSchedules {
			if !schedule.Enabled || schedule.Definition.WarningMinutes <= 0 || schedule.NextRun == nil || !now.Before(*schedule.NextRun) || now.Before(schedule.NextRun.Add(-time.Duration(schedule.Definition.WarningMinutes)*time.Minute)) {
				continue
			}
			occurrence := warningOccurrence(schedule)
			if schedule.LastWarningOccurrence == occurrence {
				continue
			}
			if !warningEligible(state, schedule, now) {
				continue
			}
			schedule.LastWarningOccurrence = occurrence
			state.RebootSchedules[id] = schedule
			warning := &RestartWarningEvidence{Trigger: "scheduled", RestartAt: *schedule.NextRun, Minutes: int(schedule.NextRun.Sub(now).Minutes() + 0.999999)}
			emitAlertEvidence(state, state.Servers[schedule.Definition.ServerID], RestartWarning, "", "Scheduled restart approaching", now, AlertEvidence{RestartWarning: warning}, schedule.ID, occurrence)
		}
		return nil
	})
}
