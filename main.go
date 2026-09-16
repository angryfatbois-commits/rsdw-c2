package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web
var webFiles embed.FS

var webRoot, _ = fs.Sub(webFiles, "web")

type Status string

const (
	StatusOnline    Status = "online"
	StatusStarting  Status = "starting"
	StatusAttention Status = "attention"
	StatusStopped   Status = "stopped"
	StatusUnknown   Status = "unknown"
)

type Server struct {
	ServerSettings
	Metrics               map[string]MetricReading `json:"metrics"`
	PasswordSecret        string                   `json:"passwordSecret,omitempty"`
	ServerPassword        string                   `json:"-"`
	AdminPassword         string                   `json:"-"`
	ID                    string                   `json:"id"`
	Name                  string                   `json:"name"`
	Namespace             string                   `json:"namespace"`
	Release               string                   `json:"release"`
	Region                string                   `json:"region"`
	OwnerID               string                   `json:"ownerId"`
	CurrentImage          string                   `json:"currentImage"`
	DesiredImage          string                   `json:"desiredImage"`
	Status                Status                   `json:"status"`
	Players               int                      `json:"players"`
	MaxPlayers            int                      `json:"maxPlayers"`
	MemoryLimitMiB        int                      `json:"memoryLimitMiB"`
	CPULimitMillis        int                      `json:"cpuLimitMillis"`
	TickRate              float64                  `json:"tickRate"`
	CPUPercent            float64                  `json:"cpuPercent"`
	MemoryUsedBytes       int64                    `json:"memoryUsedBytes"`
	MemoryLimitBytes      int64                    `json:"memoryLimitBytes"`
	DiskPercent           float64                  `json:"diskPercent"`
	NetworkBytesPerSecond int64                    `json:"networkBytesPerSecond"`
	UptimeSeconds         int64                    `json:"uptimeSeconds"`
	MetricsAvailable      bool                     `json:"metricsAvailable"`
	LastRestart           string                   `json:"lastRestart"`
	LastSeen              string                   `json:"lastSeen"`
	UpdateAvailable       bool                     `json:"updateAvailable"`
	Endpoint              string                   `json:"endpoint"`
}

func (s Server) MarshalJSON() ([]byte, error) {
	type serverJSON Server
	if s.Metrics == nil {
		return json.Marshal(serverJSON(s))
	}
	return json.Marshal(struct {
		serverJSON
		Players               *int     `json:"players,omitempty"`
		UptimeSeconds         *int64   `json:"uptimeSeconds,omitempty"`
		TickRate              *float64 `json:"tickRate,omitempty"`
		CPUPercent            *float64 `json:"cpuPercent,omitempty"`
		MemoryUsedBytes       *int64   `json:"memoryUsedBytes,omitempty"`
		MemoryLimitBytes      *int64   `json:"memoryLimitBytes,omitempty"`
		DiskPercent           *float64 `json:"diskPercent,omitempty"`
		NetworkBytesPerSecond *int64   `json:"networkBytesPerSecond,omitempty"`
	}{serverJSON: serverJSON(s)})
}

type Event struct {
	ID         string    `json:"id"`
	Timestamp  time.Time `json:"timestamp"`
	ServerID   string    `json:"serverId"`
	ServerName string    `json:"serverName"`
	Category   string    `json:"category"`
	Severity   string    `json:"severity"`
	Message    string    `json:"message"`
	Details    string    `json:"details"`
}

type State struct {
	Servers map[string]Server `json:"servers"`
	Events  []Event           `json:"events"`
}

type CreateServerRequest struct {
	ServerSettings
	ServerPassword string `json:"serverPassword"`
	AdminPassword  string `json:"adminPassword"`
	Name           string `json:"name"`
	Namespace      string `json:"namespace"`
	Region         string `json:"region"`
	OwnerID        string `json:"ownerId"`
	ImageTag       string `json:"imageTag"`
	MaxPlayers     int    `json:"maxPlayers"`
	MemoryLimitMiB int    `json:"memoryLimitMiB"`
	CPULimitMillis int    `json:"cpuLimitMillis"`
}

type ServerSettings struct {
	WorldName         string `json:"worldName"`
	GamePort          int    `json:"gamePort"`
	StorageGiB        int    `json:"storageGiB"`
	ServiceType       string `json:"serviceType"`
	AdminIDs          string `json:"adminIds"`
	AdditionalArgs    string `json:"additionalArgs"`
	DebugLevel        int    `json:"debugLevel"`
	AutoStopOnUpdate  bool   `json:"autoStopOnUpdate"`
	ValidateGameFiles bool   `json:"validateGameFiles"`
}

type UpdateServerRequest struct {
	ImageTag string `json:"imageTag"`
}

type Telemetry struct {
	Server            Server                   `json:"server"`
	Metrics           map[string]MetricReading `json:"metrics"`
	MetricsAvailable  bool                     `json:"metricsAvailable"`
	Samples           []MetricSample           `json:"samples"`
	HealthChecks      []HealthCheck            `json:"healthChecks"`
	MetricDefinitions []MetricDefinition       `json:"metricDefinitions"`
}

type MetricSample map[string]any

type HealthCheck struct {
	Name   string    `json:"name"`
	Status string    `json:"status"`
	At     time.Time `json:"at"`
}

type MetricDefinition struct {
	Metric      string `json:"metric"`
	Description string `json:"description"`
}

type LogLine struct {
	Timestamp time.Time `json:"timestamp"`
	Level     string    `json:"level"`
	Message   string    `json:"message"`
}

type Store struct {
	mu    sync.RWMutex
	path  string
	state State
}

func NewStore(path string, demo bool) (*Store, error) {
	s := &Store{path: path, state: State{Servers: map[string]Server{}}}
	if path != "" {
		if data, err := os.ReadFile(path); err == nil {
			if err := json.Unmarshal(data, &s.state); err != nil {
				return nil, fmt.Errorf("read state: %w", err)
			}
			if s.state.Servers == nil {
				s.state.Servers = map[string]Server{}
			}
			return s, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read state: %w", err)
		}
	}
	if demo {
		s.seedDemo()
	}
	return s, nil
}

func (s *Store) Snapshot() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	servers := make(map[string]Server, len(s.state.Servers))
	for id, server := range s.state.Servers {
		servers[id] = server
	}
	events := append([]Event(nil), s.state.Events...)
	return State{Servers: servers, Events: events}
}

func (s *Store) Update(fn func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := fn(&s.state); err != nil {
		return err
	}
	if s.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".state-*")
	if err != nil {
		return fmt.Errorf("create state temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write state temp file: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("protect state temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close state temp file: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("replace state: %w", err)
	}
	return nil
}

func (s *Store) seedDemo() {
	now := time.Now().UTC()
	server := Server{
		ID: "scuffedtards", Name: "ScuffedTards", Namespace: "dragonwilds", Release: "scuffedtards", Region: "eu-central", OwnerID: "demo-owner",
		CurrentImage: "ghcr.io/petzkod5/rsdragonwilds-server:0.1.1", DesiredImage: "ghcr.io/petzkod5/rsdragonwilds-server:0.1.2", Status: StatusOnline,
		Players: 12, MaxPlayers: 12, TickRate: 60, CPUPercent: 38, MemoryUsedBytes: 6_442_450_944, MemoryLimitBytes: 16_000_000_000, DiskPercent: 41,
		NetworkBytesPerSecond: 2_400_000, UptimeSeconds: 3*24*3600 + 14*3600 + 22*60, MetricsAvailable: true, LastRestart: now.Add(-3*24*time.Hour - 14*time.Hour).Format(time.RFC3339), LastSeen: now.Format(time.RFC3339), UpdateAvailable: true, Endpoint: "scuffedtards.dragonwilds.local:7777",
	}
	s.state.Servers[server.ID] = server
	for i, item := range []struct{ category, severity, message, details string }{
		{"player", "success", "Player connected", "Total players: 12 / 12"},
		{"system", "success", "World save committed", "Tick 482,913"},
		{"update", "warning", "Update available", "New image 0.1.2"},
		{"health", "success", "Health check passed", "All systems nominal"},
	} {
		s.state.Events = append(s.state.Events, Event{ID: fmt.Sprintf("demo-%d", i), Timestamp: now.Add(-time.Duration(i+1) * time.Minute), ServerID: server.ID, ServerName: server.Name, Category: item.category, Severity: item.severity, Message: item.message, Details: item.details})
	}
}

type CommandRunner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type shellRunner struct{}

func (shellRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		for _, arg := range args {
			if strings.HasPrefix(arg, "--from-literal=") {
				return nil, fmt.Errorf("%s: %w (Secret command details omitted)", name, err)
			}
		}
		return output, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

type Orchestrator interface {
	Deploy(context.Context, Server) error
	Restart(context.Context, Server) error
	Logs(context.Context, Server, int) ([]LogLine, error)
	Refresh(context.Context, Server) (Server, error)
	CheckUpdate(context.Context, Server) (Server, error)
}

type demoOrchestrator struct{}

func (demoOrchestrator) Deploy(_ context.Context, _ Server) error  { return nil }
func (demoOrchestrator) Restart(_ context.Context, _ Server) error { return nil }
func (demoOrchestrator) Logs(_ context.Context, server Server, tail int) ([]LogLine, error) {
	now := time.Now().UTC()
	lines := []LogLine{
		{Timestamp: now.Add(-18 * time.Second), Level: "INFO", Message: "health check passed"},
		{Timestamp: now.Add(-32 * time.Second), Level: "INFO", Message: fmt.Sprintf("players connected: %d/%d", server.Players, server.MaxPlayers)},
		{Timestamp: now.Add(-51 * time.Second), Level: "INFO", Message: "world save committed"},
		{Timestamp: now.Add(-77 * time.Second), Level: "INFO", Message: "server tick loop stable at 60 TPS"},
		{Timestamp: now.Add(-93 * time.Second), Level: "WARN", Message: "container image update available"},
	}
	if tail < len(lines) {
		lines = lines[len(lines)-tail:]
	}
	return lines, nil
}

func (demoOrchestrator) Refresh(_ context.Context, server Server) (Server, error) {
	server.LastSeen = time.Now().UTC().Format(time.RFC3339)
	return server, nil
}
func (demoOrchestrator) CheckUpdate(_ context.Context, server Server) (Server, error) {
	server.LastSeen = time.Now().UTC().Format(time.RFC3339)
	server.UpdateAvailable = server.DesiredImage != "" && server.CurrentImage != "" && server.DesiredImage != server.CurrentImage
	return server, nil
}

type kubeOrchestrator struct {
	runner          CommandRunner
	helm            string
	kubectl         string
	chart           string
	chartVersion    string
	imageRepository string
	gameAPIPort     string
}

func newKubeOrchestrator() *kubeOrchestrator {
	configureInClusterKubeconfig()
	return &kubeOrchestrator{
		runner: shellRunner{}, helm: envOr("RSDW_HELM_BIN", "helm"), kubectl: envOr("RSDW_KUBECTL_BIN", "kubectl"),
		chart: envOr("RSDW_CHART", "oci://ghcr.io/petzkod5/charts/rsdragonwilds"), chartVersion: envOr("RSDW_CHART_VERSION", "0.1.1"), imageRepository: envOr("RSDW_IMAGE_REPOSITORY", "ghcr.io/petzkod5/rsdragonwilds-server"), gameAPIPort: envOr("RSDW_GAME_API_PORT", "8080"),
	}
}

func configureInClusterKubeconfig() {
	if os.Getenv("KUBECONFIG") != "" || os.Getenv("KUBERNETES_SERVICE_HOST") == "" {
		return
	}
	tokenFile := envOr("RSDW_SERVICE_ACCOUNT_TOKEN_FILE", "/var/run/secrets/kubernetes.io/serviceaccount/token")
	caFile := envOr("RSDW_SERVICE_ACCOUNT_CA_FILE", "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
	if _, err := os.Stat(tokenFile); err != nil {
		log.Printf("in-cluster token is unavailable: %v", err)
		return
	}
	port := envOr("KUBERNETES_SERVICE_PORT_HTTPS", envOr("KUBERNETES_SERVICE_PORT", "443"))
	config := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: in-cluster
  cluster:
    server: https://kubernetes.default.svc:%s
    certificate-authority: %s
users:
- name: rsdw-c2
  user:
    tokenFile: %s
contexts:
- name: in-cluster
  context:
    cluster: in-cluster
    user: rsdw-c2
current-context: in-cluster
`, port, caFile, tokenFile)
	path := filepath.Join(os.TempDir(), "rsdw-c2-kubeconfig")
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		log.Printf("write in-cluster kubeconfig: %v", err)
		return
	}
	if err := os.Setenv("KUBECONFIG", path); err != nil {
		log.Printf("set in-cluster kubeconfig: %v", err)
	}
}

func (k *kubeOrchestrator) Deploy(ctx context.Context, server Server) error {
	if server.GamePort == 0 {
		server.GamePort = 7777
	}
	if server.StorageGiB == 0 {
		server.StorageGiB = 40
	}
	server.WorldName = defaultValue(server.WorldName, server.Name)
	server.ServiceType = defaultValue(server.ServiceType, "LoadBalancer")
	if server.MemoryLimitMiB == 0 {
		server.MemoryLimitMiB = 2048
	}
	if server.CPULimitMillis == 0 {
		server.CPULimitMillis = 1000
	}
	if _, err := k.runner.Run(ctx, k.kubectl, "get", "namespace", server.Namespace); err != nil {
		if _, err := k.runner.Run(ctx, k.kubectl, "create", "namespace", server.Namespace); err != nil {
			return fmt.Errorf("create namespace: %w", err)
		}
	}
	secret := server.Release + "-api"
	if server.PasswordSecret != "" && (server.ServerPassword != "" || server.AdminPassword != "") {
		if _, err := k.runner.Run(ctx, k.kubectl, "-n", server.Namespace, "get", "secret", server.PasswordSecret); err != nil {
			if _, err := k.runner.Run(ctx, k.kubectl, "-n", server.Namespace, "create", "secret", "generic", server.PasswordSecret, "--from-literal=serverPassword="+server.ServerPassword, "--from-literal=adminPassword="+server.AdminPassword); err != nil {
				return errors.New("could not create server password Secret")
			}
		}
	}
	if _, err := k.runner.Run(ctx, k.kubectl, "-n", server.Namespace, "get", "secret", secret); err != nil {
		token, tokenErr := randomToken()
		if tokenErr != nil {
			return tokenErr
		}
		if _, err := k.runner.Run(ctx, k.kubectl, "-n", server.Namespace, "create", "secret", "generic", secret, "--from-literal=token="+token); err != nil {
			return fmt.Errorf("create API token Secret: %w", err)
		}
	}
	args := []string{"upgrade", "--install", server.Release, k.chart, "--namespace", server.Namespace, "--create-namespace", "--set-literal", "server.env.RSDW_OWNER_ID=" + server.OwnerID, "--set-literal", "server.env.RSDW_SERVER_NAME=" + server.Name, "--set-string", "image.repository=" + k.imageRepository, "--set-string", "image.tag=" + imageTag(server.DesiredImage), "--set-string", "api.bearerTokenSecret.name=" + secret}
	if k.chartVersion != "" {
		args = append(args, "--version", k.chartVersion)
	}
	args = append(args, "--set-string", fmt.Sprintf("resources.limits.memory=%dMi", server.MemoryLimitMiB), "--set-string", fmt.Sprintf("resources.limits.cpu=%dm", server.CPULimitMillis), "--set-string", "resources.requests.memory=256Mi", "--set-string", "resources.requests.cpu=100m")
	for _, setting := range []string{
		"server.env.RSDW_WORLD_NAME=" + server.WorldName,
		"server.env.RSDW_ADMINS=" + server.AdminIDs,
		"server.env.RSDW_ADDITIONAL_ARGS=" + strings.TrimSpace(server.AdditionalArgs+" -ini:Game:[/Script/Engine.GameSession]:MaxPlayers="+strconv.Itoa(server.MaxPlayers)),
		"server.env.RSDW_AUTO_STOP_ON_UPDATE=" + strconv.FormatBool(server.AutoStopOnUpdate),
		"server.env.DEBUG=" + strconv.Itoa(server.DebugLevel),
		"server.env.STEAMAPPVALIDATE=" + map[bool]string{false: "0", true: "1"}[server.ValidateGameFiles],
	} {
		args = append(args, "--set-literal", setting)
	}
	args = append(args, "--set", fmt.Sprintf("server.port=%d,service.port=%d,persistence.size=%dGi,service.type=%s", server.GamePort, server.GamePort, server.StorageGiB, server.ServiceType))
	if server.PasswordSecret != "" {
		for i, entry := range []struct{ name, key string }{{"RSDW_PASSWORD", "serverPassword"}, {"RSDW_ADMIN_PASSWORD", "adminPassword"}} {
			args = append(args, "--set-string", fmt.Sprintf("server.extraEnv[%d].name=%s,server.extraEnv[%d].valueFrom.secretKeyRef.name=%s,server.extraEnv[%d].valueFrom.secretKeyRef.key=%s", i, entry.name, i, server.PasswordSecret, i, entry.key))
		}
	}
	if _, err := k.runner.Run(ctx, k.helm, args...); err != nil {
		return fmt.Errorf("helm deploy: %w", err)
	}
	return nil
}

func (k *kubeOrchestrator) Restart(ctx context.Context, server Server) error {
	_, err := k.runner.Run(ctx, k.kubectl, "-n", server.Namespace, "rollout", "restart", "deployment/"+deploymentName(server.Release))
	return err
}

func (k *kubeOrchestrator) Logs(ctx context.Context, server Server, tail int) ([]LogLine, error) {
	output, err := k.runner.Run(ctx, k.kubectl, "-n", server.Namespace, "logs", "deployment/"+deploymentName(server.Release), "-c", "server", "--tail", strconv.Itoa(tail), "--timestamps")
	if err != nil {
		return nil, err
	}
	return parseLogs(string(output)), nil
}

func (k *kubeOrchestrator) Refresh(ctx context.Context, server Server) (Server, error) {
	target, status, err := k.resolvePod(ctx, server)
	if err != nil {
		return server, err
	}
	if target.container.Image == "" {
		return server, errors.New("observed server image is unavailable")
	}
	observed := observation{at: time.Now().UTC(), metrics: emptyMetrics(), status: status, image: target.container.Image}
	return joinObservation(server, observed, time.Now().UTC()), nil
}

func (k *kubeOrchestrator) CheckUpdate(ctx context.Context, server Server) (Server, error) {
	refreshed, err := k.Refresh(ctx, server)
	if err != nil {
		return server, err
	}
	latest, err := k.latestImageTag(ctx)
	if err != nil {
		return server, fmt.Errorf("check image update: %w", err)
	}
	if latest != "" && (refreshed.DesiredImage == "" || refreshed.DesiredImage == refreshed.CurrentImage) {
		latestVersion, latestOK := semverTag(latest)
		currentVersion, currentOK := semverTag(imageTag(refreshed.CurrentImage))
		if !currentOK || (latestOK && newerVersion(latestVersion, currentVersion)) {
			refreshed.DesiredImage = k.imageRepository + ":" + latest
		}
	}
	refreshed.UpdateAvailable = refreshed.DesiredImage != "" && refreshed.CurrentImage != "" && refreshed.DesiredImage != refreshed.CurrentImage
	return refreshed, nil
}

func (k *kubeOrchestrator) latestImageTag(ctx context.Context) (string, error) {
	parts := strings.SplitN(strings.TrimPrefix(k.imageRepository, "https://"), "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", errors.New("image repository must include a registry host")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+parts[0]+"/v2/"+parts[1]+"/tags/list", nil)
	if err != nil {
		return "", err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", err
	}
	if response.StatusCode == http.StatusUnauthorized && parts[0] == "ghcr.io" {
		response.Body.Close()
		query := url.Values{"service": {"ghcr.io"}, "scope": {"repository:" + parts[1] + ":pull"}}
		tokenRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://ghcr.io/token?"+query.Encode(), nil)
		if err != nil {
			return "", err
		}
		tokenResponse, err := http.DefaultClient.Do(tokenRequest)
		if err != nil {
			return "", fmt.Errorf("registry authentication: %w", err)
		}
		defer tokenResponse.Body.Close()
		if tokenResponse.StatusCode != http.StatusOK {
			return "", fmt.Errorf("registry authentication returned %s", tokenResponse.Status)
		}
		var credentials struct {
			Token       string `json:"token"`
			AccessToken string `json:"access_token"`
		}
		if err := json.NewDecoder(io.LimitReader(tokenResponse.Body, 1<<20)).Decode(&credentials); err != nil {
			return "", errors.New("invalid registry authentication response")
		}
		token := defaultValue(credentials.Token, credentials.AccessToken)
		if token == "" {
			return "", errors.New("registry authentication returned an empty token")
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err = http.DefaultClient.Do(request)
		if err != nil {
			return "", err
		}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry returned %s", response.Status)
	}
	var payload struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&payload); err != nil {
		return "", err
	}
	best := ""
	var bestVersion [3]int
	for _, tag := range payload.Tags {
		version, ok := semverTag(tag)
		if !ok || (best != "" && !newerVersion(version, bestVersion)) {
			continue
		}
		best, bestVersion = tag, version
	}
	return best, nil
}

func semverTag(tag string) ([3]int, bool) {
	match := regexp.MustCompile(`^v?([0-9]+)\.([0-9]+)\.([0-9]+)$`).FindStringSubmatch(tag)
	if len(match) != 4 {
		return [3]int{}, false
	}
	var version [3]int
	for index := range version {
		value, err := strconv.Atoi(match[index+1])
		if err != nil {
			return [3]int{}, false
		}
		version[index] = value
	}
	return version, true
}

func newerVersion(candidate, current [3]int) bool {
	for index := range candidate {
		if candidate[index] != current[index] {
			return candidate[index] > current[index]
		}
	}
	return false
}

type App struct {
	store         *Store
	orchestrator  Orchestrator
	demo          bool
	authToken     string
	imageRepo     string
	telemetryOnce sync.Once
	telemetry     *telemetryStore
}

func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/api" {
		if r.URL.Path != "/api/auth" && r.URL.Path != "/api/session" && !a.authorized(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "authentication required"})
			return
		}
		a.api(w, r)
		return
	}
	http.FileServer(http.FS(webRoot)).ServeHTTP(w, r)
}

func (a *App) authorized(r *http.Request) bool {
	return a.authToken == "" || r.Header.Get("Authorization") == "Bearer "+a.authToken
}

func (a *App) api(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/auth" && r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]bool{"required": a.authToken != ""})
		return
	}
	if r.URL.Path == "/api/session" && r.Method == http.MethodPost {
		var body struct {
			Token string `json:"token"`
		}
		if err := decodeJSON(r, &body); err != nil || body.Token == "" || body.Token != a.authToken {
			if a.authToken == "" && body.Token == "" {
				writeJSON(w, http.StatusNoContent, nil)
				return
			}
			writeError(w, http.StatusUnauthorized, "invalid admin token")
			return
		}
		writeJSON(w, http.StatusNoContent, nil)
		return
	}
	if r.URL.Path == "/api/bootstrap" && r.Method == http.MethodGet {
		a.handleBootstrap(w, r)
		return
	}
	if r.URL.Path == "/api/servers" && r.Method == http.MethodPost {
		a.handleCreate(w, r)
		return
	}
	if r.URL.Path == "/api/events" && r.Method == http.MethodGet {
		a.handleEvents(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/servers/") {
		a.handleServerRoute(w, r)
		return
	}
	writeError(w, http.StatusNotFound, "route not found")
}

func (a *App) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	snapshot := a.store.Snapshot()
	servers := make([]Server, 0, len(snapshot.Servers))
	for _, server := range snapshot.Servers {
		servers = append(servers, a.telemetryFor(server, "60s").Server)
	}
	sort.Slice(servers, func(i, j int) bool { return servers[i].Name < servers[j].Name })
	mode := "kubernetes"
	if a.demo {
		mode = "demo"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"servers": servers, "events": recentEvents(snapshot.Events, 20), "mode": mode, "cluster": map[string]any{"name": envOr("RSDW_CLUSTER_NAME", "local-cluster"), "region": envOr("RSDW_CLUSTER_REGION", "eu-central"), "uptimeSeconds": int64(time.Since(startedAt).Seconds())},
		"capabilities": map[string]bool{"create": true, "restart": true, "update": true, "logs": true, "updateCheck": true},
	})
}

func (a *App) handleCreate(w http.ResponseWriter, r *http.Request) {
	var request CreateServerRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if request.MemoryLimitMiB == 0 {
		request.MemoryLimitMiB = 2048
	}
	if request.CPULimitMillis == 0 {
		request.CPULimitMillis = 1000
	}
	if err := validateCreate(request, a.demo); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	release := slugify(request.Name)
	if release == "" {
		writeError(w, http.StatusBadRequest, "name must include a letter or number")
		return
	}
	server := Server{ID: release, Name: strings.TrimSpace(request.Name), Namespace: defaultValue(request.Namespace, "dragonwilds"), Release: release, Region: defaultValue(request.Region, "eu-central"), OwnerID: request.OwnerID, CurrentImage: envOr("RSDW_IMAGE_REPOSITORY", "ghcr.io/petzkod5/rsdragonwilds-server") + ":" + defaultValue(request.ImageTag, envOr("RSDW_DEFAULT_IMAGE_TAG", "0.1.1")), MaxPlayers: request.MaxPlayers, Status: StatusStarting, LastRestart: time.Now().UTC().Format(time.RFC3339), LastSeen: time.Now().UTC().Format(time.RFC3339), Endpoint: release + ".dragonwilds.local:7777"}
	server.DesiredImage = server.CurrentImage
	server.MemoryLimitMiB = request.MemoryLimitMiB
	server.CPULimitMillis = request.CPULimitMillis
	server.ServerSettings = request.ServerSettings
	server.WorldName = defaultValue(server.WorldName, server.Name)
	if server.GamePort == 0 {
		server.GamePort = 7777
	}
	if server.StorageGiB == 0 {
		server.StorageGiB = 40
	}
	server.ServiceType = defaultValue(server.ServiceType, "LoadBalancer")
	server.ServerPassword, server.AdminPassword = request.ServerPassword, request.AdminPassword
	if request.ServerPassword != "" || request.AdminPassword != "" {
		server.PasswordSecret = release + "-settings"
	}
	if !a.demo {
		server.Endpoint = ""
	}
	if snapshot := a.store.Snapshot(); snapshot.Servers[server.ID].ID != "" {
		writeError(w, http.StatusConflict, "a server with this name already exists")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	if err := a.orchestrator.Deploy(ctx, server); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if a.demo {
		server.Status = StatusOnline
	}
	server.ServerPassword, server.AdminPassword = "", ""
	if err := a.store.Update(func(state *State) error {
		state.Servers[server.ID] = server
		appendEvent(state, server, "system", "success", "Server created", "Helm release accepted")
		return nil
	}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, server)
}

func (a *App) handleEvents(w http.ResponseWriter, r *http.Request) {
	snapshot := a.store.Snapshot()
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("query")))
	category := r.URL.Query().Get("category")
	serverID := r.URL.Query().Get("serverId")
	events := make([]Event, 0)
	for _, event := range snapshot.Events {
		if category != "" && category != "all" && event.Category != category {
			continue
		}
		if serverID != "" && serverID != "all" && event.ServerID != serverID {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(event.Message+" "+event.Details+" "+event.ServerName), query) {
			continue
		}
		events = append(events, event)
	}
	sort.Slice(events, func(i, j int) bool { return events[i].Timestamp.After(events[j].Timestamp) })
	writeJSON(w, http.StatusOK, map[string]any{"events": events, "total": len(events)})
}

func (a *App) handleServerRoute(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/servers/"), "/")
	if len(parts) < 1 || parts[0] == "" {
		writeError(w, http.StatusNotFound, "server not found")
		return
	}
	serverID := parts[0]
	snapshot := a.store.Snapshot()
	server, ok := snapshot.Servers[serverID]
	if !ok {
		writeError(w, http.StatusNotFound, "server not found")
		return
	}
	if len(parts) == 2 && parts[1] == "logs" && r.Method == http.MethodGet {
		tail := boundedInt(r.URL.Query().Get("tail"), 100, 1, 500)
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		lines, err := a.orchestrator.Logs(ctx, server, tail)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"server": server, "lines": lines})
		return
	}
	if len(parts) == 2 && parts[1] == "telemetry" && r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, a.telemetryFor(server, r.URL.Query().Get("range")))
		return
	}
	if len(parts) == 3 && parts[1] == "actions" && r.Method == http.MethodPost {
		switch parts[2] {
		case "restart":
			a.handleRestart(w, r, server)
		case "update":
			a.handleUpdate(w, r, server)
		case "check-update":
			a.handleCheckUpdate(w, r, server)
		default:
			writeError(w, http.StatusNotFound, "action not found")
		}
		return
	}
	writeError(w, http.StatusNotFound, "route not found")
}

func (a *App) handleRestart(w http.ResponseWriter, r *http.Request, server Server) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := a.orchestrator.Restart(ctx, server); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	server.Status = StatusStarting
	server.LastRestart = time.Now().UTC().Format(time.RFC3339)
	server.LastSeen = time.Now().UTC().Format(time.RFC3339)
	if err := a.store.Update(func(state *State) error {
		state.Servers[server.ID] = server
		appendEvent(state, server, "system", "warning", "Restart requested", "The server is restarting")
		return nil
	}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, server)
}

func (a *App) handleUpdate(w http.ResponseWriter, r *http.Request, server Server) {
	var request UpdateServerRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`).MatchString(request.ImageTag) {
		writeError(w, http.StatusBadRequest, "imageTag must contain 1 to 64 letters, numbers, dots, underscores, or hyphens")
		return
	}
	server.DesiredImage = envOr("RSDW_IMAGE_REPOSITORY", "ghcr.io/petzkod5/rsdragonwilds-server") + ":" + request.ImageTag
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	if err := a.orchestrator.Deploy(ctx, server); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	server.CurrentImage = server.DesiredImage
	server.UpdateAvailable = false
	server.Status = StatusStarting
	server.LastSeen = time.Now().UTC().Format(time.RFC3339)
	if err := a.store.Update(func(state *State) error {
		state.Servers[server.ID] = server
		appendEvent(state, server, "update", "success", "Image update requested", server.DesiredImage)
		return nil
	}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, server)
}

func (a *App) handleCheckUpdate(w http.ResponseWriter, r *http.Request, server Server) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	refreshed, err := a.orchestrator.CheckUpdate(ctx, server)
	if err != nil && !a.demo {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if err != nil {
		refreshed = server
	}
	if err := a.store.Update(func(state *State) error {
		current := state.Servers[server.ID]
		if current.DesiredImage == server.DesiredImage {
			current.DesiredImage = refreshed.DesiredImage
		}
		current.UpdateAvailable = refreshed.CurrentImage != "" && current.DesiredImage != refreshed.CurrentImage
		state.Servers[server.ID] = current
		refreshed.DesiredImage = current.DesiredImage
		refreshed.UpdateAvailable = current.UpdateAvailable
		appendEvent(state, refreshed, "update", severityFor(refreshed.UpdateAvailable), "Update check complete", updateDetails(refreshed))
		return nil
	}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, refreshed)
}

func appendEvent(state *State, server Server, category, severity, message, details string) {
	state.Events = append(state.Events, Event{ID: randomID(), Timestamp: time.Now().UTC(), ServerID: server.ID, ServerName: server.Name, Category: category, Severity: severity, Message: message, Details: details})
	if len(state.Events) > 500 {
		state.Events = state.Events[len(state.Events)-500:]
	}
}

func parseLogs(raw string) []LogLine {
	lines := make([]LogLine, 0)
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		parsed := time.Now().UTC()
		message := line
		if len(parts) == 2 {
			if value, err := time.Parse(time.RFC3339Nano, parts[0]); err == nil {
				parsed, message = value, parts[1]
			}
		}
		level := "INFO"
		if strings.Contains(strings.ToUpper(message), "WARN") {
			level = "WARN"
		}
		if strings.Contains(strings.ToUpper(message), "ERROR") {
			level = "ERROR"
		}
		lines = append(lines, LogLine{Timestamp: parsed, Level: level, Message: message})
	}
	return lines
}

func validateCreate(request CreateServerRequest, demo bool) error {
	if request.GamePort != 0 && (request.GamePort < 1024 || request.GamePort > 65535) {
		return errors.New("gamePort must be between 1024 and 65535")
	}
	if request.StorageGiB != 0 && (request.StorageGiB < 1 || request.StorageGiB > 2048) {
		return errors.New("storageGiB must be between 1 and 2048")
	}
	if request.ServiceType != "" && request.ServiceType != "ClusterIP" && request.ServiceType != "NodePort" && request.ServiceType != "LoadBalancer" {
		return errors.New("serviceType must be ClusterIP, NodePort, or LoadBalancer")
	}
	if request.DebugLevel < 0 || request.DebugLevel > 3 {
		return errors.New("debugLevel must be between 0 and 3")
	}
	for field, value := range map[string]string{"worldName": request.WorldName, "adminIds": request.AdminIDs, "additionalArgs": request.AdditionalArgs, "serverPassword": request.ServerPassword, "adminPassword": request.AdminPassword} {
		if len(value) > 2048 || strings.ContainsAny(value, "\x00\r\n") {
			return fmt.Errorf("%s must be a single line of at most 2048 characters", field)
		}
	}
	if request.MemoryLimitMiB != 0 && (request.MemoryLimitMiB < 256 || request.MemoryLimitMiB > 65536) {
		return errors.New("memoryLimitMiB must be between 256 and 65536")
	}
	if request.CPULimitMillis != 0 && (request.CPULimitMillis < 100 || request.CPULimitMillis > 64000) {
		return errors.New("cpuLimitMillis must be between 100 and 64000")
	}
	if strings.TrimSpace(request.Name) == "" || len(request.Name) > 48 {
		return errors.New("name is required and must be 48 characters or fewer")
	}
	if !demo && strings.TrimSpace(request.OwnerID) == "" {
		return errors.New("ownerId is required")
	}
	if request.MaxPlayers < 1 || request.MaxPlayers > 64 {
		return errors.New("maxPlayers must be between 1 and 64")
	}
	for field, value := range map[string]string{"namespace": request.Namespace, "imageTag": request.ImageTag} {
		if value == "" {
			continue
		}
		if field == "namespace" && !regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`).MatchString(value) {
			return errors.New("namespace must be a lowercase Kubernetes name")
		}
		if field == "imageTag" && !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`).MatchString(value) {
			return errors.New("imageTag must contain 1 to 64 letters, numbers, dots, underscores, or hyphens")
		}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if status != http.StatusNoContent {
		_ = json.NewEncoder(w).Encode(value)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func decodeJSON(r *http.Request, value any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 32<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

func recentEvents(events []Event, limit int) []Event {
	result := append([]Event(nil), events...)
	sort.Slice(result, func(i, j int) bool { return result[i].Timestamp.After(result[j].Timestamp) })
	if len(result) > limit {
		result = result[:limit]
	}
	return result
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
func defaultValue(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}
func randomID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b)
}
func randomToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate API token: %w", err)
	}
	return hex.EncodeToString(b), nil
}
func slugify(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(value, "-")
	return strings.Trim(value, "-")
}
func imageTag(image string) string {
	if index := strings.LastIndex(image, ":"); index >= 0 {
		return image[index+1:]
	}
	return image
}
func deploymentName(release string) string { return release + "-rsdragonwilds" }
func boundedInt(raw string, fallback, minValue, maxValue int) int {
	value, err := strconv.Atoi(raw)
	if err != nil || value < minValue || value > maxValue {
		return fallback
	}
	return value
}
func severityFor(update bool) string {
	if update {
		return "warning"
	}
	return "success"
}
func updateDetails(server Server) string {
	if server.UpdateAvailable {
		return "Desired image differs from the running image"
	}
	return "Running image matches the desired image"
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

var startedAt = time.Now()

func main() {
	demo := strings.EqualFold(os.Getenv("RSDW_DEMO_DATA"), "true")
	authToken := os.Getenv("RSDW_ADMIN_TOKEN")
	if !demo && authToken == "" {
		log.Fatal("RSDW_ADMIN_TOKEN is required outside demo mode")
	}
	statePath := envOr("RSDW_STATE_FILE", "/var/lib/rsdw-c2/state.json")
	store, err := NewStore(statePath, demo)
	if err != nil {
		log.Fatal(err)
	}
	var orchestrator Orchestrator = newKubeOrchestrator()
	if demo {
		orchestrator = demoOrchestrator{}
	}
	app := &App{store: store, orchestrator: orchestrator, demo: demo, authToken: authToken}
	go app.runCollector(context.Background(), 15*time.Second)
	addr := envOr("RSDW_LISTEN_ADDR", ":8080")
	server := &http.Server{Addr: addr, Handler: app, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 2 * time.Minute, IdleTimeout: 60 * time.Second}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("rsdw-c2 listening on %s demo=%t", listener.Addr(), demo)
	log.Fatal(server.Serve(listener))
}
