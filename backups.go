package main

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	backupScanInterval   = 15 * time.Second
	backupLateWindow     = 60 * time.Second
	backupRunTimeout     = 20 * time.Minute
	backupCopyTimeout    = 10 * time.Minute
	backupExecTimeout    = 30 * time.Second
	maxBackupSchedules   = 256
	maxBackupRuns        = 500
	maxBackupItemBytes   = 256 << 20
	maxBackupBundleBytes = 512 << 20
	backupSaveDir        = "RSDragonwilds/Saved/SaveGames"
	backupDataMount      = "/home/steam/rsdw-dedicated"
	backupWorldMount     = "/world"
	backupLabel          = "rsdw-c2.petzko.sh/backup"
	backupManifestEntry  = "manifest.json"
	backupSchemaVersion  = 1
)

// The game does not publish .sav.backup atomically. Two listings must agree before a copy starts.
var backupStabilityDelay = 2 * time.Second

type backupResult string

const (
	backupRunning   backupResult = "running"
	backupCompleted backupResult = "completed"
	backupFailedRes backupResult = "failed"
	backupSkipped   backupResult = "skipped"
	backupMissed    backupResult = "missed"
)

type backupSourceMode string

const (
	backupSourceRunningBak backupSourceMode = "running-bak"
	backupSourceStoppedSav backupSourceMode = "stopped-sav"
)

type backupItem struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Checksum string `json:"checksum"`
	StoredAs string `json:"storedAs"`
}

type backupManifest struct {
	SchemaVersion int              `json:"schemaVersion"`
	RunID         string           `json:"runId"`
	ServerID      string           `json:"serverId"`
	ServerName    string           `json:"serverName"`
	WorldName     string           `json:"worldName"`
	DefinitionID  string           `json:"definitionId"`
	SourceMode    backupSourceMode `json:"sourceMode"`
	CollectedAt   time.Time        `json:"collectedAt"`
	Items         []backupItem     `json:"items"`
	TotalBytes    int64            `json:"totalBytes"`
}

type backupRun struct {
	ID           string           `json:"id"`
	ScheduleID   string           `json:"scheduleId,omitempty"`
	OccurrenceID string           `json:"occurrenceId,omitempty"`
	OccurrenceAt *time.Time       `json:"occurrenceAt,omitempty"`
	ServerID     string           `json:"serverId"`
	ServerName   string           `json:"serverName"`
	DefinitionID string           `json:"definitionId"`
	SourceMode   backupSourceMode `json:"sourceMode,omitempty"`
	StartedAt    time.Time        `json:"startedAt"`
	FinishedAt   *time.Time       `json:"finishedAt,omitempty"`
	Result       backupResult     `json:"result"`
	Reason       string           `json:"reason,omitempty"`
	Manifest     *backupManifest  `json:"manifest,omitempty"`
	BundleBytes  int64            `json:"bundleBytes,omitempty"`
}

type backupDefinitionItem struct {
	Name       string           `json:"name"`
	Kind       string           `json:"kind"`
	Path       string           `json:"path"`
	SourceMode backupSourceMode `json:"sourceMode"`
	Required   bool             `json:"required"`
}

type backupDefinition struct {
	ID       string                 `json:"id"`
	Name     string                 `json:"name"`
	Server   string                 `json:"serverType"`
	Strategy string                 `json:"strategy"`
	Items    []backupDefinitionItem `json:"items"`
}

type backupSchedule struct {
	ID               string           `json:"id"`
	Definition       rebootDefinition `json:"definition"`
	DefinitionID     string           `json:"definitionId"`
	Enabled          bool             `json:"enabled"`
	Revision         uint64           `json:"revision"`
	CreatedAt        time.Time        `json:"createdAt"`
	UpdatedAt        time.Time        `json:"updatedAt"`
	IntervalAnchor   *time.Time       `json:"intervalAnchor,omitempty"`
	NextRun          *time.Time       `json:"nextRun,omitempty"`
	LastOccurrenceAt *time.Time       `json:"lastOccurrenceAt,omitempty"`
	LastResult       backupResult     `json:"lastResult,omitempty"`
	LastReason       string           `json:"lastReason,omitempty"`
	LastRunID        string           `json:"lastRunId,omitempty"`
}

// v1 ships exactly one read-only profile. The struct shape leaves room for authored profiles later.
var dragonwildsWorldSave = backupDefinition{
	ID: "dragonwilds-world-save", Name: "Dragonwilds World Save", Server: "dragonwilds", Strategy: "logical-files",
	Items: []backupDefinitionItem{
		{Name: "world-running", Kind: "file", Path: backupSaveDir, SourceMode: backupSourceRunningBak, Required: true},
		{Name: "world-stopped", Kind: "file", Path: backupSaveDir, SourceMode: backupSourceStoppedSav, Required: true},
	},
}

var backupDefinitions = []backupDefinition{dragonwildsWorldSave}

func backupDefinitionByID(id string) (backupDefinition, bool) {
	for _, definition := range backupDefinitions {
		if definition.ID == id {
			return definition, true
		}
	}
	return backupDefinition{}, false
}

var (
	errBackupNotFound     = errors.New("backup schedule not found")
	errBackupRunNotFound  = errors.New("backup run not found")
	errBackupConflict     = errors.New("a backup is already running for this server")
	errBackupInvalid      = errors.New("invalid backup definition")
	errBackupUnavailable  = errors.New("backups require persistent C2 state storage")
	errBackupQuota        = errors.New("Backup repository limit reached; existing backups were preserved")
	errBackupTransitional = errors.New("Server is in a transitional state; backups require a running or stopped server")
	errBackupSourceMoved  = errors.New("Backup source changed while it was being read")
)

type backupDefinitionError struct{ cause error }

func (e backupDefinitionError) Error() string { return e.cause.Error() }
func (e backupDefinitionError) Unwrap() error { return errBackupInvalid }

func (s *State) initBackups() {
	if s.BackupSchedules == nil {
		s.BackupSchedules = map[string]backupSchedule{}
	}
}

func backupRunActive(result backupResult) bool { return result == backupRunning }

func (a *App) backupsEnabled() bool {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("RSDW_BACKUPS_ENABLED")), "false") {
		return false
	}
	if a.store == nil || a.store.path == "" {
		return false
	}
	if a.demo {
		return true
	}
	return !strings.EqualFold(strings.TrimSpace(os.Getenv("RSDW_STATE_PERSISTENT")), "false")
}

// backupRepository owns every path under the backup volume. os.Root makes traversal impossible.
type backupRepository struct {
	root  *os.Root
	limit int64
}

func backupRepositoryLimit() int64 {
	raw := strings.TrimSpace(os.Getenv("RSDW_BACKUP_MAX_BYTES"))
	if raw == "" {
		return maxBackupBundleBytes * 8
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		return maxBackupBundleBytes * 8
	}
	return value
}

func (a *App) backupRepo() (*backupRepository, error) {
	if !a.backupsEnabled() {
		return nil, errBackupUnavailable
	}
	dir := strings.TrimSpace(os.Getenv("RSDW_BACKUP_DIR"))
	if dir == "" {
		dir = filepath.Join(filepath.Dir(a.store.path), "backups")
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return nil, err
	}
	parent, err := os.OpenRoot(filepath.Dir(dir))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	name := filepath.Base(dir)
	if err := parent.Mkdir(name, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	info, err := parent.Lstat(name)
	if err != nil || !info.IsDir() {
		return nil, errors.New("backup repository must be a directory, not a symlink")
	}
	root, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	return &backupRepository{root: root, limit: backupRepositoryLimit()}, nil
}

func (r *backupRepository) close() {
	if r != nil && r.root != nil {
		r.root.Close()
	}
}

func (r *backupRepository) bundleName(runID string) string  { return runID + ".tar" }
func (r *backupRepository) stagingName(runID string) string { return runID + ".item" }

// stage opens a private file for a run's working bytes. It never overwrites a published bundle.
func (r *backupRepository) stage(runID string) (*os.File, error) {
	return r.root.OpenFile(r.stagingName(runID), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
}

func (r *backupRepository) stagingPath(runID string) string {
	return filepath.Join(r.root.Name(), r.stagingName(runID))
}

// publish is the atomicity boundary. A run is only completed once this returns nil.
func (r *backupRepository) publish(runID string, manifest backupManifest) (int64, error) {
	// The staging file holds exactly one collected item; more would need one staging name each.
	if len(manifest.Items) != 1 {
		return 0, fmt.Errorf("a backup bundle carries exactly one item, got %d", len(manifest.Items))
	}
	if manifest.TotalBytes > maxBackupBundleBytes {
		return 0, fmt.Errorf("backup bundle would exceed %d bytes", int64(maxBackupBundleBytes))
	}
	metadata, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return 0, err
	}
	file, err := r.root.OpenFile(runID+".tar.tmp", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	writer := tar.NewWriter(file)
	if err := writeBundleEntry(writer, backupManifestEntry, int64(len(metadata)), strings.NewReader(string(metadata))); err != nil {
		file.Close()
		return 0, err
	}
	item := manifest.Items[0]
	source, err := r.root.Open(r.stagingName(runID))
	if err != nil {
		file.Close()
		return 0, err
	}
	err = writeBundleEntry(writer, item.StoredAs, item.Size, source)
	source.Close()
	if err != nil {
		file.Close()
		return 0, err
	}
	if err := writer.Close(); err != nil {
		file.Close()
		return 0, err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return 0, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return 0, err
	}
	if err := file.Close(); err != nil {
		return 0, err
	}
	if err := r.root.Rename(runID+".tar.tmp", r.bundleName(runID)); err != nil {
		return 0, err
	}
	if err := r.writeAtomic(runID+".json", metadata); err != nil {
		return 0, err
	}
	r.root.Remove(r.stagingName(runID))
	return info.Size(), nil
}

func writeBundleEntry(writer *tar.Writer, name string, size int64, source io.Reader) error {
	if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: size, Format: tar.FormatPAX}); err != nil {
		return err
	}
	written, err := io.Copy(writer, io.LimitReader(source, size))
	if err != nil {
		return err
	}
	if written != size {
		return fmt.Errorf("bundle entry %q changed size while it was written", name)
	}
	return nil
}

func (r *backupRepository) writeAtomic(name string, data []byte) error {
	file, err := r.root.OpenFile(name+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return r.root.Rename(name+".tmp", name)
}

// discard removes only working files. A published bundle is never touched.
func (r *backupRepository) discard(runID string) {
	for _, name := range []string{runID + ".tar.tmp", runID + ".json.tmp", r.stagingName(runID), runID + ".pod.json"} {
		r.root.Remove(name)
	}
}

func (r *backupRepository) open(runID string) (*os.File, int64, error) {
	info, err := r.root.Lstat(r.bundleName(runID))
	if err != nil || !info.Mode().IsRegular() {
		return nil, 0, errBackupRunNotFound
	}
	file, err := r.root.Open(r.bundleName(runID))
	if err != nil {
		return nil, 0, errBackupRunNotFound
	}
	return file, info.Size(), nil
}

func (r *backupRepository) exists(runID string) bool {
	info, err := r.root.Lstat(r.bundleName(runID))
	return err == nil && info.Mode().IsRegular()
}

func (r *backupRepository) remove(runID string) error {
	r.discard(runID)
	r.root.Remove(runID + ".json")
	if err := r.root.Remove(r.bundleName(runID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (r *backupRepository) usage() (int64, error) {
	entries, err := fs.ReadDir(r.root.FS(), ".")
	if err != nil {
		return 0, err
	}
	var total int64
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		total += info.Size()
	}
	return total, nil
}

// admit refuses a run before any byte is copied. Old bundles are never deleted to make room.
func (r *backupRepository) admit(size int64) error {
	if size > maxBackupItemBytes {
		return fmt.Errorf("Backup source is larger than the %d MiB item limit", int64(maxBackupItemBytes)>>20)
	}
	used, err := r.usage()
	if err != nil {
		return err
	}
	if used+size > r.limit {
		return errBackupQuota
	}
	return nil
}

// recoverBackupRepository clears working files left by a process that died mid-run.
func (a *App) recoverBackupRepository() {
	repo, err := a.backupRepo()
	if err != nil {
		return
	}
	defer repo.close()
	entries, err := fs.ReadDir(repo.root.FS(), ".")
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasSuffix(name, ".tar.tmp") || strings.HasSuffix(name, ".json.tmp") || strings.HasSuffix(name, ".item") || strings.HasSuffix(name, ".pod.json") {
			repo.root.Remove(name)
		}
	}
}

func (s *State) recoverBackups(now time.Time) {
	s.initBackups()
	for index := range s.BackupRuns {
		if !backupRunActive(s.BackupRuns[index].Result) {
			continue
		}
		finished := now.UTC()
		s.BackupRuns[index].Result = backupFailedRes
		s.BackupRuns[index].Reason = "C2 restarted while this backup was running"
		s.BackupRuns[index].FinishedAt = &finished
		updateBackupScheduleResult(s, s.BackupRuns[index])
	}
	ids := make([]string, 0, len(s.BackupSchedules))
	for id := range s.BackupSchedules {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		schedule := s.BackupSchedules[id]
		if !schedule.Enabled || schedule.NextRun == nil || schedule.NextRun.After(now) {
			continue
		}
		missedAt := *schedule.NextRun
		next, err := nextBackupRun(schedule, now)
		if err != nil {
			schedule.Enabled, schedule.NextRun = false, nil
			schedule.LastResult = backupSkipped
			schedule.LastReason = "Schedule could not be recalculated during startup recovery: " + err.Error()
			s.BackupSchedules[id] = schedule
			continue
		}
		recordBackupOccurrence(s, schedule, missedAt, backupMissed, "C2 was unavailable; the overdue occurrence was skipped during startup recovery", now)
		schedule.LastResult = backupMissed
		schedule.LastReason = "Skipped during startup recovery; schedules never catch up after downtime"
		schedule.LastRunID = ""
		schedule.LastOccurrenceAt = cloneTimePtr(&missedAt)
		schedule.NextRun = cloneTimePtr(&next)
		schedule.UpdatedAt = now
		s.BackupSchedules[id] = schedule
	}
}

// nextBackupRun reuses the reboot timing math so DST, cron, and interval rules stay identical.
func nextBackupRun(schedule backupSchedule, after time.Time) (time.Time, error) {
	return nextScheduleRun(rebootSchedule{Definition: schedule.Definition, IntervalAnchor: schedule.IntervalAnchor}, after)
}

func recordBackupOccurrence(state *State, schedule backupSchedule, occurrence time.Time, result backupResult, reason string, at time.Time) backupRun {
	id := randomID()
	server := state.Servers[schedule.Definition.ServerID]
	finished := at.UTC()
	occurrenceAt := occurrence.UTC()
	run := backupRun{
		ID: id, ScheduleID: schedule.ID, OccurrenceID: id, OccurrenceAt: &occurrenceAt,
		ServerID: schedule.Definition.ServerID, ServerName: worldLabel(server), DefinitionID: schedule.DefinitionID,
		StartedAt: at.UTC(), FinishedAt: &finished, Result: result, Reason: reason,
	}
	state.BackupRuns = append(state.BackupRuns, run)
	trimBackupRuns(state)
	return run
}

func trimBackupRuns(state *State) {
	for len(state.BackupRuns) > maxBackupRuns {
		index := -1
		for i, run := range state.BackupRuns {
			if !backupRunActive(run.Result) {
				index = i
				break
			}
		}
		if index < 0 {
			return
		}
		copy(state.BackupRuns[index:], state.BackupRuns[index+1:])
		state.BackupRuns = state.BackupRuns[:len(state.BackupRuns)-1]
	}
}

func updateBackupScheduleResult(state *State, run backupRun) {
	schedule, ok := state.BackupSchedules[run.ScheduleID]
	if !ok {
		return
	}
	if run.OccurrenceAt == nil || schedule.LastOccurrenceAt == nil || !run.OccurrenceAt.Before(*schedule.LastOccurrenceAt) {
		schedule.LastResult, schedule.LastReason, schedule.LastRunID = run.Result, run.Reason, run.ID
		if run.OccurrenceAt != nil {
			schedule.LastOccurrenceAt = cloneTimePtr(run.OccurrenceAt)
		}
		state.BackupSchedules[run.ScheduleID] = schedule
	}
}

func recordAndAdvanceBackup(state *State, schedule backupSchedule, result backupResult, reason string, now time.Time) {
	if schedule.NextRun == nil {
		return
	}
	run := recordBackupOccurrence(state, schedule, *schedule.NextRun, result, reason, now)
	schedule.LastOccurrenceAt = cloneTimePtr(run.OccurrenceAt)
	schedule.LastResult, schedule.LastReason, schedule.LastRunID = result, reason, ""
	advanceBackupSchedule(state, &schedule, now)
}

func advanceBackupSchedule(state *State, schedule *backupSchedule, now time.Time) {
	if !schedule.Enabled {
		schedule.NextRun = nil
		schedule.UpdatedAt = now
		state.BackupSchedules[schedule.ID] = *schedule
		return
	}
	next, err := nextBackupRun(*schedule, now)
	if err != nil {
		schedule.Enabled, schedule.NextRun = false, nil
		schedule.LastResult, schedule.LastReason = backupSkipped, "No future occurrence could be calculated; schedule disabled: "+err.Error()
	} else {
		schedule.NextRun = cloneTimePtr(&next)
	}
	schedule.UpdatedAt = now
	state.BackupSchedules[schedule.ID] = *schedule
}

func backupRunningFor(state *State, serverID string) bool {
	for _, run := range state.BackupRuns {
		if run.ServerID == serverID && backupRunActive(run.Result) {
			return true
		}
	}
	return false
}

// reserveBackupRun claims one run per server and persists it before any Kubernetes call.
func reserveBackupRun(state *State, serverID, definitionID, scheduleID, occurrenceID string, occurrenceAt *time.Time, now time.Time) (backupRun, error) {
	server, ok := state.Servers[serverID]
	if !ok {
		return backupRun{}, errBackupNotFound
	}
	if state.deleting(serverID) {
		return backupRun{}, errors.New("Target server deletion is in progress or recorded")
	}
	if backupRunningFor(state, serverID) {
		return backupRun{}, errBackupConflict
	}
	run := backupRun{
		ID: randomID(), ScheduleID: scheduleID, OccurrenceID: occurrenceID, OccurrenceAt: cloneTimePtr(occurrenceAt),
		ServerID: serverID, ServerName: worldLabel(server), DefinitionID: definitionID,
		StartedAt: now.UTC(), Result: backupRunning, Reason: "Backup collection started",
	}
	state.BackupRuns = append(state.BackupRuns, run)
	trimBackupRuns(state)
	emitAlert(state, server, BackupStarted, run.ID, "Backup collection started for "+worldLabel(server), run.StartedAt)
	return run, nil
}

func (a *App) startBackupRun(serverID, definitionID string) (backupRun, Server, error) {
	var run backupRun
	var server Server
	err := a.store.Update(func(state *State) error {
		state.initBackups()
		var err error
		run, err = reserveBackupRun(state, serverID, definitionID, "", "", nil, a.rebootNow())
		if err != nil {
			return err
		}
		server = state.Servers[serverID]
		return nil
	})
	return run, server, err
}

func (a *App) finishBackupRun(runID string, result backupResult, reason string, manifest *backupManifest, bundleBytes int64) {
	if err := a.store.Update(func(state *State) error {
		state.initBackups()
		now := a.rebootNow()
		for index := range state.BackupRuns {
			run := &state.BackupRuns[index]
			if run.ID != runID || !backupRunActive(run.Result) {
				continue
			}
			finished := now.UTC()
			run.Result, run.Reason, run.FinishedAt, run.BundleBytes = result, reason, &finished, bundleBytes
			if manifest != nil {
				run.Manifest, run.SourceMode = manifest, manifest.SourceMode
			}
			kind := BackupCompleted
			if result != backupCompleted {
				kind = BackupFailed
			}
			emitAlert(state, state.Servers[run.ServerID], kind, run.ID, reason, finished)
			updateBackupScheduleResult(state, *run)
			if run.ScheduleID != "" {
				if schedule, ok := state.BackupSchedules[run.ScheduleID]; ok {
					schedule.LastRunID = run.ID
					state.BackupSchedules[run.ScheduleID] = schedule
				}
			}
			break
		}
		return nil
	}); err != nil {
		log.Printf("backup run %s result: %v", runID, err)
	}
}

// executeBackupRun holds no store lock while it touches Kubernetes or disk.
func (a *App) executeBackupRun(run backupRun, server Server) {
	if a.demo {
		a.finishDemoBackup(run, server)
		return
	}
	k, ok := a.orchestrator.(*kubeOrchestrator)
	if !ok {
		a.finishBackupRun(run.ID, backupFailedRes, "Backups require Kubernetes storage", nil, 0)
		return
	}
	repo, err := a.backupRepo()
	if err != nil {
		a.finishBackupRun(run.ID, backupFailedRes, errBackupUnavailable.Error(), nil, 0)
		return
	}
	defer repo.close()
	defer repo.discard(run.ID)
	ctx, cancel := context.WithTimeout(context.Background(), backupRunTimeout)
	defer cancel()
	mode, err := k.backupSourceMode(ctx, server)
	if err != nil {
		result := backupSkipped
		if !errors.Is(err, errBackupTransitional) {
			result = backupFailedRes
		}
		a.finishBackupRun(run.ID, result, err.Error(), nil, 0)
		return
	}
	item, err := k.collectBackup(ctx, server, mode, repo, run.ID)
	if err != nil {
		a.finishBackupRun(run.ID, backupFailedRes, err.Error(), nil, 0)
		return
	}
	manifest := backupManifest{
		SchemaVersion: backupSchemaVersion, RunID: run.ID, ServerID: server.ID, ServerName: worldLabel(server),
		WorldName: server.WorldName, DefinitionID: run.DefinitionID, SourceMode: mode,
		CollectedAt: a.rebootNow().UTC(), Items: []backupItem{item}, TotalBytes: item.Size,
	}
	bundleBytes, err := repo.publish(run.ID, manifest)
	if err != nil {
		a.finishBackupRun(run.ID, backupFailedRes, "Backup bundle could not be published: "+err.Error(), nil, 0)
		return
	}
	a.finishBackupRun(run.ID, backupCompleted, fmt.Sprintf("Collected %s from a %s server", path.Base(item.Path), backupSourceLabel(mode)), &manifest, bundleBytes)
}

func backupSourceLabel(mode backupSourceMode) string {
	if mode == backupSourceStoppedSav {
		return "stopped"
	}
	return "running"
}

func (a *App) finishDemoBackup(run backupRun, server Server) {
	content := fmt.Sprintf("demo world save for %s\n", worldLabel(server))
	sum := sha256.Sum256([]byte(content))
	item := backupItem{
		Name: "world-running", Path: backupDataMount + "/" + backupSaveDir + "/" + worldSaveBase(server) + ".sav",
		Size: int64(len(content)), Checksum: "sha256:" + hex.EncodeToString(sum[:]), StoredAs: "items/world-running.sav",
	}
	manifest := backupManifest{
		SchemaVersion: backupSchemaVersion, RunID: run.ID, ServerID: server.ID, ServerName: worldLabel(server),
		WorldName: server.WorldName, DefinitionID: run.DefinitionID, SourceMode: backupSourceRunningBak,
		CollectedAt: a.rebootNow().UTC(), Items: []backupItem{item}, TotalBytes: item.Size,
	}
	var bundleBytes int64
	if repo, err := a.backupRepo(); err == nil {
		defer repo.close()
		if staged, err := repo.stage(run.ID); err == nil {
			_, writeErr := staged.WriteString(content)
			staged.Close()
			if writeErr == nil {
				if size, err := repo.publish(run.ID, manifest); err == nil {
					bundleBytes = size
				}
			}
		}
		repo.discard(run.ID)
	}
	a.finishBackupRun(run.ID, backupCompleted, "Demo simulated backup; no Kubernetes operation", &manifest, bundleBytes)
}

func worldSaveBase(server Server) string {
	return defaultValue(slugify(worldLabel(server)), "World")
}

func (a *App) runBackupScheduler(ctx context.Context) {
	ticker := time.NewTicker(backupScanInterval)
	defer ticker.Stop()
	for {
		a.scanBackups(ctx, a.rebootNow())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *App) scanBackups(ctx context.Context, now time.Time) {
	if !a.backupsEnabled() {
		return
	}
	snapshot := a.store.Snapshot()
	groups := map[string][]string{}
	for id, schedule := range snapshot.BackupSchedules {
		if schedule.Enabled && schedule.NextRun != nil && !schedule.NextRun.After(now) {
			groups[schedule.Definition.ServerID] = append(groups[schedule.Definition.ServerID], id)
		}
	}
	serverIDs := make([]string, 0, len(groups))
	for serverID := range groups {
		serverIDs = append(serverIDs, serverID)
	}
	sort.Strings(serverIDs)
	for _, serverID := range serverIDs {
		if ctx.Err() != nil {
			return
		}
		sort.Strings(groups[serverID])
		run, server, err := a.claimScheduledBackups(serverID, groups[serverID])
		if err != nil {
			log.Printf("scheduled backup claim for %s: %v", serverID, err)
			continue
		}
		if run.ID == "" {
			continue
		}
		go a.executeBackupRun(run, server)
	}
}

// claimScheduledBackups advances every due cursor inside the same write that claims the run.
func (a *App) claimScheduledBackups(serverID string, ids []string) (backupRun, Server, error) {
	var claimed backupRun
	var server Server
	err := a.store.Update(func(state *State) error {
		state.initBackups()
		claimNow := a.rebootNow()
		due := make([]backupSchedule, 0, len(ids))
		for _, id := range ids {
			schedule, exists := state.BackupSchedules[id]
			if !exists || !schedule.Enabled || schedule.NextRun == nil || schedule.NextRun.After(claimNow) {
				continue
			}
			if claimNow.Sub(*schedule.NextRun) > backupLateWindow {
				recordAndAdvanceBackup(state, schedule, backupMissed, "Occurrence was more than 60 seconds late; catch-up is disabled", claimNow)
				continue
			}
			if _, err := parseRebootTiming(schedule.Definition); err != nil {
				schedule.Enabled, schedule.NextRun = false, nil
				schedule.LastResult, schedule.LastReason = backupSkipped, "Schedule became invalid and was disabled: "+err.Error()
				state.BackupSchedules[schedule.ID] = schedule
				continue
			}
			due = append(due, schedule)
		}
		if len(due) == 0 {
			return nil
		}
		// A stopped server is a valid stopped-sav source, so only deletion and absence skip a backup.
		if skip, reason := backupSkipReason(state, serverID); skip {
			for _, schedule := range due {
				recordAndAdvanceBackup(state, schedule, backupSkipped, reason, claimNow)
			}
			return nil
		}
		if backupRunningFor(state, serverID) {
			for _, schedule := range due {
				recordAndAdvanceBackup(state, schedule, backupSkipped, errBackupConflict.Error(), claimNow)
			}
			return nil
		}
		first := due[0]
		run, err := reserveBackupRun(state, serverID, first.DefinitionID, first.ID, first.ID, first.NextRun, claimNow)
		if err != nil {
			for _, schedule := range due {
				recordAndAdvanceBackup(state, schedule, backupSkipped, err.Error(), claimNow)
			}
			return nil
		}
		claimed, server = run, state.Servers[serverID]
		first.LastOccurrenceAt = cloneTimePtr(first.NextRun)
		first.LastResult, first.LastReason, first.LastRunID = backupRunning, run.Reason, run.ID
		advanceBackupSchedule(state, &first, claimNow)
		for _, schedule := range due[1:] {
			recordAndAdvanceBackup(state, schedule, backupSkipped, errBackupConflict.Error(), claimNow)
		}
		return nil
	})
	return claimed, server, err
}

func backupSkipReason(state *State, serverID string) (bool, string) {
	if state.deleting(serverID) {
		return true, "Target server deletion is in progress or recorded"
	}
	if _, ok := state.Servers[serverID]; !ok {
		return true, "Target server no longer exists"
	}
	return false, ""
}

// backupSourceMode decides from verified server state. It never falls back to the other source.
func (k *kubeOrchestrator) backupSourceMode(ctx context.Context, server Server) (backupSourceMode, error) {
	_, status, err := k.resolvePod(ctx, server)
	switch {
	case errors.Is(err, errScaledZero):
		return backupSourceStoppedSav, nil
	case err != nil:
		return "", errBackupTransitional
	case status == StatusOnline:
		return backupSourceRunningBak, nil
	default:
		return "", errBackupTransitional
	}
}

type backupCandidate struct {
	name  string
	size  int64
	mtime int64
}

func parseBackupListing(data []byte) ([]backupCandidate, error) {
	candidates := []backupCandidate{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, " ", 3)
		if len(fields) != 3 {
			return nil, fmt.Errorf("unreadable save listing entry %q", line)
		}
		size, sizeErr := strconv.ParseInt(fields[0], 10, 64)
		mtime, timeErr := strconv.ParseInt(fields[1], 10, 64)
		if sizeErr != nil || timeErr != nil {
			return nil, fmt.Errorf("unreadable save listing entry %q", line)
		}
		candidates = append(candidates, backupCandidate{name: fields[2], size: size, mtime: mtime})
	}
	return candidates, nil
}

const backupListingScript = `cd "$1" && for f in *"$2"; do [ -f "$f" ] || continue; [ -L "$f" ] && continue; stat -c '%s %Y %n' -- "$f"; done`

type backupExecutor func(ctx context.Context, args ...string) ([]byte, error)

func listBackupCandidates(ctx context.Context, exec backupExecutor, dir, suffix string) ([]backupCandidate, error) {
	data, err := exec(ctx, "sh", "-ec", backupListingScript, "backup", dir, suffix)
	if err != nil {
		return nil, fmt.Errorf("could not list %s files in the world volume: %w", suffix, err)
	}
	return parseBackupListing(data)
}

func selectBackupCandidate(candidates []backupCandidate, mode backupSourceMode) (backupCandidate, error) {
	switch len(candidates) {
	case 1:
		return candidates[0], nil
	case 0:
		if mode == backupSourceRunningBak {
			return backupCandidate{}, errors.New("Running server has no game-generated .sav.backup to collect")
		}
		return backupCandidate{}, errors.New("Stopped server has no .sav file to collect")
	default:
		names := make([]string, 0, len(candidates))
		for _, candidate := range candidates {
			names = append(names, candidate.name)
		}
		sort.Strings(names)
		return backupCandidate{}, fmt.Errorf("World volume holds %d candidate saves (%s); C2 never guesses which world to back up", len(candidates), strings.Join(names, ", "))
	}
}

func backupSuffix(mode backupSourceMode) string {
	if mode == backupSourceStoppedSav {
		return ".sav"
	}
	return ".sav.backup"
}

// collectBackup copies one verified save out of the world volume and checksums it on the way in.
func (k *kubeOrchestrator) collectBackup(ctx context.Context, server Server, mode backupSourceMode, repo *backupRepository, runID string) (backupItem, error) {
	if mode == backupSourceRunningBak {
		target, status, err := k.resolvePod(ctx, server)
		if err != nil || status != StatusOnline {
			return backupItem{}, errBackupTransitional
		}
		exec := func(ctx context.Context, args ...string) ([]byte, error) { return k.podExec(ctx, target, args...) }
		copyOut := func(ctx context.Context, remote, local string) error {
			_, err := k.runner.Run(ctx, k.kubectl, "-n", target.pod.Metadata.Namespace, "cp", "-c", "server", target.pod.Metadata.Name+":"+remote, local)
			return err
		}
		return k.copyBackupItem(ctx, mode, exec, copyOut, backupDataMount+"/"+backupSaveDir, repo, runID)
	}
	claim, err := k.worldClaim(ctx, server)
	if err != nil {
		return backupItem{}, err
	}
	inspector := "rsdw-backup-" + runID
	if err := k.startInspector(ctx, server.Namespace, inspector, claim, true, repo, runID); err != nil {
		k.deleteInspector(server.Namespace, inspector)
		return backupItem{}, err
	}
	// A cancelled request context must still remove the inspector Pod.
	defer k.deleteInspector(server.Namespace, inspector)
	exec := func(ctx context.Context, args ...string) ([]byte, error) {
		return k.inspectorExec(ctx, server.Namespace, inspector, args...)
	}
	copyOut := func(ctx context.Context, remote, local string) error {
		_, err := k.runner.Run(ctx, k.kubectl, "-n", server.Namespace, "cp", "-c", "reader", inspector+":"+remote, local)
		return err
	}
	return k.copyBackupItem(ctx, mode, exec, copyOut, backupWorldMount+"/"+backupSaveDir, repo, runID)
}

func (k *kubeOrchestrator) copyBackupItem(ctx context.Context, mode backupSourceMode, exec backupExecutor, copyOut func(context.Context, string, string) error, dir string, repo *backupRepository, runID string) (backupItem, error) {
	suffix := backupSuffix(mode)
	candidates, err := listBackupCandidates(ctx, exec, dir, suffix)
	if err != nil {
		return backupItem{}, err
	}
	chosen, err := selectBackupCandidate(candidates, mode)
	if err != nil {
		return backupItem{}, err
	}
	if err := repo.admit(chosen.size); err != nil {
		return backupItem{}, err
	}
	select {
	case <-ctx.Done():
		return backupItem{}, ctx.Err()
	case <-time.After(backupStabilityDelay):
	}
	confirmations, err := listBackupCandidates(ctx, exec, dir, suffix)
	if err != nil {
		return backupItem{}, err
	}
	stable := false
	for _, candidate := range confirmations {
		if candidate.name == chosen.name && candidate.size == chosen.size && candidate.mtime == chosen.mtime {
			stable = true
		}
	}
	if !stable {
		return backupItem{}, errBackupSourceMoved
	}
	staged, err := repo.stage(runID)
	if err != nil {
		return backupItem{}, err
	}
	staged.Close()
	copyCtx, cancel := context.WithTimeout(ctx, backupCopyTimeout)
	defer cancel()
	if err := copyOut(copyCtx, dir+"/"+chosen.name, repo.stagingPath(runID)); err != nil {
		return backupItem{}, fmt.Errorf("could not copy the save out of the world volume: %w", err)
	}
	return backupItemFromStaging(repo, runID, mode, dir+"/"+chosen.name, chosen)
}

func backupItemFromStaging(repo *backupRepository, runID string, mode backupSourceMode, remote string, chosen backupCandidate) (backupItem, error) {
	file, err := repo.root.Open(repo.stagingName(runID))
	if err != nil {
		return backupItem{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return backupItem{}, err
	}
	if info.Size() > maxBackupItemBytes {
		return backupItem{}, fmt.Errorf("Backup source is larger than the %d MiB item limit", int64(maxBackupItemBytes)>>20)
	}
	if info.Size() != chosen.size {
		return backupItem{}, errBackupSourceMoved
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return backupItem{}, err
	}
	name := "world-running"
	if mode == backupSourceStoppedSav {
		name = "world-stopped"
	}
	return backupItem{
		Name: name, Path: remote, Size: info.Size(),
		Checksum: "sha256:" + hex.EncodeToString(digest.Sum(nil)),
		StoredAs: "items/" + name + backupSuffix(mode),
	}, nil
}

// worldClaim reads the Deployment rather than reconstructing the claim name from the release.
func (k *kubeOrchestrator) worldClaim(ctx context.Context, server Server) (string, error) {
	var deployment struct {
		Spec struct {
			Template struct {
				Spec struct {
					Volumes []struct {
						Name                  string `json:"name"`
						PersistentVolumeClaim *struct {
							ClaimName string `json:"claimName"`
						} `json:"persistentVolumeClaim"`
					} `json:"volumes"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := k.kubeJSON(ctx, &deployment, "-n", server.Namespace, "get", "deployment", deploymentName(server.Release), "-o", "json"); err != nil {
		return "", fmt.Errorf("could not read the world Deployment: %w", err)
	}
	claims := []string{}
	for _, volume := range deployment.Spec.Template.Spec.Volumes {
		if volume.Name == "data" && volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName != "" {
			claims = append(claims, volume.PersistentVolumeClaim.ClaimName)
		}
	}
	if len(claims) != 1 {
		return "", fmt.Errorf("expected exactly one world volume on the Deployment, found %d", len(claims))
	}
	return claims[0], nil
}

func inspectorManifest(namespace, name, claim string, readOnly bool) map[string]any {
	return map[string]any{"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": name, "namespace": namespace, "labels": map[string]string{backupLabel: name}},
		"spec": map[string]any{
			"automountServiceAccountToken": false, "restartPolicy": "Never", "activeDeadlineSeconds": 180,
			"terminationGracePeriodSeconds": 5,
			"securityContext":               map[string]any{"runAsUser": 1000, "runAsGroup": 1000, "runAsNonRoot": true, "fsGroup": 1000, "seccompProfile": map[string]string{"type": "RuntimeDefault"}},
			"containers": []any{map[string]any{
				"name": "reader", "image": envOr("RSDW_SEED_WRITER_IMAGE", "busybox:1.37.0"), "command": []string{"sleep", "180"},
				"securityContext": map[string]any{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": map[string]any{"drop": []string{"ALL"}}},
				"resources":       map[string]any{"requests": map[string]string{"cpu": "10m", "memory": "8Mi"}, "limits": map[string]string{"cpu": "100m", "memory": "64Mi"}},
				"volumeMounts":    []any{map[string]any{"name": "world", "mountPath": backupWorldMount, "readOnly": readOnly}},
			}},
			"volumes": []any{map[string]any{"name": "world", "persistentVolumeClaim": map[string]any{"claimName": claim, "readOnly": readOnly}}},
		}}
}

func (k *kubeOrchestrator) startInspector(ctx context.Context, namespace, name, claim string, readOnly bool, repo *backupRepository, runID string) error {
	data, err := json.Marshal(inspectorManifest(namespace, name, claim, readOnly))
	if err != nil {
		return err
	}
	if err := repo.writeAtomic(runID+".pod.json", data); err != nil {
		return err
	}
	if _, err := k.runner.Run(ctx, k.kubectl, "create", "-f", filepath.Join(repo.root.Name(), runID+".pod.json")); err != nil {
		return fmt.Errorf("could not start the world inspector Pod: %w", err)
	}
	if _, err := k.runner.Run(ctx, k.kubectl, "-n", namespace, "wait", "--for=condition=Ready", "pod/"+name, "--timeout=90s"); err != nil {
		return fmt.Errorf("the world inspector Pod never became ready: %w", err)
	}
	return nil
}

func (k *kubeOrchestrator) inspectorExec(ctx context.Context, namespace, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, backupExecTimeout)
	defer cancel()
	base := []string{"-n", namespace, "exec", "pod/" + name, "-c", "reader", "--"}
	return k.runner.Run(ctx, k.kubectl, append(base, args...)...)
}

func (k *kubeOrchestrator) deleteInspector(namespace, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := k.runner.Run(ctx, k.kubectl, "-n", namespace, "delete", "pod", name, "--ignore-not-found", "--wait=true", "--timeout=30s"); err != nil {
		log.Printf("delete inspector pod %s/%s: %v", namespace, name, err)
	}
}

type backupScheduleView struct {
	ID                string       `json:"id"`
	ServerID          string       `json:"serverId"`
	ServerName        string       `json:"serverName"`
	DefinitionID      string       `json:"definitionId"`
	Mode              rebootMode   `json:"mode"`
	Cron              string       `json:"cron,omitempty"`
	IntervalValue     int          `json:"intervalValue,omitempty"`
	IntervalUnit      string       `json:"intervalUnit,omitempty"`
	DailyTimes        []string     `json:"dailyTimes,omitempty"`
	ExecutionTimezone string       `json:"executionTimezone"`
	Enabled           bool         `json:"enabled"`
	Revision          uint64       `json:"revision"`
	CreatedAt         time.Time    `json:"createdAt"`
	UpdatedAt         time.Time    `json:"updatedAt"`
	NextRun           *time.Time   `json:"nextRun,omitempty"`
	LastOccurrenceAt  *time.Time   `json:"lastOccurrenceAt,omitempty"`
	LastResult        backupResult `json:"lastResult,omitempty"`
	LastReason        string       `json:"lastReason,omitempty"`
	LastRunID         string       `json:"lastRunId,omitempty"`
}

type backupRunView struct {
	backupRun
	Downloadable bool `json:"downloadable"`
}

func backupScheduleViewOf(schedule backupSchedule, servers map[string]Server) backupScheduleView {
	view := backupScheduleView{
		ID: schedule.ID, ServerID: schedule.Definition.ServerID, DefinitionID: schedule.DefinitionID,
		Mode: schedule.Definition.Mode, Cron: schedule.Definition.Cron, IntervalValue: schedule.Definition.IntervalValue,
		IntervalUnit: schedule.Definition.IntervalUnit, DailyTimes: append([]string(nil), schedule.Definition.DailyTimes...),
		ExecutionTimezone: schedule.Definition.ExecutionTimezone, Enabled: schedule.Enabled, Revision: schedule.Revision,
		CreatedAt: schedule.CreatedAt, UpdatedAt: schedule.UpdatedAt, NextRun: cloneTimePtr(schedule.NextRun),
		LastOccurrenceAt: cloneTimePtr(schedule.LastOccurrenceAt), LastResult: schedule.LastResult,
		LastReason: schedule.LastReason, LastRunID: schedule.LastRunID,
	}
	if server, ok := servers[view.ServerID]; ok {
		view.ServerName = worldLabel(server)
	}
	return view
}

type backupRequest struct {
	rebootRequest
	DefinitionID string `json:"definitionId"`
}

func normalizeBackupRequest(request backupRequest) (rebootDefinition, string, bool, error) {
	definition, enabled, err := normalizeRebootRequest(request.rebootRequest, true)
	if err != nil {
		return rebootDefinition{}, "", false, err
	}
	if definition.WarningMinutes != 0 {
		return rebootDefinition{}, "", false, errors.New("warningMinutes does not apply to backup schedules")
	}
	definitionID := strings.TrimSpace(request.DefinitionID)
	if definitionID == "" {
		definitionID = dragonwildsWorldSave.ID
	}
	if _, ok := backupDefinitionByID(definitionID); !ok {
		return rebootDefinition{}, "", false, errors.New("definitionId must name a built-in backup profile")
	}
	return definition, definitionID, enabled, nil
}

func (a *App) handleBackups(w http.ResponseWriter, r *http.Request) {
	if requestPrincipal(r).Role != RoleAdmin {
		writeError(w, http.StatusForbidden, "permission denied")
		return
	}
	route := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/backups"), "/")
	parts := []string{}
	if route != "" {
		parts = strings.Split(route, "/")
	}
	switch {
	case len(parts) == 0:
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		a.listBackups(w)
	case parts[0] == "runs" && len(parts) == 1:
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		a.createBackupRun(w, r)
	case parts[0] == "runs" && len(parts) == 2 && r.Method == http.MethodDelete:
		a.deleteBackupRun(w, parts[1])
	case parts[0] == "runs" && len(parts) == 3 && parts[2] == "bundle" && r.Method == http.MethodGet:
		a.downloadBackupBundle(w, parts[1])
	case parts[0] == "schedules" && len(parts) == 1 && r.Method == http.MethodPost:
		a.saveBackupScheduleRoute(w, r, "")
	case parts[0] == "schedules" && len(parts) == 2 && r.Method == http.MethodPut:
		a.saveBackupScheduleRoute(w, r, parts[1])
	case parts[0] == "schedules" && len(parts) == 2 && r.Method == http.MethodDelete:
		a.deleteBackupSchedule(w, r, parts[1])
	case parts[0] == "schedules" && len(parts) == 3 && parts[2] == "run" && r.Method == http.MethodPost:
		a.runBackupScheduleNow(w, parts[1])
	default:
		writeError(w, http.StatusNotFound, "route not found")
	}
}

func (a *App) backupStorageView() map[string]any {
	repo, err := a.backupRepo()
	if err != nil {
		return map[string]any{"available": false, "reason": errBackupUnavailable.Error()}
	}
	defer repo.close()
	used, err := repo.usage()
	if err != nil {
		return map[string]any{"available": false, "reason": "The backup repository could not be read: " + err.Error()}
	}
	return map[string]any{"available": true, "usedBytes": used, "limitBytes": repo.limit, "location": "local filesystem"}
}

func (a *App) listBackups(w http.ResponseWriter) {
	snapshot := a.store.Snapshot()
	schedules := make([]backupScheduleView, 0, len(snapshot.BackupSchedules))
	for _, schedule := range snapshot.BackupSchedules {
		schedules = append(schedules, backupScheduleViewOf(schedule, snapshot.Servers))
	}
	sort.Slice(schedules, func(i, j int) bool {
		if schedules[i].ServerName == schedules[j].ServerName {
			return schedules[i].ID < schedules[j].ID
		}
		return schedules[i].ServerName < schedules[j].ServerName
	})
	var repo *backupRepository
	if candidate, err := a.backupRepo(); err == nil {
		repo = candidate
		defer repo.close()
	}
	runs := make([]backupRunView, 0, len(snapshot.BackupRuns))
	for _, run := range snapshot.BackupRuns {
		view := backupRunView{backupRun: run}
		view.Downloadable = run.Result == backupCompleted && repo != nil && repo.exists(run.ID)
		runs = append(runs, view)
	}
	sort.SliceStable(runs, func(i, j int) bool {
		if runs[i].StartedAt.Equal(runs[j].StartedAt) {
			return runs[i].ID > runs[j].ID
		}
		return runs[i].StartedAt.After(runs[j].StartedAt)
	})
	available := a.backupsEnabled()
	reason := ""
	if !available {
		reason = errBackupUnavailable.Error()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"available": available, "reason": reason, "demo": a.demo, "now": a.rebootNow(),
		"definitions": backupDefinitions, "storage": a.backupStorageView(),
		"schedules": schedules, "runs": runs,
	})
}

type backupRunRequest struct {
	ServerID     string `json:"serverId"`
	DefinitionID string `json:"definitionId"`
}

func (a *App) createBackupRun(w http.ResponseWriter, r *http.Request) {
	if !a.backupsEnabled() {
		writeError(w, http.StatusServiceUnavailable, errBackupUnavailable.Error())
		return
	}
	var request backupRunRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	definitionID := defaultValue(request.DefinitionID, dragonwildsWorldSave.ID)
	if _, ok := backupDefinitionByID(definitionID); !ok {
		writeError(w, http.StatusBadRequest, "definitionId must name a built-in backup profile")
		return
	}
	run, server, err := a.startBackupRun(strings.TrimSpace(request.ServerID), definitionID)
	if err != nil {
		a.writeBackupError(w, err)
		return
	}
	go a.executeBackupRun(run, server)
	writeJSON(w, http.StatusAccepted, run)
}

func (a *App) runBackupScheduleNow(w http.ResponseWriter, id string) {
	if !a.backupsEnabled() {
		writeError(w, http.StatusServiceUnavailable, errBackupUnavailable.Error())
		return
	}
	snapshot := a.store.Snapshot()
	schedule, ok := snapshot.BackupSchedules[id]
	if !ok {
		writeError(w, http.StatusNotFound, errBackupNotFound.Error())
		return
	}
	// An extra run never reads or writes the schedule cursor.
	run, server, err := a.startBackupRun(schedule.Definition.ServerID, schedule.DefinitionID)
	if err != nil {
		a.writeBackupError(w, err)
		return
	}
	go a.executeBackupRun(run, server)
	writeJSON(w, http.StatusAccepted, run)
}

func (a *App) deleteBackupRun(w http.ResponseWriter, id string) {
	repo, err := a.backupRepo()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, errBackupUnavailable.Error())
		return
	}
	defer repo.close()
	err = a.store.Update(func(state *State) error {
		for index, run := range state.BackupRuns {
			if run.ID != id {
				continue
			}
			if backupRunActive(run.Result) {
				return errBackupConflict
			}
			state.BackupRuns = append(state.BackupRuns[:index], state.BackupRuns[index+1:]...)
			return nil
		}
		return errBackupRunNotFound
	})
	if err != nil {
		a.writeBackupError(w, err)
		return
	}
	if err := repo.remove(id); err != nil {
		writeError(w, http.StatusInternalServerError, "the backup record was removed but its bundle could not be deleted")
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

func (a *App) downloadBackupBundle(w http.ResponseWriter, id string) {
	repo, err := a.backupRepo()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, errBackupUnavailable.Error())
		return
	}
	defer repo.close()
	bundle, size, err := repo.open(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "backup bundle not found")
		return
	}
	defer bundle.Close()
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("Content-Disposition", `attachment; filename="rsdw-backup-`+id+`.tar"`)
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, bundle); err != nil {
		log.Printf("stream backup bundle %s: %v", id, err)
	}
}

func (a *App) saveBackupScheduleRoute(w http.ResponseWriter, r *http.Request, id string) {
	var request backupRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid backup JSON: "+err.Error())
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid backup JSON: request must contain one object")
		return
	}
	definition, definitionID, enabled, err := normalizeBackupRequest(request)
	if err != nil {
		writeRebootValidationError(w, err)
		return
	}
	if enabled && !a.backupsEnabled() {
		writeError(w, http.StatusServiceUnavailable, errBackupUnavailable.Error())
		return
	}
	schedule, err := a.saveBackupSchedule(id, definition, definitionID, enabled, requestPrincipal(r).Subject)
	if err != nil {
		a.writeBackupError(w, err)
		return
	}
	status := http.StatusOK
	if id == "" {
		status = http.StatusCreated
	}
	writeJSON(w, status, backupScheduleViewOf(schedule, a.store.Snapshot().Servers))
}

func (a *App) saveBackupSchedule(id string, definition rebootDefinition, definitionID string, enabled bool, actor string) (backupSchedule, error) {
	var result backupSchedule
	err := a.store.Update(func(state *State) error {
		state.initBackups()
		now := a.rebootNow()
		server, ok := state.Servers[definition.ServerID]
		if !ok {
			return errors.New("server not found")
		}
		if state.deleting(definition.ServerID) {
			return errors.New("server is being deleted")
		}
		if id == "" && len(state.BackupSchedules) >= maxBackupSchedules {
			return backupDefinitionError{cause: errors.New("the deployment has reached the maximum of 256 backup schedules")}
		}
		old, exists := state.BackupSchedules[id]
		if id != "" && !exists {
			return errBackupNotFound
		}
		if id == "" {
			id = randomID()
			for state.BackupSchedules[id].ID != "" {
				id = randomID()
			}
		}
		changed := !exists || !sameRebootTiming(old.Definition, definition) || old.DefinitionID != definitionID
		candidate := old
		candidate.ID, candidate.Definition, candidate.DefinitionID, candidate.Enabled, candidate.UpdatedAt = id, definition, definitionID, enabled, now
		if !exists {
			candidate.CreatedAt, candidate.Revision = now, 1
		} else if changed || old.Enabled != enabled {
			candidate.Revision++
		}
		if definition.Mode == rebootModeInterval {
			if !exists || changed || !old.Enabled && enabled || candidate.IntervalAnchor == nil {
				candidate.IntervalAnchor = cloneTimePtr(&now)
			}
		} else {
			candidate.IntervalAnchor = nil
		}
		next, err := nextBackupRun(candidate, now)
		if err != nil {
			return backupDefinitionError{cause: err}
		}
		if enabled && (!exists || changed || old.NextRun == nil) {
			candidate.NextRun = cloneTimePtr(&next)
		} else if !enabled {
			candidate.NextRun = nil
		}
		state.BackupSchedules[id] = candidate
		message := "Backup schedule created"
		if exists {
			message = "Backup schedule updated"
		}
		appendBackupAudit(state, server, actor, id, message, rebootDefinitionSummary(definition))
		result = candidate
		return nil
	})
	return result, err
}

func (a *App) deleteBackupSchedule(w http.ResponseWriter, r *http.Request, id string) {
	err := a.store.Update(func(state *State) error {
		state.initBackups()
		schedule, ok := state.BackupSchedules[id]
		if !ok {
			return errBackupNotFound
		}
		delete(state.BackupSchedules, id)
		appendBackupAudit(state, state.Servers[schedule.Definition.ServerID], requestPrincipal(r).Subject, id, "Backup schedule deleted", "Existing bundles are retained; deletion only stops future occurrences")
		return nil
	})
	if err != nil {
		a.writeBackupError(w, err)
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

func appendBackupAudit(state *State, server Server, actor, scheduleID, message, details string) {
	addEvent(state, Event{ID: randomID(), Timestamp: time.Now().UTC(), ServerID: server.ID, ServerName: worldLabel(server), Category: "maintenance", Severity: "success", Message: message, Details: details, Kind: RebootScheduleChanged, Source: "C2 operator", Accuracy: "observed", ScheduleID: scheduleID, Actor: actor})
}

func (a *App) writeBackupError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errBackupNotFound), errors.Is(err, errBackupRunNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, errBackupConflict):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, errBackupUnavailable):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, errBackupInvalid):
		writeRebootValidationError(w, err)
	case strings.Contains(err.Error(), "server not found"), strings.Contains(err.Error(), "server is being deleted"), strings.Contains(err.Error(), "deletion is in progress"):
		writeRebootFieldError(w, "serverId", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "could not persist the backup request")
	}
}

type restoreRequest struct {
	RunID        string `json:"runId"`
	Confirm      string `json:"confirm"`
	PurgeConfirm string `json:"purgeConfirm"`
}

func purgeWorldToken(serverID string) string { return "REPLACE WORLD " + serverID }

// restoreSaveName derives the on-disk save name. A collected .sav.backup is restored as the live .sav.
func restoreSaveName(sourcePath string) string {
	name := path.Base(sourcePath)
	name = strings.TrimSuffix(name, ".sav.backup")
	if !strings.HasSuffix(name, ".sav") {
		name += ".sav"
	}
	return name
}

func (a *App) handleRestore(w http.ResponseWriter, r *http.Request, server Server) {
	if requestPrincipal(r).Role != RoleAdmin {
		writeError(w, http.StatusForbidden, "permission denied")
		return
	}
	if !a.backupsEnabled() {
		writeError(w, http.StatusServiceUnavailable, errBackupUnavailable.Error())
		return
	}
	// Writing a save under a live game process corrupts it.
	if server.Status != StatusStopped {
		writeError(w, http.StatusConflict, "stop the server before restoring a world")
		return
	}
	request, upload, err := decodeRestore(w, r)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errSaveTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		writeError(w, status, err.Error())
		return
	}
	if request.Confirm != server.ID {
		writeError(w, http.StatusBadRequest, "confirm must repeat the server ID")
		return
	}
	repo, err := a.backupRepo()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, errBackupUnavailable.Error())
		return
	}
	defer repo.close()
	restoreID := randomID()
	defer repo.discard(restoreID)
	name, err := a.stageRestoreSource(repo, restoreID, request, upload)
	if err != nil {
		a.writeBackupError(w, err)
		return
	}
	if a.demo {
		a.recordRestore(server, name, request.RunID)
		writeJSON(w, http.StatusOK, map[string]any{"server": server, "restored": name, "demo": true})
		return
	}
	k, ok := a.orchestrator.(*kubeOrchestrator)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "restore requires Kubernetes storage")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), backupRunTimeout)
	defer cancel()
	if err := k.restoreWorld(ctx, server, repo, restoreID, name, request.PurgeConfirm == purgeWorldToken(server.ID)); err != nil {
		if errors.Is(err, errWorldNotEmpty) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	a.recordRestore(server, name, request.RunID)
	writeJSON(w, http.StatusOK, map[string]any{"server": server, "restored": name})
}

func (a *App) recordRestore(server Server, name, runID string) {
	source := "an uploaded .sav file"
	if runID != "" {
		source = "tracked backup " + runID
	}
	if err := a.store.Update(func(state *State) error {
		appendEvent(state, server, "system", "success", "World restored", "Restored "+name+" from "+source)
		return nil
	}); err != nil {
		log.Printf("record restore for %s: %v", server.ID, err)
	}
}

func decodeRestore(w http.ResponseWriter, r *http.Request) (restoreRequest, *saveUpload, error) {
	var request restoreRequest
	kind, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || kind != "multipart/form-data" {
		if r.Header.Get("Content-Type") != "" && kind != "application/json" {
			return request, nil, errors.New("use application/json or multipart/form-data with request and save parts")
		}
		return request, nil, decodeJSON(r, &request)
	}
	if r.ContentLength > maxCreateBytes {
		return request, nil, errSaveTooLarge
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxCreateBytes)
	reader := multipart.NewReader(r.Body, params["boundary"])
	var upload *saveUpload
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				return request, nil, errSaveTooLarge
			}
			return request, nil, errors.New("invalid multipart upload")
		}
		_, disposition, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
		if err != nil {
			return request, nil, errors.New("invalid upload part")
		}
		switch part.FormName() {
		case "request":
			metadata, err := io.ReadAll(io.LimitReader(part, (32<<10)+1))
			if err != nil || len(metadata) > 32<<10 {
				return request, nil, errors.New("restore settings must be at most 32 KiB")
			}
			decoder := json.NewDecoder(strings.NewReader(string(metadata)))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&request); err != nil {
				return request, nil, errors.New("invalid restore settings JSON")
			}
		case "save":
			if upload != nil {
				return request, nil, errors.New("provide exactly one .sav file")
			}
			if !validSaveName(disposition["filename"]) {
				return request, nil, errors.New("select a .sav file with a plain filename of at most 128 bytes, without paths, control characters, or a leading hyphen")
			}
			content, err := io.ReadAll(io.LimitReader(part, maxSaveBytes+1))
			if err != nil {
				return request, nil, errors.New("could not read save file")
			}
			if len(content) > maxSaveBytes {
				return request, nil, errSaveTooLarge
			}
			if len(content) == 0 {
				return request, nil, errors.New("save file must not be empty")
			}
			upload = &saveUpload{name: disposition["filename"], data: content}
		default:
			return request, nil, errors.New("upload accepts only request and save parts")
		}
	}
	if upload == nil {
		return request, nil, errors.New("upload requires one request part and one .sav file")
	}
	return request, upload, nil
}

// stageRestoreSource writes the chosen bytes into the repository and returns the target save name.
func (a *App) stageRestoreSource(repo *backupRepository, restoreID string, request restoreRequest, upload *saveUpload) (string, error) {
	if upload != nil {
		if request.RunID != "" {
			return "", errors.New("choose either a tracked backup or an uploaded file, not both")
		}
		staged, err := repo.stage(restoreID)
		if err != nil {
			return "", err
		}
		_, err = staged.Write(upload.data)
		staged.Close()
		if err != nil {
			return "", err
		}
		return upload.name, nil
	}
	if request.RunID == "" {
		return "", errors.New("choose a tracked backup or upload a .sav file")
	}
	bundle, _, err := repo.open(request.RunID)
	if err != nil {
		return "", errBackupRunNotFound
	}
	defer bundle.Close()
	reader := tar.NewReader(bundle)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", errors.New("the backup bundle could not be read")
		}
		if !strings.HasPrefix(header.Name, "items/") {
			continue
		}
		if header.Size > maxBackupItemBytes {
			return "", errors.New("the backup bundle item exceeds the restore limit")
		}
		staged, err := repo.stage(restoreID)
		if err != nil {
			return "", err
		}
		_, err = io.Copy(staged, io.LimitReader(reader, header.Size))
		staged.Close()
		if err != nil {
			return "", err
		}
		return restoreSaveName(backupItemSourcePath(repo, request.RunID, header.Name)), nil
	}
	return "", errors.New("the backup bundle contains no world item")
}

// backupItemSourcePath prefers the manifest's recorded source path so the original save name survives.
func backupItemSourcePath(repo *backupRepository, runID, storedAs string) string {
	data, err := repo.root.ReadFile(runID + ".json")
	if err != nil {
		return storedAs
	}
	var manifest backupManifest
	if json.Unmarshal(data, &manifest) != nil {
		return storedAs
	}
	for _, item := range manifest.Items {
		if item.StoredAs == storedAs && item.Path != "" {
			return item.Path
		}
	}
	return storedAs
}

var errWorldNotEmpty = errors.New("target world already contains save data; confirm replacement explicitly")

func (k *kubeOrchestrator) restoreWorld(ctx context.Context, server Server, repo *backupRepository, restoreID, name string, purge bool) error {
	claim, err := k.worldClaim(ctx, server)
	if err != nil {
		return err
	}
	inspector := "rsdw-restore-" + restoreID
	if err := k.startInspector(ctx, server.Namespace, inspector, claim, false, repo, restoreID); err != nil {
		k.deleteInspector(server.Namespace, inspector)
		return err
	}
	defer k.deleteInspector(server.Namespace, inspector)
	dir := backupWorldMount + "/" + backupSaveDir
	if _, err := k.inspectorExec(ctx, server.Namespace, inspector, "sh", "-ec", `mkdir -p "$1"`, "restore", dir); err != nil {
		return fmt.Errorf("could not prepare the world save directory: %w", err)
	}
	if !purge {
		existing, err := listBackupCandidates(ctx, func(ctx context.Context, args ...string) ([]byte, error) {
			return k.inspectorExec(ctx, server.Namespace, inspector, args...)
		}, dir, ".sav")
		if err != nil {
			return err
		}
		if len(existing) > 0 {
			return errWorldNotEmpty
		}
	}
	if _, err := k.inspectorExec(ctx, server.Namespace, inspector, "sh", "-ec", `test ! -e "$1/.incoming"; test ! -L "$1/.incoming"; test ! -L "$1/$2"`, "restore", dir, name); err != nil {
		return errors.New("the world volume holds an unexpected file at the restore target")
	}
	copyCtx, cancel := context.WithTimeout(ctx, backupCopyTimeout)
	defer cancel()
	if _, err := k.runner.Run(copyCtx, k.kubectl, "-n", server.Namespace, "cp", "-c", "reader", repo.stagingPath(restoreID), inspector+":"+dir+"/.incoming"); err != nil {
		return fmt.Errorf("could not copy the save into the world volume: %w", err)
	}
	if _, err := k.inspectorExec(ctx, server.Namespace, inspector, "sh", "-ec", `test -f "$1/.incoming"; test ! -L "$1/.incoming"; chmod 600 "$1/.incoming"; sync; mv -T "$1/.incoming" "$1/$2"; sync`, "restore", dir, name); err != nil {
		return fmt.Errorf("could not publish the restored save: %w", err)
	}
	return nil
}
