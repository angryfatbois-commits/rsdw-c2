package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
	yamlv2 "sigs.k8s.io/yaml/goyaml.v2"
)

const (
	BackupDefinitionAPIVersion = "rsdw-c2.petzko.dev/v1"
	BackupDefinitionKind       = "BackupDefinition"
)

type BackupStrategy string

const (
	BackupStrategyLogicalFiles   BackupStrategy = "logical-files"
	BackupStrategyVolumeSnapshot BackupStrategy = "volume-snapshot"
	BackupStrategyLogBundle      BackupStrategy = "log-bundle"
)

type BackupCollector string

const (
	BackupCollectorServerFiles BackupCollector = "server-files"
)

type BackupServerState string

const (
	BackupServerRunning BackupServerState = "running"
	BackupServerStopped BackupServerState = "stopped"
)

type BackupItemRequirement string

const (
	BackupItemRequired BackupItemRequirement = "required"
	BackupItemOptional BackupItemRequirement = "optional"
)

type BackupItemKind string

const (
	BackupItemFile      BackupItemKind = "file"
	BackupItemDirectory BackupItemKind = "directory"
)

type BackupSourceRule struct {
	ServerState BackupServerState `json:"serverState" yaml:"serverState"`
	Collector   BackupCollector   `json:"collector" yaml:"collector"`
	Path        string            `json:"path" yaml:"path"`
}

type BackupItemSpec struct {
	Name        string                `json:"name" yaml:"name"`
	Kind        BackupItemKind        `json:"kind" yaml:"kind"`
	Requirement BackupItemRequirement `json:"requirement" yaml:"requirement"`
	Sources     []BackupSourceRule    `json:"sources" yaml:"sources"`
}

type BackupDefinition struct {
	APIVersion string           `json:"apiVersion" yaml:"apiVersion"`
	Kind       string           `json:"kind" yaml:"kind"`
	ID         string           `json:"id" yaml:"id"`
	Revision   int              `json:"revision" yaml:"revision"`
	Name       string           `json:"name" yaml:"name"`
	ServerType string           `json:"serverType" yaml:"serverType"`
	Strategy   BackupStrategy   `json:"strategy" yaml:"strategy"`
	Items      []BackupItemSpec `json:"items" yaml:"items"`
}

func (definition BackupDefinition) Validate() error {
	if definition.APIVersion != BackupDefinitionAPIVersion {
		return fmt.Errorf("unsupported backup definition apiVersion %q", definition.APIVersion)
	}
	if definition.Kind != BackupDefinitionKind {
		return fmt.Errorf("unsupported backup definition kind %q", definition.Kind)
	}
	if err := validateBackupID("definition id", definition.ID); err != nil {
		return err
	}
	if definition.Revision < 1 {
		return errors.New("definition revision must be at least 1")
	}
	if err := validateDisplayValue("definition name", definition.Name); err != nil {
		return err
	}
	if err := validateBackupID("server type", definition.ServerType); err != nil {
		return err
	}
	if definition.ServerType != "dragonwilds" {
		return errors.New("unsupported server type: only dragonwilds has a registered collector")
	}
	if definition.Strategy != BackupStrategyLogicalFiles {
		return fmt.Errorf("unsupported backup strategy %q", definition.Strategy)
	}
	if len(definition.Items) == 0 {
		return errors.New("logical-files definitions require at least one item")
	}

	itemNames := make(map[string]struct{}, len(definition.Items))
	for index, item := range definition.Items {
		if err := validateBackupID("item name", item.Name); err != nil {
			return fmt.Errorf("items[%d]: %w", index, err)
		}
		if _, exists := itemNames[item.Name]; exists {
			return fmt.Errorf("items[%d]: duplicate item name %q", index, item.Name)
		}
		itemNames[item.Name] = struct{}{}
		if item.Kind != "" && item.Kind != BackupItemFile && item.Kind != BackupItemDirectory {
			return fmt.Errorf("items[%d]: unsupported item kind %q", index, item.Kind)
		}
		if item.Requirement != BackupItemRequired && item.Requirement != BackupItemOptional {
			return fmt.Errorf("items[%d]: unsupported requirement %q", index, item.Requirement)
		}
		if len(item.Sources) == 0 {
			return fmt.Errorf("items[%d]: at least one source is required", index)
		}
		states := make(map[BackupServerState]struct{}, len(item.Sources))
		for sourceIndex, source := range item.Sources {
			if source.ServerState != BackupServerRunning && source.ServerState != BackupServerStopped {
				return fmt.Errorf("items[%d].sources[%d]: unsupported server state %q", index, sourceIndex, source.ServerState)
			}
			if _, exists := states[source.ServerState]; exists {
				return fmt.Errorf("items[%d].sources[%d]: duplicate %q source", index, sourceIndex, source.ServerState)
			}
			states[source.ServerState] = struct{}{}
			if source.Collector != BackupCollectorServerFiles {
				return fmt.Errorf("items[%d].sources[%d]: unsupported collector %q", index, sourceIndex, source.Collector)
			}
			if err := ValidateBackupRelativePath(source.Path); err != nil {
				return fmt.Errorf("items[%d].sources[%d]: %w", index, sourceIndex, err)
			}
		}
	}
	return nil
}

type BackupSource string

func backupSaveSuffix(filename string) string {
	lower := strings.ToLower(filename)
	for _, suffix := range []string{".sav.backup", ".sav", ".bak", ".backup"} {
		if strings.HasSuffix(lower, suffix) {
			return suffix
		}
	}
	return ""
}

const (
	// Keep the stored source tag compatible with existing manifests; live saves use .sav.backup.
	BackupSourceRunningBAK BackupSource = "running-bak"
	BackupSourceStoppedSAV BackupSource = "stopped-sav"
)

type BackupItemConsistency string

const (
	BackupConsistencyAtomicPublish BackupItemConsistency = "atomic-publish"
	BackupConsistencyStableRead    BackupItemConsistency = "stable-read"
	BackupConsistencyStoppedWorld  BackupItemConsistency = "stopped-world"
)

type BackupManifestItem struct {
	Name        string                `json:"name"`
	Kind        BackupItemKind        `json:"kind"`
	SourcePath  string                `json:"sourcePath"`
	Size        int64                 `json:"size"`
	SHA256      string                `json:"sha256"`
	ObjectKey   string                `json:"objectKey"`
	Consistency BackupItemConsistency `json:"consistency"`
}

type BackupManifest struct {
	SchemaVersion      int                  `json:"schemaVersion"`
	ID                 string               `json:"id"`
	DefinitionID       string               `json:"definitionId"`
	DefinitionRevision int                  `json:"definitionRevision"`
	ServerID           string               `json:"serverId"`
	ServerType         string               `json:"serverType"`
	Source             BackupSource         `json:"source"`
	CreatedAt          time.Time            `json:"createdAt"`
	Items              []BackupManifestItem `json:"items"`
}

type BackupManifestDraft struct {
	ID                 string
	DefinitionID       string
	DefinitionRevision int
	ServerID           string
	ServerType         string
	Source             BackupSource
	CreatedAt          time.Time
}

type BackupRunPhase string

const (
	BackupRunQueued     BackupRunPhase = "queued"
	BackupRunCapturing  BackupRunPhase = "capturing"
	BackupRunPublishing BackupRunPhase = "publishing"
)

type BackupRunStatus string

const (
	BackupRunStatusRunning     BackupRunStatus = "running"
	BackupRunStatusSucceeded   BackupRunStatus = "succeeded"
	BackupRunStatusFailed      BackupRunStatus = "failed"
	BackupRunStatusInterrupted BackupRunStatus = "interrupted"
)

type BackupRun struct {
	ID           string             `json:"id"`
	ServerID     string             `json:"serverId"`
	DefinitionID string             `json:"definitionId"`
	ScheduleID   string             `json:"scheduleId,omitempty"`
	BackendID    string             `json:"backendId"`
	Source       BackupSource       `json:"source,omitempty"`
	Phase        BackupRunPhase     `json:"phase"`
	Status       BackupRunStatus    `json:"status"`
	ManifestID   string             `json:"manifestId,omitempty"`
	Bundle       BackupObject       `json:"bundle,omitempty"`
	Idempotency  IdempotencyRequest `json:"idempotency,omitempty"`
	ItemCount    int                `json:"itemCount,omitempty"`
	Error        string             `json:"error,omitempty"`
	CreatedAt    time.Time          `json:"createdAt"`
	UpdatedAt    time.Time          `json:"updatedAt"`
}

type BackupSchedule struct {
	ID             string          `json:"id"`
	ServerID       string          `json:"serverId"`
	DefinitionID   string          `json:"definitionId"`
	BackendID      string          `json:"backendId"`
	Enabled        bool            `json:"enabled"`
	Timezone       string          `json:"timezone"`
	Expression     string          `json:"expression"`
	Mode           rebootMode      `json:"mode,omitempty"`
	Cron           string          `json:"cron,omitempty"`
	IntervalValue  int             `json:"intervalValue,omitempty"`
	IntervalUnit   string          `json:"intervalUnit,omitempty"`
	DailyTimes     []string        `json:"dailyTimes,omitempty"`
	IntervalAnchor *time.Time      `json:"intervalAnchor,omitempty"`
	NextRun        *time.Time      `json:"nextRun,omitempty"`
	LastRun        *time.Time      `json:"lastRun,omitempty"`
	LastResult     BackupRunStatus `json:"lastResult,omitempty"`
	LastError      string          `json:"lastError,omitempty"`
	Revision       int             `json:"revision"`
}

type StorageBackendKind string

const (
	StorageBackendLocal StorageBackendKind = "local"
	StorageBackendS3    StorageBackendKind = "s3"
)

type StorageBackendStatus string

const (
	StorageBackendConnected    StorageBackendStatus = "connected"
	StorageBackendDisconnected StorageBackendStatus = "disconnected"
)

type StorageBackend struct {
	ID         string               `json:"id"`
	Name       string               `json:"name"`
	Kind       StorageBackendKind   `json:"kind"`
	Status     StorageBackendStatus `json:"status"`
	Removable  bool                 `json:"removable"`
	Endpoint   string               `json:"endpoint,omitempty"`
	Bucket     string               `json:"bucket,omitempty"`
	Region     string               `json:"region,omitempty"`
	PathPrefix string               `json:"pathPrefix,omitempty"`
	UseSSL     bool                 `json:"useSSL,omitempty"`
	SecretRef  SecretReference      `json:"secretRef,omitempty"`
}

func NewLocalStorageBackend(status StorageBackendStatus) StorageBackend {
	return StorageBackend{ID: "local", Name: "Local", Kind: StorageBackendLocal, Status: status, Removable: false}
}

func (backend StorageBackend) Validate() error {
	if err := validateBackupID("backend id", backend.ID); err != nil {
		return err
	}
	if err := validateDisplayValue("backend name", backend.Name); err != nil {
		return err
	}
	if backend.Status != StorageBackendConnected && backend.Status != StorageBackendDisconnected {
		return fmt.Errorf("unsupported storage backend status %q", backend.Status)
	}
	switch backend.Kind {
	case StorageBackendLocal:
		if backend.Removable {
			return errors.New("the local storage backend cannot be removable")
		}
	case StorageBackendS3:
		if !backend.Removable {
			return errors.New("an S3 storage backend must be removable")
		}
		if len(backend.Endpoint) == 0 || len(backend.Endpoint) > 253 {
			return errors.New("S3 backend endpoint must contain 1 to 253 characters")
		}
		if strings.ContainsAny(backend.Endpoint, " \t\r\n\x00") {
			return errors.New("S3 backend endpoint must not contain whitespace")
		}
		if len(backend.Bucket) == 0 || len(backend.Bucket) > 253 {
			return errors.New("S3 backend bucket must contain 1 to 253 characters")
		}
		if len(backend.Region) > 64 {
			return errors.New("S3 backend region must be 64 characters or fewer")
		}
		if len(backend.PathPrefix) > 253 {
			return errors.New("S3 backend path prefix must be 253 characters or fewer")
		}
		if len(backend.SecretRef.Name) > 253 || !secretNamePattern.MatchString(backend.SecretRef.Name) {
			return errors.New("S3 backend secretRef must contain a Kubernetes Secret name, never a credential")
		}
	default:
		return fmt.Errorf("unsupported storage backend kind %q", backend.Kind)
	}
	return nil
}

func (backend StorageBackend) CanRemove() bool {
	return backend.Kind != StorageBackendLocal && backend.Removable
}

type IdempotencyRequest struct {
	Actor         string `json:"actor"`
	Operation     string `json:"operation"`
	Key           string `json:"key"`
	RequestDigest string `json:"requestDigest"`
}

var ErrIdempotencyConflict = errors.New("idempotency key was already used with a different request")

func NewIdempotencyRequest(actor, operation, key string, request any) (IdempotencyRequest, error) {
	actor = strings.TrimSpace(actor)
	operation = strings.TrimSpace(operation)
	key = strings.TrimSpace(key)
	if actor == "" || len(actor) > 256 {
		return IdempotencyRequest{}, errors.New("idempotency actor must contain 1 to 256 characters")
	}
	if operation == "" || len(operation) > 128 {
		return IdempotencyRequest{}, errors.New("idempotency operation must contain 1 to 128 characters")
	}
	if key == "" || len(key) > 256 {
		return IdempotencyRequest{}, errors.New("idempotency key must contain 1 to 256 characters")
	}
	canonical, err := json.Marshal(request)
	if err != nil {
		return IdempotencyRequest{}, fmt.Errorf("canonicalize idempotency request: %w", err)
	}
	digest := sha256.Sum256(canonical)
	return IdempotencyRequest{
		Actor:         actor,
		Operation:     operation,
		Key:           key,
		RequestDigest: hex.EncodeToString(digest[:]),
	}, nil
}

func (request IdempotencyRequest) Identity() string {
	sum := sha256.Sum256([]byte(request.Actor + "\x00" + request.Operation + "\x00" + request.Key))
	return hex.EncodeToString(sum[:])
}

func CheckIdempotency(existing, incoming IdempotencyRequest) error {
	if existing.Actor != incoming.Actor || existing.Operation != incoming.Operation || existing.Key != incoming.Key {
		return errors.New("idempotency identities do not match")
	}
	if existing.RequestDigest != incoming.RequestDigest {
		return ErrIdempotencyConflict
	}
	return nil
}

func ImportBackupDefinitionYAML(data []byte) (BackupDefinition, error) {
	decoder := yamlv2.NewDecoder(bytes.NewReader(data))
	var document any
	if err := decoder.Decode(&document); err != nil {
		return BackupDefinition{}, fmt.Errorf("decode backup definition yaml: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return BackupDefinition{}, errors.New("backup definition yaml must contain exactly one document")
	} else if !errors.Is(err, io.EOF) {
		return BackupDefinition{}, fmt.Errorf("decode backup definition yaml: %w", err)
	}
	var definition BackupDefinition
	if err := yaml.UnmarshalStrict(data, &definition); err != nil {
		return BackupDefinition{}, fmt.Errorf("decode backup definition yaml: %w", err)
	}
	if err := definition.Validate(); err != nil {
		return BackupDefinition{}, err
	}
	return definition, nil
}

func ExportBackupDefinitionYAML(definition BackupDefinition) ([]byte, error) {
	if err := definition.Validate(); err != nil {
		return nil, err
	}
	data, err := yaml.Marshal(definition)
	if err != nil {
		return nil, fmt.Errorf("encode backup definition yaml: %w", err)
	}
	return data, nil
}

func ValidateBackupRelativePath(value string) error {
	if value == "" {
		return errors.New("source path is required")
	}
	if strings.ContainsAny(value, "\\\x00\r\n\t*?[]") || strings.HasPrefix(value, "-") || strings.HasPrefix(value, ".c2-") {
		return errors.New("source path contains an invalid character")
	}
	if strings.HasPrefix(value, "/") || path.IsAbs(value) {
		return errors.New("source path must be relative")
	}
	if path.Clean(value) != value || value == "." {
		return errors.New("source path must be clean and cannot traverse directories")
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return errors.New("source path must be clean and cannot traverse directories")
		}
	}
	return nil
}

var backupIDPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,126}[a-z0-9])?$`)

func validateBackupID(field, value string) error {
	if !backupIDPattern.MatchString(value) {
		return fmt.Errorf("%s must contain 1 to 128 lowercase letters, numbers, dots, underscores, or hyphens", field)
	}
	return nil
}

func validateDisplayValue(field, value string) error {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return fmt.Errorf("%s must contain 1 to 128 characters", field)
	}
	return nil
}
