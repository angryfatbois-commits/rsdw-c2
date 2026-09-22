package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	backupLocalBackendID    = "local"
	backupWorldSaveRoot     = "RSDragonwilds/Saved/SaveGames"
	backupDefaultMaxBytes   = int64(9 << 30)
	backupDefaultItemSize   = int64(32 << 20)
	backupDefaultBundleSize = int64(256 << 20)
)

type backupController struct {
	repository BackupRepository
	local      *LocalBackupRepository
	available  bool
	demo       bool
	root       string
	maxBytes   int64
	maxItem    int64
	maxBundle  int64
}

func (s *State) initBackups() {
	if s.BackupDefinitions == nil {
		s.BackupDefinitions = map[string]BackupDefinition{}
	}
	if s.BackupSchedules == nil {
		s.BackupSchedules = map[string]BackupSchedule{}
	}
	if s.BackupManifests == nil {
		s.BackupManifests = map[string]BackupManifest{}
	}
	if s.StorageBackends == nil {
		s.StorageBackends = map[string]StorageBackend{}
	}
	if _, ok := s.StorageBackends[backupLocalBackendID]; !ok {
		s.StorageBackends[backupLocalBackendID] = NewLocalStorageBackend(StorageBackendDisconnected)
	}
	if _, ok := s.BackupDefinitions["dragonwilds-world-save"]; !ok {
		s.BackupDefinitions["dragonwilds-world-save"] = BuiltinDragonwildsDefinition()
	}
}

func BuiltinDragonwildsDefinition() BackupDefinition {
	return BackupDefinition{
		APIVersion: BackupDefinitionAPIVersion,
		Kind:       BackupDefinitionKind,
		ID:         "dragonwilds-world-save",
		Revision:   1,
		Name:       "Dragonwilds World Save",
		ServerType: "dragonwilds",
		Strategy:   BackupStrategyLogicalFiles,
		Items: []BackupItemSpec{{
			Name:        "world-save",
			Kind:        BackupItemFile,
			Requirement: BackupItemRequired,
			Sources: []BackupSourceRule{
				{ServerState: BackupServerRunning, Collector: BackupCollectorServerFiles, Path: backupWorldSaveRoot},
				{ServerState: BackupServerStopped, Collector: BackupCollectorServerFiles, Path: backupWorldSaveRoot},
			},
		}},
	}
}

func newBackupController(store *Store, demo bool) (*backupController, error) {
	controller := &backupController{demo: demo, maxBytes: backupDefaultMaxBytes, maxItem: backupDefaultItemSize, maxBundle: backupDefaultBundleSize}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("RSDW_BACKUPS_ENABLED")), "false") {
		return controller, nil
	}
	if !demo && (store == nil || store.path == "" || strings.EqualFold(strings.TrimSpace(os.Getenv("RSDW_STATE_PERSISTENT")), "false")) {
		return controller, nil
	}
	root := strings.TrimSpace(os.Getenv("RSDW_BACKUP_PATH"))
	if root == "" {
		if store == nil || store.path == "" {
			return controller, nil
		}
		root = filepath.Join(filepath.Dir(store.path), "backups")
	}
	controller.root = root
	for name, destination := range map[string]*int64{
		"RSDW_BACKUP_MAX_REPOSITORY_BYTES": &controller.maxBytes,
		"RSDW_BACKUP_MAX_ITEM_BYTES":       &controller.maxItem,
		"RSDW_BACKUP_MAX_BUNDLE_BYTES":     &controller.maxBundle,
	} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			parsed, err := parsePositiveBytes(value)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			*destination = parsed
		}
	}
	repository, err := NewLocalBackupRepository(LocalBackupRepositoryConfig{Root: root, MaxRepositoryBytes: controller.maxBytes, MaxBackupBytes: controller.maxBundle, MaxItemBytes: controller.maxItem})
	if err != nil {
		return nil, err
	}
	controller.local, controller.repository, controller.available = repository, repository, true
	return controller, nil
}

func parsePositiveBytes(value string) (int64, error) {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, errors.New("byte limit must be a positive integer")
	}
	return parsed, nil
}

func (a *App) backupNow() time.Time {
	if a.clock != nil {
		return a.clock().UTC()
	}
	return time.Now().UTC()
}

func (a *App) backupAvailable() bool {
	_, err := a.backupStorageUsage(context.Background())
	return err == nil
}

func (a *App) backupStorageUsage(ctx context.Context) (int64, error) {
	if a == nil || a.backups == nil || !a.backups.available || a.backups.repository == nil {
		return 0, errors.New("backup storage is unavailable")
	}
	return a.backups.repository.Usage(ctx)
}

func (a *App) backupBundleLimit() int64 {
	if a.backups == nil {
		return 0
	}
	return a.backups.maxBundle
}

func (a *App) backupBackend() StorageBackend {
	status := StorageBackendDisconnected
	if a.backupAvailable() {
		status = StorageBackendConnected
	}
	return NewLocalStorageBackend(status)
}

func (a *App) updateBackupBackend() error {
	backend := a.backupBackend()
	return a.store.Update(func(state *State) error {
		state.initBackups()
		state.StorageBackends[backupLocalBackendID] = backend
		return nil
	})
}

func newBackupID(prefix string) string {
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	}
	return prefix + "-" + hex.EncodeToString(random[:])
}

func (a *App) backupDefinition(id string) (BackupDefinition, error) {
	definition, ok := a.store.Snapshot().BackupDefinitions[id]
	if !ok {
		return BackupDefinition{}, errors.New("backup profile not found")
	}
	return definition, nil
}

func backupSourceForStatus(status Status) (BackupServerState, BackupSource, error) {
	switch status {
	case StatusOnline:
		return BackupServerRunning, BackupSourceRunningBAK, nil
	case StatusStopped:
		return BackupServerStopped, BackupSourceStoppedSAV, nil
	default:
		return "", "", fmt.Errorf("server is in a transition or unknown state (%s)", status)
	}
}

func backupSourcePath(root string, state BackupServerState) (string, error) {
	if state != BackupServerRunning && state != BackupServerStopped {
		return "", errors.New("unsupported backup server state")
	}
	extension := ".sav"
	if state == BackupServerRunning {
		extension = ".sav.backup"
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", err
	}
	var matches []string
	for _, entry := range entries {
		if entry.IsDir() || backupSaveSuffix(entry.Name()) != extension || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		matches = append(matches, entry.Name())
	}
	sort.Strings(matches)
	if len(matches) != 1 {
		if len(matches) == 0 {
			return "", fmt.Errorf("expected one %s source in SaveGames, found none", extension)
		}
		return "", fmt.Errorf("expected one %s source in SaveGames, found %d", extension, len(matches))
	}
	return filepath.Join(root, matches[0]), nil
}

type backupContent struct {
	name        string
	sourcePath  string
	consistency BackupItemConsistency
	content     io.Reader
}

func (a *App) collectDemoBackup(ctx context.Context, server Server, definition BackupDefinition, source BackupSource) ([]BackupPublicationItem, error) {
	if !a.demo {
		return nil, errors.New("demo backup collector is unavailable")
	}
	if err := validateBackupID("server id", server.ID); err != nil {
		return nil, err
	}
	state := BackupServerStopped
	extension := ".sav"
	consistency := BackupConsistencyStoppedWorld
	if source == BackupSourceRunningBAK {
		state = BackupServerRunning
		extension, consistency = ".sav.backup", BackupConsistencyStableRead
	} else if source != BackupSourceStoppedSAV {
		return nil, errors.New("unsupported backup source")
	}
	maxItem, maxBundle := backupDefaultItemSize, backupDefaultBundleSize
	var restored *os.Root
	if a.backups != nil {
		if a.backups.maxItem > 0 {
			maxItem = a.backups.maxItem
		}
		if a.backups.maxBundle > 0 {
			maxBundle = a.backups.maxBundle
		}
		if state == BackupServerStopped && a.backups.root != "" {
			var err error
			restored, err = os.OpenRoot(filepath.Join(a.backups.root, ".demo-worlds", server.ID))
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("open restored demo world: %w", err)
			}
			if restored != nil {
				defer restored.Close()
			}
		}
	}
	items := make([]BackupPublicationItem, 0, len(definition.Items))
	var total int64
	for _, spec := range definition.Items {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var rule *BackupSourceRule
		for index := range spec.Sources {
			if spec.Sources[index].ServerState == state {
				rule = &spec.Sources[index]
				break
			}
		}
		if rule == nil {
			if spec.Requirement == BackupItemOptional {
				continue
			}
			return nil, fmt.Errorf("profile has no %s source rule for %s", state, spec.Name)
		}
		if err := ValidateBackupRelativePath(rule.Path); err != nil {
			return nil, err
		}
		kind := spec.Kind
		if kind == "" {
			kind = BackupItemFile
		}
		if kind != BackupItemFile && kind != BackupItemDirectory {
			return nil, fmt.Errorf("unsupported demo backup item kind %q", kind)
		}
		relative := rule.Path
		if kind == BackupItemFile {
			declaredExtension := backupSaveSuffix(relative)
			if declaredExtension != "" && declaredExtension != extension {
				return nil, errors.New("source file extension does not match the verified server state")
			}
			resolveDirectory := filepath.Ext(relative) == ""
			if restored != nil {
				info, err := restored.Lstat(relative)
				if err != nil {
					if spec.Requirement == BackupItemOptional && errors.Is(err, os.ErrNotExist) {
						continue
					}
					return nil, err
				}
				resolveDirectory = info.IsDir()
			}
			if resolveDirectory {
				filename := server.ID + extension
				if restored != nil {
					entries, err := fs.ReadDir(restored.FS(), relative)
					if err != nil {
						if spec.Requirement == BackupItemOptional && errors.Is(err, os.ErrNotExist) {
							continue
						}
						return nil, err
					}
					var matches []string
					for _, entry := range entries {
						if !entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") && backupSaveSuffix(entry.Name()) == extension {
							matches = append(matches, entry.Name())
						}
					}
					if len(matches) != 1 {
						return nil, fmt.Errorf("expected one %s source in %s, found %d", extension, relative, len(matches))
					}
					filename = matches[0]
				}
				relative = filepath.ToSlash(filepath.Join(relative, filename))
			}
		}
		limit := min(maxItem, maxBundle-total)
		content, err := demoBackupPayload(ctx, restored, relative, kind, server.ID+extension, []byte(fmt.Sprintf("demo world %s\nserver=%s\nsource=%s\n", spec.Name, server.ID, source)), limit)
		if err != nil {
			if spec.Requirement == BackupItemOptional && errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("capture demo item %q: %w", spec.Name, err)
		}
		total += int64(len(content))
		items = append(items, BackupPublicationItem{Name: spec.Name, Kind: kind, SourcePath: relative, Consistency: consistency, Content: bytes.NewReader(content)})
	}
	return items, nil
}

type demoBackupWriter struct {
	buffer bytes.Buffer
	limit  int64
}

func (writer *demoBackupWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > writer.limit-int64(writer.buffer.Len()) {
		return 0, fmt.Errorf("%w: demo capture exceeds %d bytes", ErrBackupQuotaExceeded, writer.limit)
	}
	return writer.buffer.Write(data)
}

func demoBackupPayload(ctx context.Context, restored *os.Root, relative string, kind BackupItemKind, filename string, synthetic []byte, limit int64) ([]byte, error) {
	buffer := &demoBackupWriter{limit: limit}
	var archive *tar.Writer
	if kind == BackupItemDirectory {
		archive = tar.NewWriter(buffer)
	}
	writeFile := func(name string, info fs.FileInfo, reader io.Reader) error {
		if info != nil && !info.Mode().IsRegular() && !info.IsDir() {
			return fmt.Errorf("demo source %q is not a regular file or directory", name)
		}
		if archive != nil {
			header := &tar.Header{Name: name, Mode: 0600, Size: int64(len(synthetic)), Typeflag: tar.TypeReg}
			if info != nil {
				var err error
				header, err = tar.FileInfoHeader(info, "")
				if err != nil {
					return err
				}
				header.Name = name
			}
			if err := archive.WriteHeader(header); err != nil {
				return err
			}
		} else if info != nil && info.IsDir() {
			return errors.New("demo file source is a directory")
		}
		if info != nil && info.IsDir() {
			return nil
		}
		var destination io.Writer = buffer
		if archive != nil {
			destination = archive
		}
		_, err := io.Copy(destination, &contextReader{ctx: ctx, reader: reader})
		return err
	}
	if restored == nil {
		name := relative
		if archive != nil {
			name = filepath.ToSlash(filepath.Join(relative, filename))
		}
		if err := writeFile(name, nil, bytes.NewReader(synthetic)); err != nil {
			return nil, err
		}
	} else {
		capture := func(name string, info fs.FileInfo) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if info.IsDir() {
				return writeFile(name, info, nil)
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("demo source %q is not a regular file", name)
			}
			file, err := restored.Open(name)
			if err != nil {
				return err
			}
			defer file.Close()
			return writeFile(name, info, file)
		}
		info, err := restored.Lstat(relative)
		if err != nil {
			return nil, err
		}
		if archive == nil {
			if err := capture(relative, info); err != nil {
				return nil, err
			}
		} else {
			if !info.IsDir() {
				return nil, errors.New("demo directory source is not a directory")
			}
			if err := fs.WalkDir(restored.FS(), relative, func(name string, entry fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				info, err := entry.Info()
				if err != nil {
					return err
				}
				return capture(name, info)
			}); err != nil {
				return nil, err
			}
		}
	}
	if archive != nil {
		if err := archive.Close(); err != nil {
			return nil, err
		}
	}
	return buffer.buffer.Bytes(), nil
}

func (a *App) collectBackup(ctx context.Context, server Server, definition BackupDefinition, source BackupSource) ([]BackupPublicationItem, func(), error) {
	if a.demo {
		items, err := a.collectDemoBackup(ctx, server, definition, source)
		return items, func() {}, err
	}
	if orchestrator, ok := a.orchestrator.(*kubeOrchestrator); ok {
		if a.backups == nil || a.backups.root == "" {
			return nil, func() {}, errors.New("Kubernetes backup collection requires a persistent backup repository")
		}
		return orchestrator.collectBackup(ctx, server, definition, source, a.backups.root, a.backups.maxItem)
	}
	return nil, func() {}, errors.New("Kubernetes backup collection is unavailable until the server save volume can be verified")
}

func (s *State) recoverBackupSchedules(now time.Time) {
	s.initBackups()
	for id, schedule := range s.BackupSchedules {
		if !schedule.Enabled || schedule.NextRun == nil || schedule.NextRun.After(now) {
			continue
		}
		next, err := nextBackupScheduleRun(schedule, now)
		if err != nil {
			schedule.Enabled = false
			schedule.NextRun, schedule.LastResult, schedule.LastError = nil, BackupRunStatusFailed, "Schedule could not be recalculated during startup recovery: "+err.Error()
		} else {
			schedule.LastResult = BackupRunStatusFailed
			schedule.LastError = "Overdue occurrence skipped during startup recovery; schedules never catch up after downtime"
			schedule.NextRun = cloneTimePtr(&next)
		}
		schedule.Revision++
		s.BackupSchedules[id] = schedule
	}
}

func (a *App) runBackupScheduler(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		a.runDueBackups(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *App) runDueBackups(ctx context.Context) {
	if !a.lifecycleMu.TryLock() {
		return
	}
	defer a.lifecycleMu.Unlock()
	now := a.backupNow()
	snapshot := a.store.Snapshot()
	for _, schedule := range snapshot.BackupSchedules {
		if !schedule.Enabled || schedule.NextRun == nil || schedule.NextRun.After(now) {
			continue
		}
		_, err := a.executeBackup(ctx, backupRunRequest{ServerID: schedule.ServerID, DefinitionID: schedule.DefinitionID, BackendID: schedule.BackendID, ScheduleID: schedule.ID, Actor: "scheduler", IdempotencyKey: "schedule-" + schedule.ID + "-" + schedule.NextRun.Format(time.RFC3339Nano), Acknowledge: true, Scheduled: true})
		if err != nil {
			_ = a.store.Update(func(state *State) error {
				current, exists := state.BackupSchedules[schedule.ID]
				if !exists || !current.Enabled || current.NextRun == nil {
					return nil
				}
				now := a.backupNow()
				current.LastRun, current.LastResult, current.LastError = cloneTimePtr(&now), BackupRunStatusFailed, err.Error()
				next, scheduleErr := nextBackupScheduleRun(current, now)
				if scheduleErr != nil {
					current.Enabled, current.NextRun = false, nil
					current.LastError = "Schedule could not be recalculated after a failed run: " + scheduleErr.Error()
				} else {
					current.NextRun = cloneTimePtr(&next)
				}
				current.Revision++
				state.BackupSchedules[current.ID] = current
				return nil
			})
		}
	}
}

func (a *App) recoverBackupRepository(ctx context.Context) error {
	if a.backups == nil || !a.backups.available || a.backups.repository == nil {
		return nil
	}
	if a.backups.root != "" {
		if err := os.RemoveAll(filepath.Join(a.backups.root, ".capture")); err != nil {
			return fmt.Errorf("remove interrupted backup captures: %w", err)
		}
		entries, err := os.ReadDir(a.backups.root)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".restore-") {
				if err := os.RemoveAll(filepath.Join(a.backups.root, entry.Name())); err != nil {
					return fmt.Errorf("remove interrupted restore capture: %w", err)
				}
			}
		}
	}
	publications, err := a.backups.repository.Recover(ctx)
	if err != nil {
		return err
	}
	backend := a.backupBackend()
	now := a.backupNow()
	return a.store.Update(func(state *State) error {
		state.initBackups()
		state.StorageBackends[backupLocalBackendID] = backend
		published := make(map[string]PublishedBackup, len(publications))
		for _, publication := range publications {
			published[publication.Manifest.ID] = publication
			state.BackupManifests[publication.Manifest.ID] = publication.Manifest
		}
		for index := range state.BackupRuns {
			run := &state.BackupRuns[index]
			publication, exists := published[run.ID]
			if !exists && run.ManifestID != "" {
				publication, exists = published[run.ManifestID]
			}
			if exists {
				delete(published, publication.Manifest.ID)
				if run.Status == BackupRunStatusSucceeded && run.ManifestID == publication.Manifest.ID && run.Bundle == publication.Bundle && run.ItemCount == len(publication.Manifest.Items) && run.Idempotency == publication.Idempotency && run.Error == "" {
					continue
				}
				run.Phase, run.Status, run.Error = BackupRunPublishing, BackupRunStatusSucceeded, ""
				run.ManifestID, run.Bundle, run.ItemCount = publication.Manifest.ID, publication.Bundle, len(publication.Manifest.Items)
				run.Idempotency = publication.Idempotency
			} else if run.Status == BackupRunStatusRunning {
				run.Status, run.Error = BackupRunStatusInterrupted, "Backup interrupted before publication during C2 restart"
			} else {
				continue
			}
			run.UpdatedAt = now
		}
		for _, publication := range publications {
			if _, missing := published[publication.Manifest.ID]; !missing {
				continue
			}
			state.BackupRuns = append(state.BackupRuns, BackupRun{ID: publication.Manifest.ID, ServerID: publication.Manifest.ServerID, DefinitionID: publication.Manifest.DefinitionID, BackendID: backupLocalBackendID, Source: publication.Manifest.Source, Bundle: publication.Bundle, ItemCount: len(publication.Manifest.Items), Phase: BackupRunPublishing, Status: BackupRunStatusSucceeded, ManifestID: publication.Manifest.ID, Idempotency: publication.Idempotency, CreatedAt: publication.Manifest.CreatedAt, UpdatedAt: now})
		}
		return nil
	})
}
