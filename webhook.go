package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"time"
)

var nonPublicWebhookPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("2001:1::/48"),
	netip.MustParsePrefix("2001:2::/48"),
	netip.MustParsePrefix("2001:10::/28"),
	netip.MustParsePrefix("2001:20::/28"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

func publicWebhookIP(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	if !addr.IsValid() || !addr.IsGlobalUnicast() || addr.IsPrivate() {
		return false
	}
	for _, prefix := range nonPublicWebhookPrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
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
