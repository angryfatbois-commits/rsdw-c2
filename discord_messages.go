package main

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

type discordEmbed struct {
	Title       string              `json:"title"`
	Description string              `json:"description"`
	Color       int                 `json:"color"`
	Fields      []discordEmbedField `json:"fields,omitempty"`
	Footer      discordEmbedFooter  `json:"footer"`
	Timestamp   string              `json:"timestamp,omitempty"`
}

type discordEmbedField struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type discordEmbedFooter struct {
	Text string `json:"text"`
}

type discordAllowedMentions struct {
	Parse []string `json:"parse"`
}

type discordMessage struct {
	Embeds          []discordEmbed         `json:"embeds"`
	Nonce           string                 `json:"nonce"`
	EnforceNonce    bool                   `json:"enforce_nonce"`
	AllowedMentions discordAllowedMentions `json:"allowed_mentions"`
}

type discordMessageSpec struct {
	Kind        EventKind
	Title       string
	Description string
	Color       int
	Server      bool
}

var discordMessageSpecs = []discordMessageSpec{
	{MemoryPressureRestartRequested, "Memory pressure restart requested", "The server kept hogging memory. C2 requested a restart before it ate the whole damn limit.", 0xF59E0B, true},
	{RestartWarning, "Restart approaching", "A scheduled restart is approaching. Save your progress.", 0xF59E0B, true},
	{RestartRequested, "Restart requested", "The server was told to get its act together. It chose a dramatic reboot instead.", 0xF59E0B, true},
	{RestartCompleted, "Restart completed", "The corpse has staggered back online. It did the bare minimum and expects a fucking parade.", 0x22C55E, true},
	{RestartFailed, "Restart not confirmed", "The restart fucked off into the void and never bothered to report back. Even the server can't explain what the hell it's doing.", 0xEF4444, true},
	{PlayerJoined, "Player count increased", "Another idiot crawled into this rancid little server. The population is up; the average brain-cell count remains terminal.", 0x3B82F6, true},
	{PlayerLimitReached, "Player limit reached", "It's full. Stop cramming people into this overcrowded shit-casket before it bursts and sprays everyone's problems across Discord.", 0xF59E0B, true},
	{ServerDown, "Server unhealthy", "The server has stopped pretending to be functional. How honest of it.", 0xEF4444, true},
	{ServerRecovered, "Server recovered", "It is back. Nobody knows why, and nobody should trust it.", 0x22C55E, true},
	{ServerStopped, "Server stopped", "The operator parked this world. The volume is still here. The players are not.", 0xF59E0B, true},
	{ServerStarted, "Server started", "Same world, same id, same disk. Try not to immediately fill it with tragedy.", 0x22C55E, true},
	{BackupStarted, "Backup started", "C2 is collecting the configured backup profile.", 0x3B82F6, true},
	{BackupCompleted, "Backup completed", "The backup bundle is stored and ready to download.", 0x22C55E, true},
	{BackupFailed, "Backup failed", "No completed backup was published. Review the failure in C2.", 0xEF4444, true},
	{IntegrationTest, "Discord integration test", "The bot successfully vomited into Discord and called it a test. The webhook works; civilization remains a mistake.", 0x8B5CF6, false},
}

var errUnsupportedDiscordMessage = errors.New("Discord message kind is unsupported or unavailable")

func renderDiscordEmbed(event Event) (discordEmbed, error) {
	for _, spec := range discordMessageSpecs {
		if spec.Kind != event.Kind {
			continue
		}
		embed := discordEmbed{Title: spec.Title, Description: spec.Description, Color: spec.Color, Footer: discordEmbedFooter{Text: "Dragonwilds C2"}}
		if spec.Server {
			name := event.ServerName
			if name == "" {
				name = "Unknown server"
			}
			embed.Fields = []discordEmbedField{{Name: "Server", Value: name}}
		}
		if !event.Timestamp.IsZero() {
			embed.Timestamp = event.Timestamp.UTC().Format(time.RFC3339Nano)
		}
		if count := event.Evidence.PlayerCount; count != nil {
			embed.Fields = append(embed.Fields, discordEmbedField{Name: "Players", Value: playerCountText(count)})
		}
		if len(event.Evidence.JoinedPlayers) > 0 {
			var names []string
			for _, player := range event.Evidence.JoinedPlayers {
				name := player.CharacterName
				if player.Name != "" {
					name += " (" + player.Name + ")"
				}
				names = append(names, name)
			}
			embed.Title, embed.Description = "Player joined", strings.Join(names, ", ")+" joined the server."
		}
		if warning := event.Evidence.RestartWarning; warning != nil {
			embed.Description = fmt.Sprintf("Restart in %d minutes. Save your progress. Scheduled for %s.", warning.Minutes, warning.RestartAt.UTC().Format(time.RFC3339))
		}
		return embed, nil
	}
	return discordEmbed{}, errUnsupportedDiscordMessage
}

func buildDiscordMessage(delivery Delivery) (discordMessage, error) {
	embed, err := renderDiscordEmbed(delivery.Event)
	if err != nil {
		return discordMessage{}, err
	}
	return discordMessage{Embeds: []discordEmbed{embed}, Nonce: delivery.ID, EnforceNonce: true, AllowedMentions: discordAllowedMentions{Parse: []string{}}}, nil
}

type discordDeliveryView struct {
	Delivery
	Embed *discordEmbed `json:"embed,omitempty"`
}

func discordDeliveryViews(deliveries []Delivery) []discordDeliveryView {
	views := make([]discordDeliveryView, 0, len(deliveries))
	for _, delivery := range deliveries {
		view := discordDeliveryView{Delivery: delivery}
		if normalizedProvider(delivery.Provider) == providerDiscord {
			if embed, err := renderDiscordEmbed(delivery.Event); err == nil {
				view.Embed = &embed
			}
		}
		views = append(views, view)
	}
	return views
}
