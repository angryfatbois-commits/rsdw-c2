package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"time"
)

func publicWebhookIP(ip net.IP) bool {
	return ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsUnspecified()
}

func webhookHTTPTransport() *http.Transport {
	return &http.Transport{
		TLSHandshakeTimeout:    5 * time.Second,
		ResponseHeaderTimeout:  5 * time.Second,
		MaxResponseHeaderBytes: 16 << 10,
		DisableKeepAlives:      true,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil || len(ips) == 0 {
				return nil, errors.New("webhook destination unavailable")
			}
			for _, ip := range ips {
				if !publicWebhookIP(ip.IP) {
					return nil, errors.New("webhook destination is not public")
				}
			}
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
		},
	}
}

func (a *App) sendWebhook(ctx context.Context, i DiscordIntegration, d Delivery) discordResult {
	if err := d.target().validate(); err != nil {
		return discordResult{status: DeliveryFailed, reason: "Invalid webhook destination"}
	}
	if a.demo {
		return discordResult{status: DeliverySent, reason: "Demo simulated delivery; no webhook request"}
	}
	k, ok := a.orchestrator.(*kubeOrchestrator)
	if !ok {
		return discordResult{status: DeliveryFailed, reason: "Webhook requires Kubernetes Secret access"}
	}
	token, err := k.discordToken(ctx, i.SecretRef)
	if err != nil {
		return discordResult{status: DeliveryRetry, reason: "Webhook Secret unavailable"}
	}
	data, err := json.Marshal(struct {
		Version    int    `json:"version"`
		DeliveryID string `json:"deliveryId"`
		Event      Event  `json:"event"`
	}{1, d.ID, d.Event})
	if err != nil || len(data) > 64<<10 {
		return discordResult{status: DeliveryFailed, reason: "Webhook message exceeds the supported size"}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, d.WebhookURL, bytes.NewReader(data))
	if err != nil {
		return discordResult{status: DeliveryFailed, reason: "Invalid webhook destination"}
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", d.ID)
	transport := a.webhookTransport
	if transport == nil {
		transport = webhookHTTPTransport()
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return discordResult{status: DeliveryUncertain, reason: "Webhook send outcome is unknown; no automatic retry"}
	}
	defer response.Body.Close()
	switch {
	case response.StatusCode >= 200 && response.StatusCode < 300:
		return discordResult{status: DeliverySent, reason: "Webhook accepted message"}
	case response.StatusCode == http.StatusTooManyRequests:
		return discordResult{status: DeliveryRetry, reason: "Webhook rate limited", wait: rateWait(response)}
	case response.StatusCode >= 500 || response.StatusCode == http.StatusRequestTimeout:
		return discordResult{status: DeliveryUncertain, reason: "Webhook send outcome is unknown; no automatic retry"}
	default:
		return discordResult{status: DeliveryFailed, reason: "Webhook rejected message"}
	}
}

func (a *App) sendNotification(ctx context.Context, i DiscordIntegration, d Delivery) discordResult {
	switch normalizedProvider(d.Provider) {
	case providerDiscord:
		return a.sendDiscord(ctx, i, d)
	case providerWebhook:
		return a.sendWebhook(ctx, i, d)
	default:
		return discordResult{status: DeliveryFailed, reason: "Unknown notification provider"}
	}
}
