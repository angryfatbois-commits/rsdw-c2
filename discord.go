package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const deliveryMaxAttempts = 5

func (k *kubeOrchestrator) discordToken(ctx context.Context, ref SecretReference) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	namespace := envOr("RSDW_NAMESPACE", "rsdw-system")
	data, err := k.runner.Run(ctx, k.kubectl, "-n", namespace, "get", "secret", ref.Name, "-o", "json")
	if err != nil {
		return "", errors.New("Discord Secret could not be read")
	}
	var secret struct {
		Data map[string][]byte `json:"data"`
	}
	if json.Unmarshal(data, &secret) != nil {
		return "", errors.New("Discord Secret is invalid")
	}
	token := string(secret.Data[ref.Key])
	if len(token) == 0 || len(token) > 4096 || strings.ContainsAny(token, " \t\r\n\x00") {
		return "", errors.New("Discord Secret key is missing or invalid")
	}
	return token, nil
}

func (k *kubeOrchestrator) s3Credentials(ctx context.Context, ref SecretReference) (accessKeyID, secretAccessKey string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	namespace := envOr("RSDW_NAMESPACE", "rsdw-system")
	data, err := k.runner.Run(ctx, k.kubectl, "-n", namespace, "get", "secret", ref.Name, "-o", "json")
	if err != nil {
		return "", "", errors.New("S3 Secret could not be read")
	}
	var secret struct {
		Data map[string][]byte `json:"data"`
	}
	if json.Unmarshal(data, &secret) != nil {
		return "", "", errors.New("S3 Secret is invalid")
	}
	valid := func(value []byte) bool {
		return len(value) > 0 && len(value) <= 4096 && !strings.ContainsAny(string(value), " \t\r\n\x00")
	}
	accessKeyID = string(secret.Data["accessKeyId"])
	secretAccessKey = string(secret.Data["secretAccessKey"])
	if !valid(secret.Data["accessKeyId"]) || !valid(secret.Data["secretAccessKey"]) {
		return "", "", errors.New("S3 Secret must contain accessKeyId and secretAccessKey keys")
	}
	return accessKeyID, secretAccessKey, nil
}

type discordResult struct {
	status DeliveryStatus
	reason string
	wait   time.Duration
}

func rateWait(response *http.Response) time.Duration {
	var seconds float64
	for _, key := range []string{"Retry-After", "X-RateLimit-Reset-After"} {
		if value, err := strconv.ParseFloat(response.Header.Get(key), 64); err == nil && !math.IsNaN(value) && !math.IsInf(value, 0) {
			seconds = math.Max(seconds, value)
		}
	}
	if response.StatusCode == http.StatusTooManyRequests {
		var body struct {
			RetryAfter float64 `json:"retry_after"`
		}
		if json.NewDecoder(io.LimitReader(response.Body, 8192)).Decode(&body) == nil {
			seconds = math.Max(seconds, body.RetryAfter)
		}
		seconds = math.Max(seconds, 1)
	}
	if seconds > float64((365*24*time.Hour)/time.Second) {
		return 365 * 24 * time.Hour
	}
	return time.Duration(math.Ceil(seconds*1000)) * time.Millisecond
}

func (a *App) sendDiscord(ctx context.Context, i DiscordIntegration, d Delivery) discordResult {
	if a.demo {
		return discordResult{status: DeliverySent, reason: "Demo simulated delivery; no Discord request"}
	}
	message, err := buildDiscordMessage(d)
	if err != nil {
		return discordResult{status: DeliveryFailed, reason: err.Error()}
	}
	k, ok := a.orchestrator.(*kubeOrchestrator)
	if !ok {
		return discordResult{status: DeliveryFailed, reason: "Discord requires Kubernetes Secret access"}
	}
	token, err := k.discordToken(ctx, i.SecretRef)
	if err != nil {
		return discordResult{status: DeliveryRetry, reason: "Discord Secret unavailable"}
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: a.discordTransport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	channelURL := "https://discord.com/api/v10/channels/" + d.ChannelID
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, channelURL, nil)
	request.Header.Set("Authorization", "Bot "+token)
	response, err := client.Do(request)
	if err != nil {
		return discordResult{status: DeliveryRetry, reason: "Discord channel verification unavailable"}
	}
	wait := rateWait(response)
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		if response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500 {
			return discordResult{status: DeliveryRetry, reason: "Discord channel verification deferred", wait: wait}
		}
		return discordResult{status: DeliveryFailed, reason: "Discord channel verification rejected"}
	}
	var channel struct {
		ID      string `json:"id"`
		GuildID string `json:"guild_id"`
	}
	err = json.NewDecoder(io.LimitReader(response.Body, 8192)).Decode(&channel)
	response.Body.Close()
	if err != nil || channel.ID != d.ChannelID || channel.GuildID != d.GuildID {
		return discordResult{status: DeliveryFailed, reason: "Discord channel does not match the configured guild"}
	}
	data, _ := json.Marshal(message)
	request, _ = http.NewRequestWithContext(ctx, http.MethodPost, channelURL+"/messages", bytes.NewReader(data))
	request.Header.Set("Authorization", "Bot "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err = client.Do(request)
	if err != nil {
		return discordResult{status: DeliveryUncertain, reason: "Discord send outcome is unknown; no automatic retry"}
	}
	defer response.Body.Close()
	wait = rateWait(response)
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return discordResult{status: DeliverySent, reason: "Discord accepted the message", wait: wait}
	}
	if response.StatusCode == http.StatusTooManyRequests {
		return discordResult{status: DeliveryRetry, reason: "Discord rate limited the message", wait: wait}
	}
	if response.StatusCode >= 500 || response.StatusCode == http.StatusRequestTimeout {
		return discordResult{status: DeliveryUncertain, reason: "Discord send outcome is unknown; no automatic retry", wait: wait}
	}
	return discordResult{status: DeliveryFailed, reason: "Discord rejected the message"}
}

func (a *App) processDeliveries(ctx context.Context, now time.Time) error {
	if !a.deliveryMu.TryLock() {
		return nil
	}
	defer a.deliveryMu.Unlock()
	snapshot := a.store.Snapshot()
	for id, d := range snapshot.Deliveries {
		if d.Status == DeliverySending {
			if err := a.store.Update(func(state *State) error {
				current, ok := state.Deliveries[id]
				if !ok || current.Status != DeliverySending {
					return nil
				}
				d = current
				d.Status, d.Result = DeliveryUncertain, "Delivery result was not persisted; message may have been sent. No automatic retry."
				state.Deliveries[id] = d
				return nil
			}); err != nil {
				return errors.New("could not persist uncertain delivery")
			}
		}
	}
	for id, d := range snapshot.Deliveries {
		if d.Status != DeliveryPending && d.Status != DeliveryRetry || d.NextAttempt.After(now) {
			continue
		}
		claimNow := time.Now().UTC()
		if claimNow.Before(now) {
			claimNow = now
		}
		var integration DiscordIntegration
		claimed := false
		scheduledWarning := isScheduledRestartWarning(d.Event)
		err := a.store.Update(func(state *State) error {
			d = state.Deliveries[id]
			if (d.Status != DeliveryPending && d.Status != DeliveryRetry) || d.NextAttempt.After(claimNow) {
				return nil
			}
			var ok bool
			integration, ok = state.Integrations[d.IntegrationID]
			if !ok || state.deleting(d.Event.ServerID) || !deliveryEnabled(integration, d) || !warningDeliveryValid(state, d, claimNow) || (scheduledWarning && !a.rebootSchedulingEnabled()) {
				d.Status, d.Result, d.UpdatedAt = DeliveryFailed, "Integration no longer enables this delivery", claimNow
				state.Deliveries[id] = d
				state.pruneDeliveryHistory()
				return nil
			}
			if normalizedProvider(d.Provider) == providerDiscord && state.DiscordRetryAt.After(claimNow) {
				return nil
			}
			d.Status, d.UpdatedAt = DeliverySending, claimNow
			d.Attempts++
			state.Deliveries[id] = d
			claimed = true
			return nil
		})
		if err != nil {
			return errors.New("could not persist delivery claim")
		}
		if !claimed {
			continue
		}
		result := a.sendNotification(ctx, integration, d)
		finished := time.Now().UTC()
		if finished.Before(claimNow) {
			finished = claimNow
		}
		if err := a.store.Update(func(state *State) error {
			current, ok := state.Deliveries[id]
			if !ok || current.Status != DeliverySending || current.Attempts != d.Attempts {
				return nil
			}
			d.Status, d.Result, d.UpdatedAt = result.status, result.reason, finished
			if result.wait > 0 && normalizedProvider(d.Provider) == providerDiscord {
				state.DiscordRetryAt = finished.Add(result.wait)
			}
			if d.Status == DeliveryRetry {
				if d.Attempts >= deliveryMaxAttempts {
					d.Status, d.Result = DeliveryFailed, "Delivery retry limit reached"
				} else {
					delay := time.Duration(1<<d.Attempts) * time.Second
					if result.wait > delay {
						delay = result.wait
					}
					d.NextAttempt = finished.Add(delay)
				}
			}
			state.Deliveries[id] = d
			state.pruneDeliveryHistory()
			return nil
		}); err != nil {
			return errors.New("could not persist delivery result; sending delivery requires reconciliation")
		}
		if ctx.Err() != nil {
			return nil
		}
	}
	return nil
}

func (a *App) runDeliveries(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := a.processDeliveries(ctx, time.Now().UTC()); err != nil {
			log.Print(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
