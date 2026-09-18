package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestClassifyEvent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event Event
		want  eventClass
	}{
		{"player joined kind", Event{Kind: PlayerJoined, Category: "player"}, eventRoutine},
		{"player limit kind", Event{Kind: PlayerLimitReached, Category: "player"}, eventRoutine},
		{"player category without kind", Event{Category: "player", Message: "Player connected"}, eventRoutine},
		{"integration test", Event{Kind: IntegrationTest, Category: "system"}, eventRoutine},
		{"restart requested", Event{Kind: RestartRequested, Category: "system"}, eventLifecycle},
		{"restart completed", Event{Kind: RestartCompleted, Category: "system"}, eventLifecycle},
		{"restart failed", Event{Kind: RestartFailed, Category: "system"}, eventLifecycle},
		{"delete kind-empty system", Event{Category: "system", Message: "Server deleted"}, eventLifecycle},
		{"display only kind-empty system", Event{Category: "system", Severity: "success", Message: "display only"}, eventLifecycle},
		{"settings kind-empty system", Event{Category: "system", Message: "Settings apply requested"}, eventLifecycle},
		{"update kind-empty", Event{Category: "update", Message: "Image update requested"}, eventLifecycle},
		{"reboot schedule", Event{Kind: RebootScheduleChanged, Category: "maintenance"}, eventLifecycle},
		{"health down", Event{Kind: ServerDown, Category: "health"}, eventLifecycle},
		{"health recovered", Event{Kind: ServerRecovered, Category: "health"}, eventLifecycle},
		{"backup started", Event{Kind: BackupStarted, Category: "system"}, eventLifecycle},
		{"kind-empty health", Event{Category: "health", Message: "Health check passed"}, eventLifecycle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyEvent(tc.event); got != tc.want {
				t.Fatalf("classifyEvent(%+v) = %v, want %v", tc.event, got, tc.want)
			}
		})
	}
}

func TestPruneEventsKeepsLifecycleThroughRoutineFlood(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	events := make([]Event, 0, 601)
	for i := 0; i < 600; i++ {
		events = append(events, Event{
			ID:        fmt.Sprintf("p%03d", i),
			Timestamp: now.Add(time.Duration(i) * time.Second),
			Kind:      PlayerJoined,
			Category:  "player",
			Message:   "Player joined (approximate count increase)",
		})
	}
	events = append(events, Event{ID: "restart", Timestamp: now, Kind: RestartRequested, Category: "system", Message: "Restart requested"})
	pruned := pruneEvents(events)
	if !containsEventID(pruned, "restart") {
		t.Fatal("lifecycle restart was pruned")
	}
	if got := countEventClass(pruned, eventRoutine); got != maxRoutineEvents {
		t.Fatalf("routine events = %d, want %d", got, maxRoutineEvents)
	}
	if got := countEventClass(pruned, eventLifecycle); got != 1 {
		t.Fatalf("lifecycle events = %d, want 1", got)
	}
	if want := append(slices.Clone(events[100:600]), events[600]); !eventIDSeqEqual(pruned, want) {
		t.Fatalf("survivors = %v, want original relative order %v", eventIDs(pruned), eventIDs(want))
	}
	again := pruneEvents(pruned)
	if !eventIDSeqEqual(pruned, again) {
		t.Fatalf("second prune changed events: %v vs %v", eventIDs(pruned), eventIDs(again))
	}
}

func TestQueryEventsFiltersPagesAndCounts(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	events := []Event{
		{ID: "a", Timestamp: now.Add(-2 * time.Hour), ServerID: "east", ServerName: "East", Category: "system", Severity: "success", Message: "Restart requested", Details: "operator"},
		{ID: "b", Timestamp: now.Add(-time.Hour), ServerID: "west", ServerName: "West", Category: "player", Severity: "warning", Message: "Player joined", Details: "approx"},
		{ID: "c", Timestamp: now.Add(-30 * time.Minute), ServerID: "east", ServerName: "East", Category: "health", Severity: "error", Message: "Server down", Details: "unhealthy"},
		{ID: "d", Timestamp: now.Add(-10 * time.Minute), ServerID: "east", ServerName: "East", Category: "update", Severity: "warning", Message: "Update available", Details: "image"},
		{ID: "e", Timestamp: now.Add(-10 * time.Minute), ServerID: "east", ServerName: "East", Category: "system", Severity: "critical", Message: "Restart failed", Details: "deadline"},
	}
	list := queryEvents(events, eventQuery{Since: now.Add(-90 * time.Minute), Limit: 10})
	if list.Total != 4 || len(list.Events) != 4 || list.Warnings != 2 || list.Critical != 2 {
		t.Fatalf("since filter list = %+v", list)
	}
	if eventIDs(list.Events)[0] != "e" || eventIDs(list.Events)[1] != "d" {
		t.Fatalf("equal timestamps should sort by id desc, got %v", eventIDs(list.Events))
	}
	byServer := queryEvents(events, eventQuery{ServerID: "west", Limit: 10})
	if byServer.Total != 1 || len(byServer.Events) != 1 || byServer.Events[0].ID != "b" {
		t.Fatalf("serverId filter = %+v", byServer)
	}
	byCategory := queryEvents(events, eventQuery{Category: "update", Limit: 10})
	if byCategory.Total != 1 || byCategory.Events[0].ID != "d" {
		t.Fatalf("category filter = %+v", byCategory)
	}
	byText := queryEvents(events, eventQuery{Text: "unhealthy", Limit: 10})
	if byText.Total != 1 || byText.Events[0].ID != "c" {
		t.Fatalf("query filter = %+v", byText)
	}
	page := queryEvents(events, eventQuery{ServerID: "east", Limit: 2, Offset: 1})
	if page.Total != 4 || len(page.Events) != 2 || page.Warnings != 1 || page.Critical != 2 {
		t.Fatalf("page stats should cover the full match, got %+v", page)
	}
	if got := eventIDs(page.Events); !slices.Equal(got, []string{"d", "c"}) {
		t.Fatalf("page ids = %v, want [d c]", got)
	}
}

func TestHandleEventsQueryAndSince(t *testing.T) {
	app := newTestApp(t, true)
	res := requestJSON(t, app, http.MethodGet, "/api/events", "")
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "Update available") {
		t.Fatalf("default GET = %d %s", res.Code, res.Body.String())
	}
	res = requestJSON(t, app, http.MethodGet, "/api/events?since=yesterday", "")
	if res.Code != http.StatusBadRequest {
		t.Fatalf("invalid since status = %d, body = %s", res.Code, res.Body.String())
	}
	res = requestJSON(t, app, http.MethodGet, "/api/events?since="+url.QueryEscape(time.Now().UTC().Add(-time.Hour).Format("2006-01-02T15:04:05.000Z"))+"&limit=1", "")
	if res.Code != http.StatusOK {
		t.Fatalf("fractional since status = %d, body = %s", res.Code, res.Body.String())
	}
	var firstPage eventList
	if err := json.Unmarshal(res.Body.Bytes(), &firstPage); err != nil {
		t.Fatal(err)
	}
	if firstPage.Total < 1 || len(firstPage.Events) != 1 {
		t.Fatalf("limit=1 page = %+v", firstPage)
	}
	old := time.Now().UTC().Add(-48 * time.Hour)
	if err := app.store.Update(func(state *State) error {
		addEvent(state, Event{ID: "ancient", Timestamp: old, Category: "system", Severity: "success", Message: "Ancient restart", ServerID: "scuffedtards", ServerName: "ScuffedTards"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	since := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339)
	res = requestJSON(t, app, http.MethodGet, "/api/events?since="+url.QueryEscape(since), "")
	if res.Code != http.StatusOK {
		t.Fatalf("since status = %d, body = %s", res.Code, res.Body.String())
	}
	var list eventList
	if err := json.Unmarshal(res.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if containsEventID(list.Events, "ancient") || strings.Contains(res.Body.String(), "Ancient restart") {
		t.Fatal("24h since included an old row")
	}
	if !strings.Contains(res.Body.String(), "Update available") {
		t.Fatal("24h since dropped demo update event")
	}
}

func TestNewStorePrunesOversizedEvents(t *testing.T) {
	app := newTestApp(t, false)
	now := time.Now().UTC()
	if err := app.store.Update(func(state *State) error {
		events := make([]Event, 0, 601)
		for i := 0; i < 600; i++ {
			events = append(events, Event{ID: fmt.Sprintf("join-%d", i), Timestamp: now.Add(time.Duration(i) * time.Second), Kind: PlayerJoined, Category: "player", Message: "join"})
		}
		events = append(events, Event{ID: "restart", Timestamp: now, Kind: RestartRequested, Category: "system", Message: "Restart requested"})
		state.Events = events
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewStore(app.store.path, false)
	if err != nil {
		t.Fatal(err)
	}
	s := reloaded.Snapshot()
	if !containsEventID(s.Events, "restart") {
		t.Fatal("boot prune dropped lifecycle restart")
	}
	if got := countEventClass(s.Events, eventRoutine); got != maxRoutineEvents {
		t.Fatalf("boot prune routine = %d, want %d", got, maxRoutineEvents)
	}
}

func eventIDs(events []Event) []string {
	ids := make([]string, len(events))
	for i, event := range events {
		ids[i] = event.ID
	}
	return ids
}

func eventIDSeqEqual(a, b []Event) bool {
	return slices.Equal(eventIDs(a), eventIDs(b))
}

func containsEventID(events []Event, id string) bool {
	return slices.Contains(eventIDs(events), id)
}

func countEventClass(events []Event, class eventClass) int {
	n := 0
	for _, event := range events {
		if classifyEvent(event) == class {
			n++
		}
	}
	return n
}
