package main

import (
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	maxLifecycleEvents = 2000
	maxRoutineEvents   = 500
	eventPageDefault   = 100
	eventExportLimit   = 2500
)

type eventClass int

const (
	eventRoutine eventClass = iota
	eventLifecycle
)

type eventQuery struct {
	Text     string
	Category string
	ServerID string
	Since    time.Time
	Limit    int
	Offset   int
}

type eventList struct {
	Events   []Event `json:"events"`
	Total    int     `json:"total"`
	Warnings int     `json:"warnings"`
	Critical int     `json:"critical"`
}

func classifyEvent(event Event) eventClass {
	switch event.Kind {
	case PlayerJoined, PlayerLimitReached, IntegrationTest:
		return eventRoutine
	}
	if event.Category == "player" {
		return eventRoutine
	}
	return eventLifecycle
}

func eventNewer(a, b Event) bool {
	// Prune and list by timestamp then id because emitAlert can share an observation time.
	if a.Timestamp.Equal(b.Timestamp) {
		return a.ID > b.ID
	}
	return a.Timestamp.After(b.Timestamp)
}

func pruneEvents(events []Event) []Event {
	var lifecycle, routine []int
	for i, event := range events {
		if classifyEvent(event) == eventRoutine {
			routine = append(routine, i)
		} else {
			lifecycle = append(lifecycle, i)
		}
	}
	if len(lifecycle) <= maxLifecycleEvents && len(routine) <= maxRoutineEvents {
		return events
	}
	keep := make([]bool, len(events))
	markNewestEvents(keep, events, lifecycle, maxLifecycleEvents)
	markNewestEvents(keep, events, routine, maxRoutineEvents)
	out := make([]Event, 0, maxLifecycleEvents+maxRoutineEvents)
	for i, event := range events {
		if keep[i] {
			out = append(out, event)
		}
	}
	return out
}

func markNewestEvents(keep []bool, events []Event, indexes []int, limit int) {
	sort.Slice(indexes, func(i, j int) bool {
		return eventNewer(events[indexes[i]], events[indexes[j]])
	})
	if len(indexes) > limit {
		indexes = indexes[:limit]
	}
	for _, index := range indexes {
		keep[index] = true
	}
}

func addEvent(state *State, event Event) {
	state.Events = pruneEvents(append(state.Events, event.clone()))
}

func appendEvent(state *State, server Server, category, severity, message, details string) {
	addEvent(state, Event{ID: randomID(), Timestamp: time.Now().UTC(), ServerID: server.ID, ServerName: worldLabel(server), Category: category, Severity: severity, Message: message, Details: details})
}

func parseEventQuery(values url.Values) (eventQuery, error) {
	q := eventQuery{
		Text:     strings.ToLower(strings.TrimSpace(values.Get("query"))),
		Category: strings.TrimSpace(values.Get("category")),
		ServerID: strings.TrimSpace(values.Get("serverId")),
		Limit:    boundedInt(values.Get("limit"), eventPageDefault, 1, eventExportLimit),
	}
	if q.Category == "all" {
		q.Category = ""
	}
	if q.ServerID == "all" {
		q.ServerID = ""
	}
	if raw := strings.TrimSpace(values.Get("since")); raw != "" {
		since, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return eventQuery{}, errors.New("since must be an RFC3339 timestamp")
		}
		q.Since = since
	}
	if raw := strings.TrimSpace(values.Get("offset")); raw != "" {
		offset, err := strconv.Atoi(raw)
		if err == nil && offset > 0 {
			q.Offset = offset
		}
	}
	return q, nil
}

func queryEvents(events []Event, q eventQuery) eventList {
	matched := make([]Event, 0)
	for _, event := range events {
		if q.Category != "" && event.Category != q.Category {
			continue
		}
		if q.ServerID != "" && event.ServerID != q.ServerID {
			continue
		}
		if !q.Since.IsZero() && event.Timestamp.Before(q.Since) {
			continue
		}
		if q.Text != "" && !strings.Contains(strings.ToLower(event.Message+" "+event.Details+" "+event.ServerName), q.Text) {
			continue
		}
		matched = append(matched, event)
	}
	sort.Slice(matched, func(i, j int) bool {
		return eventNewer(matched[i], matched[j])
	})
	list := eventList{Events: make([]Event, 0), Total: len(matched)}
	for _, event := range matched {
		if event.Severity == "warning" {
			list.Warnings++
		}
		if event.Severity == "critical" || event.Severity == "error" {
			// Critical includes error because the UI already treats both as the Critical stat.
			list.Critical++
		}
	}
	if q.Offset >= len(matched) || q.Limit <= 0 {
		return list
	}
	end := q.Offset + q.Limit
	if end > len(matched) {
		end = len(matched)
	}
	list.Events = matched[q.Offset:end]
	return list
}

func (a *App) handleEvents(w http.ResponseWriter, r *http.Request) {
	q, err := parseEventQuery(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, queryEvents(a.store.Snapshot().Events, q))
}
