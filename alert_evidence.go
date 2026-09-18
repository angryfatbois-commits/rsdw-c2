package main

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
)

type PlayerCountEvidence struct {
	Available  bool `json:"available"`
	Players    int  `json:"players"`
	MaxPlayers int  `json:"maxPlayers"`
}

type RestartWarningEvidence struct {
	Trigger   string    `json:"trigger"`
	RestartAt time.Time `json:"restartAt"`
	Minutes   int       `json:"minutes"`
}

type AlertEvidence struct {
	PlayerCount    *PlayerCountEvidence    `json:"playerCount,omitempty"`
	JoinedPlayers  []ConnectedPlayer       `json:"joinedPlayers,omitempty"`
	RestartWarning *RestartWarningEvidence `json:"restartWarning,omitempty"`
}

func (event Event) clone() Event {
	event.Evidence.JoinedPlayers = slices.Clone(event.Evidence.JoinedPlayers)
	if event.Evidence.PlayerCount != nil {
		count := *event.Evidence.PlayerCount
		event.Evidence.PlayerCount = &count
	}
	if event.Evidence.RestartWarning != nil {
		warning := *event.Evidence.RestartWarning
		event.Evidence.RestartWarning = &warning
	}
	return event
}

func countEvidence(server Server, o observation, now time.Time) *PlayerCountEvidence {
	count := &PlayerCountEvidence{MaxPlayers: server.MaxPlayers}
	reading := freshMetrics(o.metrics, now)["players"]
	if reading.Value != nil && reading.Status == "available" {
		count.Available, count.Players = true, int(*reading.Value)
	}
	return count
}

func safePlayerName(name string) bool {
	return strings.TrimSpace(name) == name && len(name) <= 128 && !strings.ContainsAny(name, "@<>`*_~|[]\\") && !strings.ContainsFunc(name, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) })
}

func rosterEvidence(roster PlayerRoster, count int) []ConnectedPlayer {
	if roster.Status != RosterAvailable || len(roster.Players) == 0 || len(roster.Players) != count {
		return nil
	}
	seen := map[string]bool{}
	players := map[string]bool{}
	for _, player := range roster.Players {
		if player.CharacterName == "" || !safePlayerName(player.CharacterName) || !safePlayerName(player.Name) || seen[player.CharacterName] || (player.Name != "" && players[player.Name]) {
			return nil
		}
		seen[player.CharacterName] = true
		players[player.Name] = true
	}
	return slices.Clone(roster.Players)
}

func joinedRoster(previous, current []ConnectedPlayer, increase int) []ConnectedPlayer {
	if len(previous) == 0 || len(current) == 0 {
		return nil
	}
	for _, player := range previous {
		if !slices.Contains(current, player) {
			return nil
		}
	}
	var joined []ConnectedPlayer
	for _, player := range current {
		if !slices.Contains(previous, player) {
			joined = append(joined, player)
		}
	}
	if len(joined) != increase || len(joined) > 4 {
		return nil
	}
	return joined
}

func playerCountText(count *PlayerCountEvidence) string {
	if count.Available {
		return fmt.Sprintf("%d/%d", count.Players, count.MaxPlayers)
	}
	return fmt.Sprintf("unavailable/%d", count.MaxPlayers)
}
