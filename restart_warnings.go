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

func warningEligible(state *State, schedule rebootSchedule, now time.Time) bool {
	server, exists := state.Servers[schedule.Definition.ServerID]
	return exists && !state.deleting(server.ID) && server.Status != StatusStopped && state.Producers[server.ID].Restart == nil && schedule.Enabled && schedule.Definition.WarningMinutes > 0 && schedule.NextRun != nil && now.Before(*schedule.NextRun)
}

func warningDeliveryValid(state *State, delivery Delivery, now time.Time) bool {
	if delivery.Event.Kind != RestartWarning {
		return true
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
			schedule.LastWarningOccurrence = occurrence
			state.RebootSchedules[id] = schedule
			if !warningEligible(state, schedule, now) {
				continue
			}
			warning := &RestartWarningEvidence{Trigger: "scheduled", RestartAt: *schedule.NextRun, Minutes: int(schedule.NextRun.Sub(now).Minutes() + 0.999999)}
			emitAlertEvidence(state, state.Servers[schedule.Definition.ServerID], RestartWarning, "", "Scheduled restart approaching", now, AlertEvidence{RestartWarning: warning}, schedule.ID, occurrence)
		}
		return nil
	})
}
