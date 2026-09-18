package main

import (
	"errors"
	"net"
	"net/url"
	"strings"
	"time"
)

type integrationProvider string

const (
	providerDiscord integrationProvider = "discord"
	providerWebhook integrationProvider = "webhook"
)

func normalizedProvider(provider integrationProvider) integrationProvider {
	if provider == "" {
		return providerDiscord
	}
	return provider
}

type notificationTarget struct {
	Provider   integrationProvider
	GuildID    string
	ChannelID  string
	WebhookURL string
}

func (i DiscordIntegration) target() notificationTarget {
	return notificationTarget{normalizedProvider(i.Provider), i.GuildID, i.ChannelID, i.WebhookURL}
}

func (d Delivery) target() notificationTarget {
	return notificationTarget{normalizedProvider(d.Provider), d.GuildID, d.ChannelID, d.WebhookURL}
}

func (t notificationTarget) validate() error {
	switch t.Provider {
	case providerDiscord:
		if t.WebhookURL != "" || !discordIDPattern.MatchString(t.GuildID) || !discordIDPattern.MatchString(t.ChannelID) {
			return errors.New("Discord requires numeric guildId and channelId and no webhookUrl")
		}
	case providerWebhook:
		if t.GuildID != "" || t.ChannelID != "" {
			return errors.New("webhook destinations must not include guildId or channelId")
		}
		u, err := url.Parse(t.WebhookURL)
		if err != nil || len(t.WebhookURL) > 2048 || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" || u.Opaque != "" || (u.Port() != "" && u.Port() != "443") {
			return errors.New("webhookUrl must be a public HTTPS URL on port 443 without credentials, query, or fragment; use secretRef for authentication")
		}
		host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
		if host == "localhost" || !strings.Contains(host, ".") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") || (net.ParseIP(host) != nil && !publicWebhookIP(net.ParseIP(host))) {
			return errors.New("webhookUrl must use a public Internet destination")
		}
	default:
		return errors.New("provider must be discord or webhook")
	}
	return nil
}

type QuietHours struct {
	Start    string `json:"start"`
	End      string `json:"end"`
	Timezone string `json:"timezone"`
}

func integrationErrorFields(err error) map[string]string {
	message := err.Error()
	fields := []string{"form"}
	switch {
	case strings.HasPrefix(message, "name "):
		fields = []string{"name"}
	case strings.Contains(message, "quietHours timezone"):
		fields = []string{"quietTimezone"}
	case strings.Contains(message, "quietHours"):
		fields = []string{"quietStart", "quietEnd"}
	case strings.Contains(message, "webhookUrl"):
		fields = []string{"webhookUrl"}
	case strings.Contains(message, "Discord requires"):
		fields = []string{"guildId", "channelId"}
	case strings.Contains(message, "secretRef"):
		fields = []string{"secretName", "secretKey"}
	case strings.Contains(message, "provider"):
		fields = []string{"provider"}
	}
	result := map[string]string{}
	for _, field := range fields {
		result[field] = message
	}
	return result
}

func (q QuietHours) validate() error {
	start, e1 := time.Parse("15:04", q.Start)
	end, e2 := time.Parse("15:04", q.End)
	if e1 != nil || e2 != nil || start.Format("15:04") != q.Start || end.Format("15:04") != q.End || q.Start == q.End {
		return errors.New("quietHours start and end must be distinct HH:mm times")
	}
	if _, err := time.LoadLocation(q.Timezone); err != nil || q.Timezone == "" || q.Timezone == "Local" {
		return errors.New("quietHours timezone must be an IANA timezone")
	}
	return nil
}

func (q *QuietHours) contains(at time.Time) bool {
	if q == nil {
		return false
	}
	loc, err := time.LoadLocation(q.Timezone)
	if err != nil {
		return true
	}
	clock := at.In(loc).Format("15:04")
	if q.Start < q.End {
		return clock >= q.Start && clock < q.End
	}
	return clock >= q.Start || clock < q.End
}
