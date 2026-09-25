package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

var errRestoreConfirmation = errors.New("target contains save data; a separate destructive confirmation is required")

type restoreFile struct {
	Path      string
	Data      []byte
	LocalPath string
}

func (a *App) backupRestoreFiles(ctx context.Context, manifestID string) (files []restoreFile, err error) {
	if !a.backupAvailable() {
		return nil, errors.New("backup storage is unavailable")
	}
	snapshot := a.store.Snapshot()
	manifest, ok := snapshot.BackupManifests[manifestID]
	if !ok || manifest.ServerType != "dragonwilds" {
		return nil, errors.New("completed Dragonwilds backup not found")
	}
	var run BackupRun
	for _, candidate := range snapshot.BackupRuns {
		if candidate.ManifestID == manifestID && candidate.Status == BackupRunStatusSucceeded {
			run = candidate
			break
		}
	}
	if run.Bundle.Key == "" {
		return nil, errors.New("completed backup bundle not found")
	}
	backendID := run.BackendID
	if backendID == "" {
		backendID = backupLocalBackendID
	}
	repository, err := a.repositoryFor(backendID)
	if err != nil {
		return nil, err
	}
	reader, err := repository.OpenBundle(ctx, run.Bundle.Key)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	limit := a.backupBundleLimit()
	if run.Bundle.Size <= 0 || run.Bundle.Size > limit {
		return nil, errors.New("invalid backup bundle size")
	}
	bundle, err := os.CreateTemp(a.backups.root, ".restore-bundle-")
	if err != nil {
		return nil, err
	}
	defer os.Remove(bundle.Name())
	defer bundle.Close()
	hash := sha256.New()
	count, err := io.Copy(io.MultiWriter(bundle, hash), io.LimitReader(reader, run.Bundle.Size+1))
	if err != nil {
		return nil, err
	}
	if count != run.Bundle.Size || hex.EncodeToString(hash.Sum(nil)) != run.Bundle.SHA256 {
		return nil, errors.New("backup bundle checksum mismatch")
	}
	archive, err := zip.NewReader(bundle, count)
	if err != nil {
		return nil, err
	}
	entries := map[string]*zip.File{}
	for _, entry := range archive.File {
		if entries[entry.Name] != nil || !entry.Mode().IsRegular() {
			return nil, errors.New("invalid or duplicate bundle entry")
		}
		entries[entry.Name] = entry
	}
	readEntry := func(name string, size int64) ([]byte, error) {
		entry := entries[name]
		if entry == nil || entry.UncompressedSize64 > uint64(size) {
			return nil, errors.New("missing or oversized bundle entry")
		}
		stream, err := entry.Open()
		if err != nil {
			return nil, err
		}
		defer stream.Close()
		return io.ReadAll(io.LimitReader(stream, size+1))
	}
	manifestData, err := readEntry("manifest.json", 1<<20)
	if err != nil {
		return nil, err
	}
	var embedded BackupManifest
	if err := json.Unmarshal(manifestData, &embedded); err != nil {
		return nil, err
	}
	expectedJSON, _ := json.Marshal(manifest)
	actualJSON, _ := json.Marshal(embedded)
	if !bytes.Equal(expectedJSON, actualJSON) {
		return nil, errors.New("backup manifest does not match its tracked metadata")
	}
	if len(entries) != len(manifest.Items)+1 {
		return nil, errors.New("untracked bundle contents")
	}
	var staged []restoreFile
	defer func() {
		if err != nil {
			cleanupRestoreFiles(staged)
		}
	}()
	var total int64
	seen := map[string]bool{}
	add := func(relative string, content []byte) error {
		if err := ValidateBackupRelativePath(relative); err != nil {
			return err
		}
		if seen[relative] {
			return errors.New("backup items restore to duplicate destinations")
		}
		if int64(len(content)) > a.backups.maxItem || total+int64(len(content)) > limit {
			return errors.New("restore exceeds configured limits")
		}
		seen[relative] = true
		total += int64(len(content))
		payload, err := os.CreateTemp(a.backups.root, ".restore-payload-")
		if err != nil {
			return err
		}
		staged = append(staged, restoreFile{Path: relative, LocalPath: payload.Name()})
		_, writeErr := payload.Write(content)
		closeErr := payload.Close()
		if err := errors.Join(writeErr, closeErr); err != nil {
			return err
		}
		return nil
	}
	for _, item := range manifest.Items {
		if item.Size < 0 || item.Size > a.backups.maxItem {
			return nil, errors.New("backup item exceeds configured limit")
		}
		content, err := readEntry(item.ObjectKey, item.Size)
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256(content)
		if int64(len(content)) != item.Size || hex.EncodeToString(digest[:]) != item.SHA256 {
			return nil, errors.New("backup item checksum mismatch")
		}
		if item.Kind == BackupItemDirectory {
			tr := tar.NewReader(bytes.NewReader(content))
			for {
				header, err := tr.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					return nil, err
				}
				relative := strings.TrimSuffix(header.Name, "/")
				if err := ValidateBackupRelativePath(relative); err != nil {
					return nil, err
				}
				if relative != item.SourcePath && !strings.HasPrefix(relative, item.SourcePath+"/") {
					return nil, errors.New("directory entry escapes its declared source")
				}
				if header.Typeflag == tar.TypeDir {
					continue
				}
				if header.Typeflag != tar.TypeReg || header.Size < 0 || header.Size > a.backups.maxItem {
					return nil, errors.New("directory restore accepts only bounded regular files")
				}
				value, err := io.ReadAll(io.LimitReader(tr, header.Size+1))
				if err != nil {
					return nil, err
				}
				if err := add(restoreDestination(relative, manifest.Source), value); err != nil {
					return nil, err
				}
			}
		} else if item.Kind == BackupItemFile || item.Kind == "" {
			if err := add(restoreDestination(item.SourcePath, manifest.Source), content); err != nil {
				return nil, err
			}
		} else {
			return nil, errors.New("unsupported restore item kind")
		}
	}
	if len(staged) == 0 {
		return nil, errors.New("backup contains no restorable files")
	}
	sort.Slice(staged, func(i, j int) bool { return staged[i].Path < staged[j].Path })
	return staged, nil
}

func cleanupRestoreFiles(files []restoreFile) {
	for _, file := range files {
		if file.LocalPath != "" {
			_ = os.Remove(file.LocalPath)
		}
	}
}

func restoreFileBytes(file restoreFile) ([]byte, error) {
	if file.LocalPath != "" {
		return os.ReadFile(file.LocalPath)
	}
	return file.Data, nil
}

func restoreDestination(source string, mode BackupSource) string {
	if mode == BackupSourceRunningBAK && strings.HasSuffix(strings.ToLower(source), ".sav.backup") {
		return source[:len(source)-len(".backup")]
	}
	if mode == BackupSourceRunningBAK && strings.EqualFold(path.Ext(source), ".bak") {
		base := strings.TrimSuffix(source, path.Ext(source))
		if strings.EqualFold(path.Ext(base), ".sav") {
			return base
		}
		return base + ".sav"
	}
	return source
}

// The journal is written before each rename so a later attempt can recover after a killed process.
const restoreTransactionScript = `set -eu
root=$1
mode=$2
overwrite=$3
replace_world=$4
stage="$root/.c2-restore"
safe() {
  rel=$1
  case "$rel" in ''|/*|../*|*/../*|*/..|..|./*|*/./*|*/.|.c2-*|*\\*) return 1;; esac
  current=$root
  while [ -n "$rel" ]; do
    component=${rel%%/*}
    test -n "$component"
    current="$current/$component"
    test ! -L "$current"
    if [ "$rel" = "$component" ]; then break; fi
    test ! -e "$current" || test -d "$current"
    rel=${rel#*/}
  done
}
recover() {
  test ! -L "$stage"
  if [ -d "$stage" ]; then test -z "$(find "$stage" -type l -print -quit)"; fi
  if [ -f "$stage/committed" ]; then rm -rf -- "$stage"; return; fi
  if [ -f "$stage/journal" ]; then
    while IFS= read -r file; do
      safe "$file"
      if [ -f "$stage/original/$file" ]; then
        mkdir -p -- "$(dirname "$root/$file")"
        mv -f -- "$stage/original/$file" "$root/$file"
      elif [ -f "$stage/new/$file" ]; then
        rm -f -- "$root/$file"
      fi
    done < "$stage/journal"
  fi
  rm -rf -- "$stage"
  sync
}
test -d "$root"
test ! -L "$root"
test ! -L "$stage"
if [ -d "$stage" ]; then test -z "$(find "$stage" -type l -print -quit)"; fi
ancestor=$root
while [ "$ancestor" != / ] && [ "$ancestor" != . ]; do
  test ! -L "$ancestor"
  ancestor=$(dirname "$ancestor")
done
if [ "$mode" = prepare ]; then
  recover
  mkdir -m 700 -- "$stage"
  exit
fi
if [ "$mode" = finalize ]; then
  test -f "$stage/applied"
  : > "$stage/committed"
  sync
  rm -rf -- "$stage"
  exit
fi
test "$mode" = commit
test -d "$stage/incoming"
test -f "$stage/paths"
test -z "$(find "$stage" -type l -print -quit)"
(cd "$stage/incoming" && sha256sum -c ../checksums)
safe RSDragonwilds/Saved/SaveGames
if [ -d "$root/RSDragonwilds/Saved/SaveGames" ]; then
  test -z "$(find "$root/RSDragonwilds/Saved/SaveGames" -type l -print -quit)"
  save_root="$root/RSDragonwilds/Saved/SaveGames"
  find "$save_root" -type f \( -iname '*.sav' -o -iname '*.bak' -o -iname '*.sav.backup' \) -exec sh -ec '
    root=$1
    shift
    newline="
"
    for absolute do
      case "$absolute" in
        "$root"/*) file=${absolute#"$root"/} ;;
        *) exit 1 ;;
      esac
      case "$file" in *"$newline"*) exit 1 ;; esac
      printf "%s\n" "$file"
    done
  ' sh "$root" {} + > "$stage/existing"
else
  : > "$stage/existing"
fi
while IFS= read -r file; do safe "$file"; done < "$stage/existing"
if [ -s "$stage/existing" ] && [ "$overwrite" != true ]; then
  printf 'RESTORE_CONFIRMATION_REQUIRED\n'
  exit 42
fi
while IFS= read -r file; do
  safe "$file"
  test -f "$stage/incoming/$file"
  test ! -e "$root/$file" || test -f "$root/$file"
done < "$stage/paths"
if [ "$replace_world" = true ]; then
  cat "$stage/existing" "$stage/paths" | sort -u > "$stage/targets"
else
  sort -u "$stage/paths" > "$stage/targets"
fi
mkdir "$stage/original" "$stage/new"
: > "$stage/journal"
trap 'recover' HUP INT TERM
trap 'code=$?; if [ "$code" != 0 ]; then recover; fi' EXIT
while IFS= read -r file; do
  safe "$file"
  printf '%s\n' "$file" >> "$stage/journal"
  if [ -f "$root/$file" ]; then
    mkdir -p -- "$(dirname "$stage/original/$file")"
    sync
    mv -- "$root/$file" "$stage/original/$file"
  else
    mkdir -p -- "$(dirname "$stage/new/$file")"
    : > "$stage/new/$file"
    sync
  fi
  if [ -f "$stage/incoming/$file" ]; then
    mkdir -p -- "$(dirname "$root/$file")"
    mv -- "$stage/incoming/$file" "$root/$file"
  fi
done < "$stage/targets"
sync
: > "$stage/applied"
sync
trap - EXIT HUP INT TERM
`

func (a *App) restoreFiles(ctx context.Context, server Server, files []restoreFile, destructive bool) error {
	if len(files) == 0 {
		return errors.New("restore has no files")
	}
	files = append([]restoreFile(nil), files...)
	worlds := 0
	for index := range files {
		if path.Dir(files[index].Path) == backupWorldSaveRoot && strings.EqualFold(path.Ext(files[index].Path), ".sav") {
			worlds++
			if server.WorldName != "" {
				name := server.WorldName + ".sav"
				if !validRestoreSaveName(name) {
					return errors.New("target world name cannot form a safe save filename")
				}
				files[index].Path = path.Join(backupWorldSaveRoot, name)
			}
		}
	}
	if worlds > 1 {
		return errors.New("backup contains multiple world saves; select a single-world profile")
	}
	staging, err := os.MkdirTemp(a.backups.root, ".restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	var paths strings.Builder
	var checksums strings.Builder
	seen := map[string]bool{}
	for _, file := range files {
		if err := ValidateBackupRelativePath(file.Path); err != nil {
			return err
		}
		if seen[file.Path] {
			return errors.New("restore contains duplicate destination paths")
		}
		seen[file.Path] = true
		dest := filepath.Join(staging, "incoming", filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return err
		}
		data, err := restoreFileBytes(file)
		if err != nil {
			return err
		}
		if err := os.WriteFile(dest, data, 0o600); err != nil {
			return err
		}
		paths.WriteString(file.Path + "\n")
		checksums.WriteString(fmt.Sprintf("%x  %s\n", sha256.Sum256(data), file.Path))
	}
	if err := os.WriteFile(filepath.Join(staging, "paths"), []byte(paths.String()), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(staging, "checksums"), []byte(checksums.String()), 0o600); err != nil {
		return err
	}
	overwrite := fmt.Sprint(destructive)
	if a.demo {
		root := filepath.Join(a.backups.root, ".demo-worlds", server.ID)
		if err := os.MkdirAll(root, 0o700); err != nil {
			return err
		}
		run := func(mode string) error {
			output, err := exec.CommandContext(ctx, "sh", "-ec", restoreTransactionScript, "restore", root, mode, overwrite, fmt.Sprint(worlds > 0)).CombinedOutput()
			if strings.Contains(string(output), "RESTORE_CONFIRMATION_REQUIRED") {
				return errRestoreConfirmation
			}
			if err != nil {
				return fmt.Errorf("restore transaction: %w: %s", err, output)
			}
			return nil
		}
		if err := run("prepare"); err != nil {
			return err
		}
		for _, name := range []string{"incoming", "paths", "checksums"} {
			if err := os.Rename(filepath.Join(staging, name), filepath.Join(root, ".c2-restore", name)); err != nil {
				return err
			}
		}
		if err := run("commit"); err != nil {
			return err
		}
		return run("finalize")
	}
	k, ok := a.orchestrator.(*kubeOrchestrator)
	if !ok {
		return errors.New("Kubernetes restore collector is unavailable")
	}
	target, cleanup, err := k.backupTarget(ctx, server, BackupServerStopped)
	if err != nil {
		return err
	}
	defer cleanup()
	run := func(mode string) error {
		output, err := k.backupPodExec(ctx, target, "sh", "-ec", restoreTransactionScript, "restore", backupDataRoot, mode, overwrite, fmt.Sprint(worlds > 0))
		if strings.Contains(string(output), "RESTORE_CONFIRMATION_REQUIRED") {
			return errRestoreConfirmation
		}
		return err
	}
	if err := run("prepare"); err != nil {
		return fmt.Errorf("prepare restore: %w", err)
	}
	for _, name := range []string{"incoming", "paths", "checksums"} {
		if _, err := k.runner.Run(ctx, k.kubectl, "-n", server.Namespace, "cp", "-c", target.container.Name, filepath.Join(staging, name), target.pod.Metadata.Name+":"+backupDataRoot+"/.c2-restore/"+name); err != nil {
			return fmt.Errorf("stage restore: %w", err)
		}
	}
	if err := k.verifyBackupStopped(ctx, server); err != nil {
		return err
	}
	if err := run("commit"); err != nil {
		return err
	}
	if err := k.verifyBackupStopped(ctx, server); err != nil {
		return fmt.Errorf("target changed state during restore; original files retained for recovery on the next stopped restore: %w", err)
	}
	return run("finalize")
}
