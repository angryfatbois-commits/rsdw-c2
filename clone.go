package main

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// handleClone creates a new stopped server that starts as a copy of a world save.
// It runs before handleServerRoute's shared Server lookup because source (2) below
// clones from a completed keep-mode deletion receipt, which has no live Server record.
func (a *App) handleClone(w http.ResponseWriter, r *http.Request, id string) {
	if !a.lifecycleMu.TryLock() {
		writeError(w, http.StatusConflict, "another server lifecycle operation is in progress")
		return
	}
	defer a.lifecycleMu.Unlock()
	var request struct {
		ServerName    string `json:"serverName"`
		OwnerName     string `json:"ownerName"`
		OwnerID       string `json:"ownerId"`
		ConfirmCreate bool   `json:"confirmCreate"`
	}
	if err := decodeBackupJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !request.ConfirmCreate {
		writeError(w, http.StatusBadRequest, "confirm that a new server and world will be created")
		return
	}
	if strings.TrimSpace(request.ServerName) == "" || len(request.ServerName) > 48 || strings.ContainsAny(request.ServerName, "\x00\r\n") {
		writeError(w, http.StatusBadRequest, "serverName must contain 1 to 48 characters")
		return
	}
	if strings.TrimSpace(request.OwnerName) == "" || len(request.OwnerName) > 48 || strings.ContainsAny(request.OwnerName, "\x00\r\n") {
		writeError(w, http.StatusBadRequest, "ownerName must contain 1 to 48 characters")
		return
	}
	ownerID, err := normalizePlayerID(request.OwnerID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "provide a valid owner EOS ID")
		return
	}

	snapshot := a.store.Snapshot()
	var namespace, image string
	var maxPlayers, memoryLimitMiB, cpuLimitMillis int
	var settings ServerSettings
	var sourceLabel string
	var claim string
	var syntheticSource Server
	fromLiveServer := false

	if source, ok := snapshot.Servers[id]; ok {
		if snapshot.deleting(id) {
			writeError(w, http.StatusConflict, "source server is being deleted")
			return
		}
		if source.Status != StatusStopped {
			writeError(w, http.StatusConflict, "stop the source server and wait for C2 to verify it is stopped before cloning")
			return
		}
		namespace, image = source.Namespace, source.CurrentImage
		maxPlayers, memoryLimitMiB, cpuLimitMillis = source.MaxPlayers, source.MemoryLimitMiB, source.CPULimitMillis
		settings = source.ServerSettings
		sourceLabel = worldLabel(source)
		syntheticSource = source
		fromLiveServer = true
	} else if record, ok := snapshot.Deletions[id]; ok {
		if record.Mode != keepWorld {
			writeError(w, http.StatusConflict, "world data was purged; nothing to clone")
			return
		}
		if !record.Completed {
			writeError(w, http.StatusConflict, "deletion is still in progress; wait for it to complete before cloning")
			return
		}
		if len(record.Plan.World) > 1 || (!a.demo && len(record.Plan.World) != 1) {
			writeError(w, http.StatusConflict, "retained world plan does not contain exactly one PVC")
			return
		}
		namespace = record.Namespace
		image = envOr("RSDW_SEED_WRITER_IMAGE", "busybox:1.37.0")
		maxPlayers = 4
		sourceLabel = record.WorldLabel
		if len(record.Plan.World) == 1 {
			claim = record.Plan.World[0].Name
		}
		syntheticSource = Server{ID: record.ServerID, Namespace: record.Namespace, CurrentImage: image}
	} else {
		writeError(w, http.StatusNotFound, "clone source not found")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	newID, err := a.newServerID(ctx, CreateServerRequest{Name: request.OwnerName, Namespace: namespace, ServerSettings: ServerSettings{WorldName: request.ServerName}})
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	token, err := randomToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not allocate server ownership")
		return
	}
	created := Server{ID: newID, Release: newID, Name: strings.TrimSpace(request.OwnerName), Namespace: namespace, OwnerID: ownerID, OwnershipToken: token, CurrentImage: image, DesiredImage: image, MaxPlayers: maxPlayers, MemoryLimitMiB: memoryLimitMiB, CPULimitMillis: cpuLimitMillis, Status: StatusStopped, ServerSettings: settings}
	created.WorldName = strings.TrimSpace(request.ServerName)
	created.AdminIDs = ""
	if err := validateCreate(CreateServerRequest{Name: created.Name, Namespace: created.Namespace, OwnerID: created.OwnerID, MaxPlayers: created.MaxPlayers, MemoryLimitMiB: created.MemoryLimitMiB, CPULimitMillis: created.CPULimitMillis, ServerSettings: created.ServerSettings}); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !validRestoreSaveName(created.WorldName + ".sav") {
		writeError(w, http.StatusBadRequest, "target world name cannot form a safe save filename")
		return
	}

	file, cleanupCapture, captureErr := a.captureCloneSource(ctx, syntheticSource, claim, fromLiveServer)
	if captureErr != nil {
		writeError(w, http.StatusBadGateway, "could not read source world: "+captureErr.Error())
		return
	}
	defer cleanupCapture()

	if k, ok := a.orchestrator.(*kubeOrchestrator); ok && !a.demo {
		err = k.DeployStopped(ctx, created)
	} else {
		err = a.orchestrator.Deploy(ctx, created)
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not provision stopped clone target: "+err.Error())
		return
	}
	if err := a.store.Update(func(state *State) error {
		state.Servers[created.ID] = created
		appendEvent(state, created, "system", "info", "Clone target created", "Cloned from "+sourceLabel+"; new server and world PVC remain stopped until the copy completes")
		return nil
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "could not persist clone target")
		return
	}

	var files []restoreFile
	if file != nil {
		files = []restoreFile{{Path: path.Join(backupWorldSaveRoot, "World.sav"), LocalPath: file.Name()}}
	}
	if len(files) > 0 {
		if err := a.restoreFiles(ctx, created, files, false); err != nil {
			_ = a.store.Update(func(state *State) error {
				appendEvent(state, created, "system", "error", "Clone failed", "Retry from Maintenance on "+created.ID+": "+err.Error())
				return nil
			})
			writeError(w, http.StatusBadGateway, "new server "+created.ID+" remains stopped; retry clone from Maintenance: "+err.Error())
			return
		}
	}
	_ = a.store.Update(func(state *State) error {
		appendEvent(state, created, "system", "success", "Server cloned", "Cloned from "+sourceLabel)
		return nil
	})
	writeJSON(w, http.StatusCreated, created)
}

// captureCloneSource reads the source world save into a local temp file, returning nil
// when there is nothing to copy (demo mode with no recorded demo world). The returned
// cleanup always closes the file (if any) and removes its containing temp directory.
func (a *App) captureCloneSource(ctx context.Context, source Server, claim string, fromLiveServer bool) (*os.File, func(), error) {
	noop := func() {}
	if a.demo {
		if a.backups == nil || a.backups.root == "" {
			return nil, noop, nil
		}
		restored, err := os.OpenRoot(filepath.Join(a.backups.root, ".demo-worlds", source.ID))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, noop, nil
			}
			return nil, noop, err
		}
		defer restored.Close()
		relative := backupWorldSaveRoot
		info, err := restored.Lstat(relative)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, noop, nil
			}
			return nil, noop, err
		}
		if info.IsDir() {
			entries, err := fs.ReadDir(restored.FS(), relative)
			if err != nil {
				return nil, noop, err
			}
			var matches []string
			for _, entry := range entries {
				if !entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") && backupSaveSuffix(entry.Name()) == ".sav" {
					matches = append(matches, entry.Name())
				}
			}
			if len(matches) != 1 {
				return nil, noop, nil
			}
			relative = filepath.ToSlash(filepath.Join(relative, matches[0]))
		}
		data, err := demoBackupPayload(ctx, restored, relative, BackupItemFile, source.ID+".sav", []byte("demo world clone source="+source.ID+"\n"), maxSaveBytes)
		if err != nil {
			return nil, noop, err
		}
		captureRoot, err := os.MkdirTemp("", "rsdw-clone-")
		if err != nil {
			return nil, noop, err
		}
		cleanup := func() { _ = os.RemoveAll(captureRoot) }
		file, err := os.CreateTemp(captureRoot, "world-")
		if err != nil {
			cleanup()
			return nil, noop, err
		}
		if _, err := file.Write(data); err != nil {
			_ = file.Close()
			cleanup()
			return nil, noop, err
		}
		if _, err := file.Seek(0, 0); err != nil {
			_ = file.Close()
			cleanup()
			return nil, noop, err
		}
		return file, func() { _ = file.Close(); cleanup() }, nil
	}

	k, ok := a.orchestrator.(*kubeOrchestrator)
	if !ok {
		return nil, noop, errors.New("Kubernetes clone collection is unavailable")
	}
	var target podTarget
	var targetCleanup func()
	var err error
	if fromLiveServer {
		target, targetCleanup, err = k.backupTarget(ctx, source, BackupServerStopped, false)
	} else {
		target, targetCleanup, err = k.backupInspector(ctx, source, claim, false)
	}
	if err != nil {
		return nil, noop, err
	}
	captureRoot, err := os.MkdirTemp("", "rsdw-clone-")
	if err != nil {
		targetCleanup()
		return nil, noop, err
	}
	cleanup := func() { _ = os.RemoveAll(captureRoot); targetCleanup() }
	spec := BackupItemSpec{Name: "world-save", Kind: BackupItemFile, Requirement: BackupItemRequired}
	rule := BackupSourceRule{ServerState: BackupServerStopped, Collector: BackupCollectorServerFiles, Path: backupWorldSaveRoot}
	item, file, err := k.captureBackupItem(ctx, target, spec, rule, BackupServerStopped, captureRoot, maxSaveBytes)
	if err != nil {
		cleanup()
		if errors.Is(err, errBackupSourceMissing) {
			return nil, noop, nil
		}
		return nil, noop, err
	}
	_ = item
	return file, func() { _ = file.Close(); cleanup() }, nil
}
