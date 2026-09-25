package main

import (
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
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	ErrBackupQuotaExceeded = errors.New("backup storage quota exceeded")
	ErrPublicationConflict = errors.New("backup publication conflicts with existing data")
)

type BackupPublicationItem struct {
	Name        string
	Kind        BackupItemKind
	SourcePath  string
	Consistency BackupItemConsistency
	Content     io.Reader
}

type PublishBackupRequest struct {
	Idempotency IdempotencyRequest
	Manifest    BackupManifestDraft
	Items       []BackupPublicationItem
}

type BackupObject struct {
	Key    string `json:"key"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type PublishedBackup struct {
	Idempotency IdempotencyRequest `json:"idempotency"`
	Manifest    BackupManifest     `json:"manifest"`
	Bundle      BackupObject       `json:"bundle"`
}

type BackupRepository interface {
	Publish(context.Context, PublishBackupRequest) (PublishedBackup, error)
	Recover(context.Context) ([]PublishedBackup, error)
	OpenBundle(context.Context, string) (io.ReadCloser, error)
	Delete(context.Context, string) error
	Usage(context.Context) (int64, error)
}

type LocalBackupRepositoryConfig struct {
	Root               string
	MaxRepositoryBytes int64
	MaxBackupBytes     int64
	MaxItemBytes       int64
}

type LocalBackupRepository struct {
	root               string
	maxRepositoryBytes int64
	maxBackupBytes     int64
	maxItemBytes       int64
	mu                 sync.Mutex
}

func NewLocalBackupRepository(config LocalBackupRepositoryConfig) (*LocalBackupRepository, error) {
	if strings.TrimSpace(config.Root) == "" {
		return nil, errors.New("backup repository root is required")
	}
	if config.MaxRepositoryBytes < 0 || config.MaxBackupBytes < 0 || config.MaxItemBytes < 0 {
		return nil, errors.New("backup repository limits cannot be negative")
	}
	root, err := filepath.Abs(config.Root)
	if err != nil {
		return nil, fmt.Errorf("resolve backup repository root: %w", err)
	}
	for _, directory := range []string{root, filepath.Join(root, ".staging"), filepath.Join(root, "objects")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, fmt.Errorf("create backup repository directory: %w", err)
		}
	}
	if err := syncDirectory(root); err != nil {
		return nil, err
	}
	return &LocalBackupRepository{
		root:               root,
		maxRepositoryBytes: config.MaxRepositoryBytes,
		maxBackupBytes:     config.MaxBackupBytes,
		maxItemBytes:       config.MaxItemBytes,
	}, nil
}

func (repository *LocalBackupRepository) Publish(ctx context.Context, request PublishBackupRequest) (PublishedBackup, error) {
	var published PublishedBackup
	err := repository.withWriterLock(func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := validateIdempotencyRequest(request.Idempotency); err != nil {
			return err
		}
		if err := validateManifestDraft(request.Manifest); err != nil {
			return err
		}

		objectDirectory := repository.objectDirectory(request.Manifest.ID)
		if _, err := os.Stat(objectDirectory); err == nil {
			existing, err := readPublishedBackup(objectDirectory)
			if err != nil {
				return fmt.Errorf("read existing backup publication: %w", err)
			}
			if err := matchPublication(existing, request.Idempotency, request.Manifest.ID); err != nil {
				return err
			}
			published = existing
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect backup publication: %w", err)
		}

		stageDirectory := repository.stageDirectory(request.Idempotency.Identity())
		if _, err := os.Stat(stageDirectory); err == nil {
			staged, err := readPublishedBackup(stageDirectory)
			if err != nil {
				return fmt.Errorf("read staged backup publication: %w", err)
			}
			if err := matchPublication(staged, request.Idempotency, request.Manifest.ID); err != nil {
				return err
			}
			published, err = repository.publishStaged(staged, stageDirectory)
			return err
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect staged backup publication: %w", err)
		}

		staged, err := repository.createStage(ctx, request)
		if err != nil {
			return err
		}
		published, err = repository.publishStaged(staged, repository.stageDirectory(request.Idempotency.Identity()))
		return err
	})
	if err != nil {
		return PublishedBackup{}, err
	}
	return published, nil
}

func (repository *LocalBackupRepository) Recover(ctx context.Context) ([]PublishedBackup, error) {
	var recovered []PublishedBackup
	err := repository.withWriterLock(func() error {
		entries, err := os.ReadDir(filepath.Join(repository.root, ".staging"))
		if err != nil {
			return fmt.Errorf("read backup staging directory: %w", err)
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			stageDirectory := filepath.Join(repository.root, ".staging", entry.Name())
			if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".tmp-") {
				if err := os.RemoveAll(stageDirectory); err != nil {
					return fmt.Errorf("remove incomplete backup staging data: %w", err)
				}
				continue
			}

			staged, err := readPublishedBackup(stageDirectory)
			if err != nil {
				if removeErr := os.RemoveAll(stageDirectory); removeErr != nil {
					return fmt.Errorf("remove invalid backup staging data after %v: %w", err, removeErr)
				}
				continue
			}
			if entry.Name() != staged.Idempotency.Identity() {
				if err := os.RemoveAll(stageDirectory); err != nil {
					return fmt.Errorf("remove mismatched backup staging data: %w", err)
				}
				continue
			}

			objectDirectory := repository.objectDirectory(staged.Manifest.ID)
			if _, err := os.Stat(objectDirectory); err == nil {
				existing, err := readPublishedBackup(objectDirectory)
				if err != nil {
					return fmt.Errorf("read recovered backup target: %w", err)
				}
				if err := matchPublishedBackups(existing, staged); err != nil {
					return err
				}
				if err := os.RemoveAll(stageDirectory); err != nil {
					return fmt.Errorf("remove duplicate backup staging data: %w", err)
				}
				continue
			} else if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("inspect recovered backup target: %w", err)
			}

			_, err = repository.publishStaged(staged, stageDirectory)
			if errors.Is(err, ErrBackupQuotaExceeded) {
				continue
			}
			if err != nil {
				return err
			}
		}
		objects, err := os.ReadDir(filepath.Join(repository.root, "objects"))
		if err != nil {
			return err
		}
		for _, entry := range objects {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !entry.IsDir() {
				if strings.HasPrefix(entry.Name(), ".health-") {
					if err := os.Remove(filepath.Join(repository.root, "objects", entry.Name())); err != nil {
						return err
					}
				}
				continue
			}
			publication, err := readPublishedBackup(filepath.Join(repository.root, "objects", entry.Name()))
			if err != nil {
				return fmt.Errorf("read published backup %q: %w", entry.Name(), err)
			}
			if publication.Manifest.ID != entry.Name() {
				return ErrPublicationConflict
			}
			recovered = append(recovered, publication)
		}
		return syncDirectory(filepath.Join(repository.root, ".staging"))
	})
	if err != nil {
		return nil, err
	}
	return recovered, nil
}

func (repository *LocalBackupRepository) OpenBundle(ctx context.Context, objectKey string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	manifestID, err := manifestIDFromObjectKey(objectKey)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(filepath.Join(repository.objectDirectory(manifestID), "bundle.zip"))
	if err != nil {
		return nil, fmt.Errorf("open backup bundle: %w", err)
	}
	return file, nil
}

func (repository *LocalBackupRepository) Usage(ctx context.Context) (int64, error) {
	var usage int64
	err := repository.withWriterLock(func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, directory := range []string{repository.root, filepath.Join(repository.root, ".staging"), filepath.Join(repository.root, "objects")} {
			probe, err := os.CreateTemp(directory, ".health-")
			if err != nil {
				return fmt.Errorf("backup storage is not writable: %w", err)
			}
			_, writeErr := probe.Write([]byte{0})
			closeErr := probe.Close()
			removeErr := os.Remove(probe.Name())
			if err := errors.Join(writeErr, closeErr, removeErr); err != nil {
				return fmt.Errorf("check backup storage: %w", err)
			}
		}
		var err error
		usage, err = repository.usageLocked()
		return err
	})
	return usage, err
}

func (repository *LocalBackupRepository) Delete(ctx context.Context, manifestID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return repository.withWriterLock(func() error {
		directory := repository.objectDirectory(manifestID)
		if err := os.RemoveAll(directory); err != nil {
			return fmt.Errorf("delete backup object: %w", err)
		}
		return syncDirectory(filepath.Join(repository.root, "objects"))
	})
}

func (repository *LocalBackupRepository) createStage(ctx context.Context, request PublishBackupRequest) (PublishedBackup, error) {
	if len(request.Items) == 0 {
		return PublishedBackup{}, errors.New("backup publication requires at least one item")
	}
	items := append([]BackupPublicationItem(nil), request.Items...)
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })

	stagingRoot := filepath.Join(repository.root, ".staging")
	temporaryDirectory, err := os.MkdirTemp(stagingRoot, ".tmp-"+request.Idempotency.Identity()+"-")
	if err != nil {
		return PublishedBackup{}, fmt.Errorf("create private backup staging directory: %w", err)
	}
	defer os.RemoveAll(temporaryDirectory)
	if err := os.Chmod(temporaryDirectory, 0o700); err != nil {
		return PublishedBackup{}, fmt.Errorf("secure backup staging directory: %w", err)
	}
	payloadDirectory := filepath.Join(temporaryDirectory, "payloads")
	if err := os.Mkdir(payloadDirectory, 0o700); err != nil {
		return PublishedBackup{}, fmt.Errorf("create backup payload staging directory: %w", err)
	}

	manifestItems := make([]BackupManifestItem, 0, len(items))
	seenNames := make(map[string]struct{}, len(items))
	var rawSize int64
	for index, item := range items {
		if err := ctx.Err(); err != nil {
			return PublishedBackup{}, err
		}
		if err := validateBackupID("publication item name", item.Name); err != nil {
			return PublishedBackup{}, fmt.Errorf("items[%d]: %w", index, err)
		}
		if _, exists := seenNames[item.Name]; exists {
			return PublishedBackup{}, fmt.Errorf("items[%d]: duplicate item name %q", index, item.Name)
		}
		seenNames[item.Name] = struct{}{}
		if item.Kind == "" {
			item.Kind = BackupItemFile
		}
		if item.Kind != BackupItemFile && item.Kind != BackupItemDirectory {
			return PublishedBackup{}, fmt.Errorf("items[%d]: unsupported item kind %q", index, item.Kind)
		}
		if err := ValidateBackupRelativePath(item.SourcePath); err != nil {
			return PublishedBackup{}, fmt.Errorf("items[%d]: %w", index, err)
		}
		if !validItemConsistency(item.Consistency) {
			return PublishedBackup{}, fmt.Errorf("items[%d]: unsupported consistency %q", index, item.Consistency)
		}
		if item.Content == nil {
			return PublishedBackup{}, fmt.Errorf("items[%d]: content is required", index)
		}

		objectKey := opaqueItemObjectKey(item.Name, item.SourcePath)
		payloadPath := filepath.Join(payloadDirectory, strings.TrimPrefix(objectKey, "items/"))
		size, digest, err := repository.writePayload(ctx, payloadPath, item.Content, rawSize)
		if err != nil {
			return PublishedBackup{}, fmt.Errorf("stage item %q: %w", item.Name, err)
		}
		rawSize += size
		manifestItems = append(manifestItems, BackupManifestItem{
			Name:        item.Name,
			Kind:        item.Kind,
			SourcePath:  item.SourcePath,
			Size:        size,
			SHA256:      digest,
			ObjectKey:   objectKey,
			Consistency: item.Consistency,
		})
	}

	manifest := BackupManifest{
		SchemaVersion:      1,
		ID:                 request.Manifest.ID,
		DefinitionID:       request.Manifest.DefinitionID,
		DefinitionRevision: request.Manifest.DefinitionRevision,
		ServerID:           request.Manifest.ServerID,
		ServerType:         request.Manifest.ServerType,
		Source:             request.Manifest.Source,
		CreatedAt:          request.Manifest.CreatedAt.UTC(),
		Items:              manifestItems,
	}
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return PublishedBackup{}, fmt.Errorf("encode backup manifest: %w", err)
	}
	manifestData = append(manifestData, '\n')

	bundlePath := filepath.Join(temporaryDirectory, "bundle.zip")
	if err := createDeterministicBundle(bundlePath, manifestData, payloadDirectory, manifestItems); err != nil {
		return PublishedBackup{}, err
	}
	bundleSize, bundleDigest, err := fileDigest(bundlePath)
	if err != nil {
		return PublishedBackup{}, fmt.Errorf("digest backup bundle: %w", err)
	}
	if repository.maxBackupBytes > 0 && bundleSize > repository.maxBackupBytes {
		return PublishedBackup{}, fmt.Errorf("%w: bundle is %d bytes and the per-backup limit is %d", ErrBackupQuotaExceeded, bundleSize, repository.maxBackupBytes)
	}

	published := PublishedBackup{
		Idempotency: request.Idempotency,
		Manifest:    manifest,
		Bundle: BackupObject{
			Key:    objectKeyForManifest(manifest.ID),
			Size:   bundleSize,
			SHA256: bundleDigest,
		},
	}
	if err := writeJSONFile(filepath.Join(temporaryDirectory, ".publication.json"), published); err != nil {
		return PublishedBackup{}, err
	}
	if err := os.RemoveAll(payloadDirectory); err != nil {
		return PublishedBackup{}, fmt.Errorf("remove staged item payloads: %w", err)
	}
	if err := syncDirectory(temporaryDirectory); err != nil {
		return PublishedBackup{}, err
	}

	stageDirectory := repository.stageDirectory(request.Idempotency.Identity())
	if err := os.Rename(temporaryDirectory, stageDirectory); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return PublishedBackup{}, fmt.Errorf("commit backup staging data: %w", err)
		}
		existing, readErr := readPublishedBackup(stageDirectory)
		if readErr != nil {
			return PublishedBackup{}, fmt.Errorf("read concurrent backup staging data: %w", readErr)
		}
		if err := matchPublishedBackups(existing, published); err != nil {
			return PublishedBackup{}, err
		}
		return existing, nil
	}
	if err := syncDirectory(stagingRoot); err != nil {
		return PublishedBackup{}, err
	}
	return published, nil
}

func (repository *LocalBackupRepository) writePayload(ctx context.Context, destination string, source io.Reader, existingSize int64) (int64, string, error) {
	return writeBackupPayload(ctx, destination, source, existingSize, repository.maxItemBytes, repository.maxBackupBytes)
}

// writeBackupPayload streams source into destination while hashing it,
// enforcing per-item and per-backup byte limits. Shared by the local
// filesystem and S3 backup repositories so both stage payloads identically.
func writeBackupPayload(ctx context.Context, destination string, source io.Reader, existingSize, maxItemBytes, maxBackupBytes int64) (int64, string, error) {
	file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, "", err
	}
	hash := sha256.New()
	reader := io.Reader(&contextReader{ctx: ctx, reader: source})
	if maxItemBytes > 0 && maxItemBytes < int64(^uint64(0)>>1) {
		reader = io.LimitReader(reader, maxItemBytes+1)
	}
	if maxBackupBytes > 0 {
		remaining := maxBackupBytes - existingSize
		if remaining < 0 {
			remaining = 0
		}
		if remaining < int64(^uint64(0)>>1) {
			reader = io.LimitReader(reader, remaining+1)
		}
	}
	size, copyErr := io.Copy(io.MultiWriter(file, hash), reader)
	syncErr := file.Sync()
	closeErr := file.Close()
	if copyErr != nil {
		return 0, "", copyErr
	}
	if syncErr != nil {
		return 0, "", syncErr
	}
	if closeErr != nil {
		return 0, "", closeErr
	}
	if maxItemBytes > 0 && size > maxItemBytes {
		return 0, "", fmt.Errorf("%w: item exceeds the limit of %d bytes", ErrBackupQuotaExceeded, maxItemBytes)
	}
	if maxBackupBytes > 0 && existingSize+size > maxBackupBytes {
		return 0, "", fmt.Errorf("%w: item contents exceed the per-backup limit of %d bytes", ErrBackupQuotaExceeded, maxBackupBytes)
	}
	return size, hex.EncodeToString(hash.Sum(nil)), nil
}

func (repository *LocalBackupRepository) publishStaged(staged PublishedBackup, stageDirectory string) (publication PublishedBackup, err error) {
	defer func() {
		if errors.Is(err, ErrBackupQuotaExceeded) {
			if cleanupErr := errors.Join(os.RemoveAll(stageDirectory), syncDirectory(filepath.Dir(stageDirectory))); cleanupErr != nil {
				err = fmt.Errorf("clean rejected stage after %v: %w", err, cleanupErr)
			}
		}
	}()
	for _, item := range staged.Manifest.Items {
		if repository.maxItemBytes > 0 && item.Size > repository.maxItemBytes {
			return PublishedBackup{}, fmt.Errorf("%w: staged item exceeds the limit of %d bytes", ErrBackupQuotaExceeded, repository.maxItemBytes)
		}
	}
	if repository.maxBackupBytes > 0 && staged.Bundle.Size > repository.maxBackupBytes {
		return PublishedBackup{}, fmt.Errorf("%w: bundle is %d bytes and the per-backup limit is %d", ErrBackupQuotaExceeded, staged.Bundle.Size, repository.maxBackupBytes)
	}
	usage, err := repository.usageLocked()
	if err != nil {
		return PublishedBackup{}, err
	}
	if repository.maxRepositoryBytes > 0 && (usage > repository.maxRepositoryBytes || staged.Bundle.Size > repository.maxRepositoryBytes-usage) {
		return PublishedBackup{}, fmt.Errorf("%w: repository uses %d bytes and the new bundle needs %d of %d bytes", ErrBackupQuotaExceeded, usage, staged.Bundle.Size, repository.maxRepositoryBytes)
	}

	objectDirectory := repository.objectDirectory(staged.Manifest.ID)
	if _, err := os.Stat(objectDirectory); err == nil {
		existing, err := readPublishedBackup(objectDirectory)
		if err != nil {
			return PublishedBackup{}, fmt.Errorf("read concurrent backup publication: %w", err)
		}
		if err := matchPublishedBackups(existing, staged); err != nil {
			return PublishedBackup{}, err
		}
		if err := os.RemoveAll(stageDirectory); err != nil {
			return PublishedBackup{}, fmt.Errorf("remove duplicate staged publication: %w", err)
		}
		return existing, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return PublishedBackup{}, fmt.Errorf("inspect backup object target: %w", err)
	}

	if err := os.Rename(stageDirectory, objectDirectory); err != nil {
		return PublishedBackup{}, fmt.Errorf("publish backup bundle: %w", err)
	}
	if err := syncDirectory(filepath.Join(repository.root, "objects")); err != nil {
		return PublishedBackup{}, err
	}
	if err := syncDirectory(filepath.Join(repository.root, ".staging")); err != nil {
		return PublishedBackup{}, err
	}
	return staged, nil
}

func (repository *LocalBackupRepository) usageLocked() (int64, error) {
	entries, err := os.ReadDir(filepath.Join(repository.root, "objects"))
	if err != nil {
		return 0, fmt.Errorf("read backup object directory: %w", err)
	}
	var usage int64
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		directory := filepath.Join(repository.root, "objects", entry.Name())
		published, err := readBackupPublicationMetadata(directory)
		if err != nil {
			return 0, fmt.Errorf("read backup object %q: %w", entry.Name(), err)
		}
		if published.Manifest.ID != entry.Name() {
			return 0, ErrPublicationConflict
		}
		info, err := os.Lstat(filepath.Join(directory, "bundle.zip"))
		if err != nil {
			return 0, fmt.Errorf("stat backup bundle %q: %w", entry.Name(), err)
		}
		if !info.Mode().IsRegular() || info.Size() != published.Bundle.Size {
			return 0, fmt.Errorf("backup bundle %q is not a regular file matching its publication size", entry.Name())
		}
		if published.Bundle.Size > int64(^uint64(0)>>1)-usage {
			return 0, errors.New("backup repository usage overflow")
		}
		usage += published.Bundle.Size
	}
	return usage, nil
}

func (repository *LocalBackupRepository) withWriterLock(action func() error) error {
	repository.mu.Lock()
	defer repository.mu.Unlock()

	lock, err := os.OpenFile(filepath.Join(repository.root, ".writer.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open backup repository writer lock: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("acquire backup repository writer lock: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	return action()
}

func (repository *LocalBackupRepository) stageDirectory(identity string) string {
	return filepath.Join(repository.root, ".staging", identity)
}

func (repository *LocalBackupRepository) objectDirectory(manifestID string) string {
	return filepath.Join(repository.root, "objects", manifestID)
}

func createDeterministicBundle(destination string, manifest []byte, payloadDirectory string, items []BackupManifestItem) error {
	file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create backup bundle: %w", err)
	}
	archive := zip.NewWriter(file)
	writeEntry := func(name string, source io.Reader) error {
		header := &zip.FileHeader{Name: name, Method: zip.Store}
		header.SetMode(0o600)
		header.SetModTime(time.Date(1980, time.January, 1, 0, 0, 0, 0, time.UTC))
		writer, err := archive.CreateHeader(header)
		if err != nil {
			return err
		}
		_, err = io.Copy(writer, source)
		return err
	}
	if err := writeEntry("manifest.json", bytes.NewReader(manifest)); err != nil {
		archive.Close()
		file.Close()
		return fmt.Errorf("write backup manifest to bundle: %w", err)
	}
	for _, item := range items {
		payload, err := os.Open(filepath.Join(payloadDirectory, strings.TrimPrefix(item.ObjectKey, "items/")))
		if err != nil {
			archive.Close()
			file.Close()
			return fmt.Errorf("open staged backup item %q: %w", item.Name, err)
		}
		writeErr := writeEntry(item.ObjectKey, payload)
		closeErr := payload.Close()
		if writeErr != nil {
			archive.Close()
			file.Close()
			return fmt.Errorf("write backup item %q to bundle: %w", item.Name, writeErr)
		}
		if closeErr != nil {
			archive.Close()
			file.Close()
			return fmt.Errorf("close staged backup item %q: %w", item.Name, closeErr)
		}
	}
	if err := archive.Close(); err != nil {
		file.Close()
		return fmt.Errorf("finish backup bundle: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("sync backup bundle: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close backup bundle: %w", err)
	}
	return nil
}

func readPublishedBackup(directory string) (PublishedBackup, error) {
	published, err := readBackupPublicationMetadata(directory)
	if err != nil {
		return PublishedBackup{}, err
	}
	size, digest, err := fileDigest(filepath.Join(directory, "bundle.zip"))
	if err != nil {
		return PublishedBackup{}, err
	}
	if size != published.Bundle.Size || digest != published.Bundle.SHA256 {
		return PublishedBackup{}, errors.New("backup bundle does not match its publication record")
	}
	return published, nil
}

func readBackupPublicationMetadata(directory string) (PublishedBackup, error) {
	record, err := os.ReadFile(filepath.Join(directory, ".publication.json"))
	if err != nil {
		return PublishedBackup{}, err
	}
	var published PublishedBackup
	if err := json.Unmarshal(record, &published); err != nil {
		return PublishedBackup{}, err
	}
	if err := validatePublishedBackup(published); err != nil {
		return PublishedBackup{}, err
	}
	return published, nil
}

func validatePublishedBackup(published PublishedBackup) error {
	if err := validateIdempotencyRequest(published.Idempotency); err != nil {
		return err
	}
	manifest := published.Manifest
	if manifest.SchemaVersion != 1 {
		return fmt.Errorf("unsupported backup manifest schema version %d", manifest.SchemaVersion)
	}
	if err := validateManifestDraft(BackupManifestDraft{
		ID:                 manifest.ID,
		DefinitionID:       manifest.DefinitionID,
		DefinitionRevision: manifest.DefinitionRevision,
		ServerID:           manifest.ServerID,
		ServerType:         manifest.ServerType,
		Source:             manifest.Source,
		CreatedAt:          manifest.CreatedAt,
	}); err != nil {
		return err
	}
	if len(manifest.Items) == 0 {
		return errors.New("backup manifest contains no items")
	}
	seen := make(map[string]struct{}, len(manifest.Items))
	for index, item := range manifest.Items {
		if err := validateBackupID("manifest item name", item.Name); err != nil {
			return fmt.Errorf("manifest items[%d]: %w", index, err)
		}
		if _, exists := seen[item.Name]; exists {
			return fmt.Errorf("manifest items[%d]: duplicate name %q", index, item.Name)
		}
		seen[item.Name] = struct{}{}
		if item.Kind != "" && item.Kind != BackupItemFile && item.Kind != BackupItemDirectory {
			return fmt.Errorf("manifest items[%d]: invalid kind", index)
		}
		if err := ValidateBackupRelativePath(item.SourcePath); err != nil {
			return fmt.Errorf("manifest items[%d]: %w", index, err)
		}
		if item.Size < 0 || !validSHA256(item.SHA256) {
			return fmt.Errorf("manifest items[%d]: invalid size or sha256", index)
		}
		if item.ObjectKey != opaqueItemObjectKey(item.Name, item.SourcePath) {
			return fmt.Errorf("manifest items[%d]: invalid object key", index)
		}
		if !validItemConsistency(item.Consistency) {
			return fmt.Errorf("manifest items[%d]: invalid consistency", index)
		}
	}
	if published.Bundle.Key != objectKeyForManifest(manifest.ID) || published.Bundle.Size < 0 || !validSHA256(published.Bundle.SHA256) {
		return errors.New("invalid backup bundle metadata")
	}
	return nil
}

func validateManifestDraft(manifest BackupManifestDraft) error {
	if err := validateBackupID("manifest id", manifest.ID); err != nil {
		return err
	}
	if err := validateBackupID("manifest definition id", manifest.DefinitionID); err != nil {
		return err
	}
	if manifest.DefinitionRevision < 1 {
		return errors.New("manifest definition revision must be at least 1")
	}
	if err := validateBackupID("manifest server id", manifest.ServerID); err != nil {
		return err
	}
	if err := validateBackupID("manifest server type", manifest.ServerType); err != nil {
		return err
	}
	if manifest.Source != BackupSourceRunningBAK && manifest.Source != BackupSourceStoppedSAV {
		return fmt.Errorf("unsupported backup source %q", manifest.Source)
	}
	if manifest.CreatedAt.IsZero() {
		return errors.New("manifest creation time is required")
	}
	return nil
}

func validateIdempotencyRequest(request IdempotencyRequest) error {
	if strings.TrimSpace(request.Actor) == "" || strings.TrimSpace(request.Operation) == "" || strings.TrimSpace(request.Key) == "" {
		return errors.New("idempotency actor, operation, and key are required")
	}
	if !validSHA256(request.RequestDigest) {
		return errors.New("idempotency request digest must be a lowercase sha256")
	}
	return nil
}

func validItemConsistency(consistency BackupItemConsistency) bool {
	return consistency == BackupConsistencyAtomicPublish || consistency == BackupConsistencyStableRead || consistency == BackupConsistencyStoppedWorld
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func opaqueItemObjectKey(name, sourcePath string) string {
	sum := sha256.Sum256([]byte(name + "\x00" + sourcePath))
	return "items/" + hex.EncodeToString(sum[:])
}

func objectKeyForManifest(manifestID string) string {
	return "objects/" + manifestID + "/bundle.zip"
}

func manifestIDFromObjectKey(objectKey string) (string, error) {
	parts := strings.Split(objectKey, "/")
	if len(parts) != 3 || parts[0] != "objects" || parts[2] != "bundle.zip" {
		return "", errors.New("invalid backup object key")
	}
	if err := validateBackupID("manifest id", parts[1]); err != nil {
		return "", err
	}
	return parts[1], nil
}

func matchPublication(existing PublishedBackup, incoming IdempotencyRequest, manifestID string) error {
	if existing.Manifest.ID != manifestID {
		return ErrPublicationConflict
	}
	if err := CheckIdempotency(existing.Idempotency, incoming); err != nil {
		if errors.Is(err, ErrIdempotencyConflict) {
			return ErrPublicationConflict
		}
		return ErrPublicationConflict
	}
	return nil
}

func matchPublishedBackups(existing, incoming PublishedBackup) error {
	if err := matchPublication(existing, incoming.Idempotency, incoming.Manifest.ID); err != nil {
		return err
	}
	if existing.Bundle != incoming.Bundle {
		return ErrPublicationConflict
	}
	existingManifest, _ := json.Marshal(existing.Manifest)
	incomingManifest, _ := json.Marshal(incoming.Manifest)
	if !bytes.Equal(existingManifest, incomingManifest) {
		return ErrPublicationConflict
	}
	return nil
}

func fileDigest(path string) (int64, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	hash := sha256.New()
	size, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if copyErr != nil {
		return 0, "", copyErr
	}
	if closeErr != nil {
		return 0, "", closeErr
	}
	return size, hex.EncodeToString(hash.Sum(nil)), nil
}

func writeJSONFile(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode backup publication record: %w", err)
	}
	data = append(data, '\n')
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create backup publication record: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return fmt.Errorf("write backup publication record: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("sync backup publication record: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close backup publication record: %w", err)
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open directory for sync: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync directory: %w", err)
	}
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *contextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}
