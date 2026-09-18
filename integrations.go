package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

type EventKind string

const (
	RestartRequested      EventKind = "restart_requested"
	RestartCompleted      EventKind = "restart_completed"
	RestartFailed         EventKind = "restart_failed"
	PlayerJoined          EventKind = "player_joined"
	PlayerLimitReached    EventKind = "player_limit_reached"
	ServerDown            EventKind = "server_down"
	ServerRecovered       EventKind = "server_recovered"
	BackupStarted         EventKind = "backup_started"
	BackupCompleted       EventKind = "backup_completed"
	BackupFailed          EventKind = "backup_failed"
	IntegrationTest       EventKind = "integration_test"
	RebootScheduleChanged EventKind = "reboot_schedule_changed"
	ServerStopped         EventKind = "server_stopped"
	ServerStarted         EventKind = "server_started"
)

type AlertRule struct {
	Kind      EventKind `json:"kind"`
	Label     string    `json:"label"`
	Available bool      `json:"available"`
	Source    string    `json:"source"`
	Accuracy  string    `json:"accuracy"`
}

var alertRules = []AlertRule{
	{RestartRequested, "Restart requested", true, "C2 restart operation", "observed"},
	{RestartCompleted, "Restart completed", true, "Owned replacement runtime and fresh engine readiness", "observed"},
	{RestartFailed, "Restart failed", true, "Restart deadline and fresh runtime observation", "observed"},
	{PlayerJoined, "Player joined (approximate count increase)", true, "Fresh game API player counts on the same runtime", "approximate"},
	{PlayerLimitReached, "Player limit reached", true, "Fresh game API count crossing the configured limit", "observed"},
	{ServerDown, "Server down", true, "Three definitive unhealthy observations over at least 30 seconds", "observed"},
	{ServerRecovered, "Server recovered", true, "Two healthy observations over at least 15 seconds after an outage", "observed"},
	{ServerStopped, "Server stopped", true, "C2 operator parked the world without deleting inventory or volumes", "observed"},
	{ServerStarted, "Server started", true, "C2 operator started a parked world", "observed"},
	{BackupStarted, "Backup started (unavailable until a backup producer exists)", false, "No backup producer", "unavailable"},
	{BackupCompleted, "Backup completed (unavailable until a backup producer exists)", false, "No backup producer", "unavailable"},
	{BackupFailed, "Backup failed (unavailable until a backup producer exists)", false, "No backup producer", "unavailable"},
}

type SecretReference struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

type DiscordIntegration struct {
	ID        string             `json:"id"`
	Name      string             `json:"name"`
	Enabled   bool               `json:"enabled"`
	GuildID   string             `json:"guildId"`
	ChannelID string             `json:"channelId"`
	SecretRef SecretReference    `json:"secretRef"`
	ServerIDs []string           `json:"serverIds"`
	Rules     map[EventKind]bool `json:"rules"`
}

type DeliveryStatus string

const (
	DeliveryPending   DeliveryStatus = "pending"
	DeliverySending   DeliveryStatus = "sending"
	DeliverySent      DeliveryStatus = "sent"
	DeliveryRetry     DeliveryStatus = "retry"
	DeliveryFailed    DeliveryStatus = "failed"
	DeliveryUncertain DeliveryStatus = "uncertain"
)

const terminalDeliveryHistoryLimit = 100

type Delivery struct {
	ID            string         `json:"id"`
	IntegrationID string         `json:"integrationId"`
	Event         Event          `json:"event"`
	GuildID       string         `json:"guildId"`
	ChannelID     string         `json:"channelId"`
	Status        DeliveryStatus `json:"status"`
	Attempts      int            `json:"attempts"`
	NextAttempt   time.Time      `json:"nextAttempt"`
	UpdatedAt     time.Time      `json:"updatedAt"`
	Result        string         `json:"result"`
}

func (s *State) initIntegrations() {
	if s.Integrations == nil {
		s.Integrations = map[string]DiscordIntegration{}
	}
	if s.Producers == nil {
		s.Producers = map[string]AlertProducer{}
	}
	if s.Deliveries == nil {
		s.Deliveries = map[string]Delivery{}
	}
	for id, i := range s.Integrations {
		if i.ServerIDs == nil {
			i.ServerIDs = []string{}
		}
		if i.Rules == nil {
			i.Rules = map[EventKind]bool{}
		}
		s.Integrations[id] = i
	}
}

func (s *State) recoverAlerts() {
	for id, d := range s.Deliveries {
		if d.Status == DeliverySending {
			d.Status, d.Result = DeliveryUncertain, "C2 stopped during delivery; message may have been sent. No automatic retry."
			s.Deliveries[id] = d
		}
	}
	for id, p := range s.Producers {
		p.Runtime, p.PlayerAt, p.LastAt = "", time.Time{}, time.Time{}
		p.resetStreak()
		s.Producers[id] = p
	}
	s.pruneDeliveryHistory()
}

func (s *State) pruneDeliveryHistory() {
	if len(s.Deliveries) <= terminalDeliveryHistoryLimit {
		return
	}
	var terminal []Delivery
	for _, d := range s.Deliveries {
		if d.Status == DeliverySent || d.Status == DeliveryFailed {
			terminal = append(terminal, d)
		}
	}
	if len(terminal) <= terminalDeliveryHistoryLimit {
		return
	}
	sort.Slice(terminal, func(i, j int) bool {
		if terminal[i].UpdatedAt.Equal(terminal[j].UpdatedAt) {
			return terminal[i].ID < terminal[j].ID
		}
		return terminal[i].UpdatedAt.After(terminal[j].UpdatedAt)
	})
	for _, d := range terminal[terminalDeliveryHistoryLimit:] {
		delete(s.Deliveries, d.ID)
	}
}

func queueDeliveryForNewEvent(state *State, integration DiscordIntegration, event Event) Delivery {
	sum := sha256.Sum256([]byte(event.ID + "/" + integration.ID))
	id := hex.EncodeToString(sum[:12])
	if existing, ok := state.Deliveries[id]; ok {
		return existing
	}
	d := Delivery{ID: id, IntegrationID: integration.ID, Event: event, GuildID: integration.GuildID, ChannelID: integration.ChannelID, Status: DeliveryPending, UpdatedAt: event.Timestamp}
	state.Deliveries[id] = d
	return d
}

func emitAlert(state *State, server Server, kind EventKind, operation, details string, at time.Time) Event {
	var rule AlertRule
	for _, candidate := range alertRules {
		if candidate.Kind == kind {
			rule = candidate
			break
		}
	}
	if !rule.Available {
		return Event{}
	}
	category, severity := "system", "success"
	switch kind {
	case PlayerJoined, PlayerLimitReached:
		category = "player"
	case ServerDown, ServerRecovered:
		category = "health"
	}
	if kind == RestartRequested || kind == PlayerLimitReached || kind == ServerStopped {
		severity = "warning"
	}
	if kind == RestartFailed || kind == ServerDown {
		severity = "error"
	}
	event := Event{ID: randomID(), Timestamp: at, ServerID: server.ID, ServerName: worldLabel(server), Category: category, Severity: severity, Message: rule.Label, Details: details, Kind: kind, Source: rule.Source, Accuracy: rule.Accuracy, OperationID: operation}
	addEvent(state, event)
	for _, integration := range state.Integrations {
		if integration.Enabled && integration.Rules[kind] && slices.Contains(integration.ServerIDs, server.ID) {
			queueDeliveryForNewEvent(state, integration, event)
		}
	}
	return event
}

var discordIDPattern = regexp.MustCompile(`^[0-9]{1,20}$`)
var secretNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
var secretKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,253}$`)

func validateIntegration(i DiscordIntegration, state State) error {
	if strings.TrimSpace(i.Name) == "" || len(i.Name) > 80 || strings.ContainsAny(i.Name, "\x00\r\n") {
		return errors.New("name must be a single line of 1 to 80 characters")
	}
	if !discordIDPattern.MatchString(i.GuildID) || !discordIDPattern.MatchString(i.ChannelID) {
		return errors.New("guildId and channelId must be Discord numeric IDs")
	}
	if len(i.SecretRef.Name) > 253 || !secretNamePattern.MatchString(i.SecretRef.Name) || !secretKeyPattern.MatchString(i.SecretRef.Key) {
		return errors.New("secretRef must contain a Kubernetes Secret name and key, never a token")
	}
	if len(i.ServerIDs) > len(state.Servers) {
		return errors.New("invalid server associations")
	}
	seen := map[string]bool{}
	for _, id := range i.ServerIDs {
		if _, ok := state.Servers[id]; !ok || state.deleting(id) || seen[id] {
			return errors.New("server associations must be unique existing server IDs")
		}
		seen[id] = true
	}
	for kind, enabled := range i.Rules {
		found := false
		for _, rule := range alertRules {
			if rule.Kind == kind {
				found = true
				if enabled && !rule.Available {
					return errors.New("backup rules are unavailable until a backup producer exists")
				}
			}
		}
		if !found {
			return errors.New("unknown alert rule")
		}
	}
	return nil
}

func (a *App) handleIntegrations(w http.ResponseWriter, r *http.Request) {
	if requestPrincipal(r).Role != RoleAdmin {
		writeError(w, http.StatusForbidden, "permission denied")
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/integrations"), "/")
	collection := len(parts) == 1 && parts[0] == ""
	if collection && r.Method == http.MethodGet {
		snapshot := a.store.Snapshot()
		integrations, deliveries := []DiscordIntegration{}, []Delivery{}
		restarts := map[string]RestartOperation{}
		for id, p := range snapshot.Producers {
			if p.Restart != nil {
				restarts[id] = *p.Restart
			}
		}
		for _, i := range snapshot.Integrations {
			integrations = append(integrations, i)
		}
		for _, d := range snapshot.Deliveries {
			deliveries = append(deliveries, d)
		}
		sort.Slice(integrations, func(i, j int) bool { return integrations[i].ID < integrations[j].ID })
		sort.Slice(deliveries, func(i, j int) bool { return deliveries[i].UpdatedAt.After(deliveries[j].UpdatedAt) })
		if len(deliveries) > 100 {
			deliveries = deliveries[:100]
		}
		writeJSON(w, http.StatusOK, map[string]any{"integrations": integrations, "deliveries": discordDeliveryViews(deliveries), "rules": alertRules, "pendingRestarts": restarts, "demo": a.demo})
		return
	}
	if len(parts) == 3 && parts[1] != "" && parts[2] == "test" && r.Method == http.MethodPost {
		var delivery Delivery
		status := http.StatusInternalServerError
		err := a.store.Update(func(state *State) error {
			i, ok := state.Integrations[parts[1]]
			if !ok {
				status = http.StatusNotFound
				return errors.New("integration not found")
			}
			event := Event{ID: randomID(), Timestamp: time.Now().UTC(), Kind: IntegrationTest, Message: "Discord integration test", Source: "C2 operator", Accuracy: "observed"}
			delivery = queueDeliveryForNewEvent(state, i, event)
			return nil
		})
		if err != nil {
			if status == http.StatusInternalServerError {
				writeError(w, status, "could not persist test delivery")
			} else {
				writeError(w, status, err.Error())
			}
			return
		}
		writeJSON(w, http.StatusAccepted, delivery)
		return
	}
	if !(collection && r.Method == http.MethodPost) && !(len(parts) == 2 && parts[1] != "" && r.Method == http.MethodPut) {
		writeError(w, http.StatusMethodNotAllowed, "use GET or POST on integrations, PUT on an integration, or POST on its test route")
		return
	}
	var input DiscordIntegration
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid integration JSON; use only a Secret reference")
		return
	}
	if decoder.Decode(new(any)) != io.EOF || input.ID != "" {
		writeError(w, http.StatusBadRequest, "invalid integration JSON")
		return
	}
	input.ID = randomID()
	if !collection {
		input.ID = parts[1]
	}
	if input.Rules == nil {
		input.Rules = map[EventKind]bool{}
	}
	if input.ServerIDs == nil {
		input.ServerIDs = []string{}
	}
	status := http.StatusInternalServerError
	err := a.store.Update(func(state *State) error {
		if !collection {
			if _, ok := state.Integrations[input.ID]; !ok {
				status = http.StatusNotFound
				return errors.New("integration not found")
			}
		}
		if err := validateIntegration(input, *state); err != nil {
			status = http.StatusBadRequest
			return err
		}
		state.Integrations[input.ID] = input
		for id, d := range state.Deliveries {
			if d.IntegrationID == input.ID && (d.Status == DeliveryPending || d.Status == DeliveryRetry) && !deliveryEnabled(input, d) {
				d.Status, d.Result, d.UpdatedAt = DeliveryFailed, "Cancelled by integration configuration change", time.Now().UTC()
				state.Deliveries[id] = d
			}
		}
		state.pruneDeliveryHistory()
		return nil
	})
	if err != nil {
		if status == http.StatusInternalServerError {
			writeError(w, status, "could not persist integration")
		} else {
			writeError(w, status, err.Error())
		}
		return
	}
	status = http.StatusOK
	if collection {
		status = http.StatusCreated
	}
	writeJSON(w, status, input)
}

func deliveryEnabled(i DiscordIntegration, d Delivery) bool {
	return d.GuildID == i.GuildID && d.ChannelID == i.ChannelID && (d.Event.Kind == IntegrationTest || (i.Enabled && i.Rules[d.Event.Kind] && slices.Contains(i.ServerIDs, d.Event.ServerID)))
}
