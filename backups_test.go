package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestBackupDefinitionValidation(t *testing.T) {
	definition := testBackupDefinition()
	if err := definition.Validate(); err != nil {
		t.Fatalf("valid definition was rejected: %v", err)
	}

	badPaths := []string{
		"/absolute/world.sav",
		"../world.sav",
		"SaveGames/../world.sav",
		"SaveGames\\world.sav",
		"SaveGames//world.sav",
	}
	for _, badPath := range badPaths {
		t.Run(badPath, func(t *testing.T) {
			invalid := testBackupDefinition()
			invalid.Items[0].Sources[0].Path = badPath
			if err := invalid.Validate(); err == nil {
				t.Fatalf("unsafe source path %q was accepted", badPath)
			}
		})
	}

	duplicate := testBackupDefinition()
	duplicate.Items = append(duplicate.Items, duplicate.Items[0])
	if err := duplicate.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate item name") {
		t.Fatalf("duplicate item names returned %v", err)
	}

	unsupportedStrategy := testBackupDefinition()
	unsupportedStrategy.Strategy = BackupStrategyVolumeSnapshot
	if err := unsupportedStrategy.Validate(); err == nil || !strings.Contains(err.Error(), "unsupported backup strategy") {
		t.Fatalf("future strategy returned %v", err)
	}

	unsupportedCollector := testBackupDefinition()
	unsupportedCollector.Items[0].Sources[0].Collector = "shell"
	if err := unsupportedCollector.Validate(); err == nil || !strings.Contains(err.Error(), "unsupported collector") {
		t.Fatalf("unsupported collector returned %v", err)
	}

	duplicateState := testBackupDefinition()
	duplicateState.Items[0].Sources = append(duplicateState.Items[0].Sources, duplicateState.Items[0].Sources[0])
	if err := duplicateState.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate \"running\" source") {
		t.Fatalf("duplicate source state returned %v", err)
	}
}

func TestLocalBackupRepositoryPublishesMultiItemBundle(t *testing.T) {
	repository := testRepository(t, LocalBackupRepositoryConfig{Root: t.TempDir()})
	request := testPublicationRequest(t, "backup-multi", "request-multi", []testItem{
		{name: "world", path: "RSDragonwilds/Saved/SaveGames/World.sav.backup", content: "world-data", consistency: BackupConsistencyAtomicPublish},
		{name: "settings", path: "RSDragonwilds/Saved/Config/LinuxServer/DedicatedServer.ini", content: "setting=true", consistency: BackupConsistencyStableRead},
	})

	published, err := repository.Publish(context.Background(), request)
	if err != nil {
		t.Fatalf("publish multi-item backup: %v", err)
	}
	if published.Manifest.SchemaVersion != 1 || len(published.Manifest.Items) != 2 {
		t.Fatalf("unexpected manifest shape: %#v", published.Manifest)
	}
	if published.Manifest.Items[0].Name != "settings" || published.Manifest.Items[1].Name != "world" {
		t.Fatalf("manifest items are not deterministic: %#v", published.Manifest.Items)
	}
	if published.Manifest.Items[1].Size != int64(len("world-data")) {
		t.Fatalf("world item size = %d", published.Manifest.Items[1].Size)
	}
	if published.Manifest.Items[1].SHA256 != "d6077a74658ac843cb1cf9e794884dde7a533812c7654ad8845bc5c059f82a71" {
		t.Fatalf("world item sha256 = %s", published.Manifest.Items[1].SHA256)
	}

	entries := readBundle(t, repository, published.Bundle.Key)
	if string(entries["manifest.json"]) == "" {
		t.Fatal("bundle does not contain manifest.json")
	}
	for _, item := range published.Manifest.Items {
		content, exists := entries[item.ObjectKey]
		if !exists {
			t.Fatalf("bundle does not contain %s", item.ObjectKey)
		}
		if item.Name == "world" && string(content) != "world-data" {
			t.Fatalf("world bundle content = %q", content)
		}
	}
	if len(entries) != 3 {
		t.Fatalf("bundle has %d entries, want manifest plus two items", len(entries))
	}

	var bundledManifest BackupManifest
	if err := json.Unmarshal(entries["manifest.json"], &bundledManifest); err != nil {
		t.Fatalf("decode bundled manifest: %v", err)
	}
	if !reflect.DeepEqual(bundledManifest, published.Manifest) {
		t.Fatalf("bundled manifest differs from publication\nbundled: %#v\npublished: %#v", bundledManifest, published.Manifest)
	}
}

func TestLocalBackupRepositoryAlwaysBundlesOneItem(t *testing.T) {
	repository := testRepository(t, LocalBackupRepositoryConfig{Root: t.TempDir()})
	request := testPublicationRequest(t, "backup-single", "request-single", []testItem{
		{name: "world", path: "RSDragonwilds/Saved/SaveGames/World.sav", content: "one-world", consistency: BackupConsistencyStoppedWorld},
	})
	request.Manifest.Source = BackupSourceStoppedSAV

	published, err := repository.Publish(context.Background(), request)
	if err != nil {
		t.Fatalf("publish one-item backup: %v", err)
	}
	entries := readBundle(t, repository, published.Bundle.Key)
	if len(entries) != 2 {
		t.Fatalf("one-item bundle has %d entries, want manifest plus one item", len(entries))
	}
	if _, exists := entries["manifest.json"]; !exists {
		t.Fatal("one-item bundle does not contain manifest.json")
	}
	if string(entries[published.Manifest.Items[0].ObjectKey]) != "one-world" {
		t.Fatal("one-item bundle does not contain the save data")
	}
}

func TestLocalBackupRepositoryQuotaPreservesCompletedObjects(t *testing.T) {
	repository := testRepository(t, LocalBackupRepositoryConfig{Root: t.TempDir()})
	firstRequest := testPublicationRequest(t, "backup-first", "request-first", []testItem{
		{name: "world", path: "SaveGames/World.sav.backup", content: "first-world", consistency: BackupConsistencyAtomicPublish},
	})
	first, err := repository.Publish(context.Background(), firstRequest)
	if err != nil {
		t.Fatalf("publish first backup: %v", err)
	}
	usageBefore, err := repository.Usage(context.Background())
	if err != nil {
		t.Fatalf("read repository usage: %v", err)
	}
	repository.maxRepositoryBytes = usageBefore + 1

	secondRequest := testPublicationRequest(t, "backup-second", "request-second", []testItem{
		{name: "world", path: "SaveGames/World.sav.backup", content: strings.Repeat("second", 32), consistency: BackupConsistencyAtomicPublish},
	})
	if _, err := repository.Publish(context.Background(), secondRequest); !errors.Is(err, ErrBackupQuotaExceeded) {
		t.Fatalf("publish beyond quota returned %v", err)
	}
	usageAfter, err := repository.Usage(context.Background())
	if err != nil {
		t.Fatalf("read repository usage after rejection: %v", err)
	}
	if usageAfter != usageBefore {
		t.Fatalf("repository usage changed after quota rejection: before %d, after %d", usageBefore, usageAfter)
	}
	entries := readBundle(t, repository, first.Bundle.Key)
	if string(entries[first.Manifest.Items[0].ObjectKey]) != "first-world" {
		t.Fatal("completed backup changed after quota rejection")
	}
	if _, err := os.Stat(repository.objectDirectory("backup-second")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected backup was published: %v", err)
	}
}

func TestLocalBackupRepositoryRetryAndConflict(t *testing.T) {
	repository := testRepository(t, LocalBackupRepositoryConfig{Root: t.TempDir()})
	firstRequest := testPublicationRequest(t, "backup-retry", "request-retry", []testItem{
		{name: "world", path: "SaveGames/World.sav.backup", content: "stable-world", consistency: BackupConsistencyAtomicPublish},
	})
	first, err := repository.Publish(context.Background(), firstRequest)
	if err != nil {
		t.Fatalf("publish first attempt: %v", err)
	}

	retryRequest := testPublicationRequest(t, "backup-retry", "request-retry", []testItem{
		{name: "world", path: "SaveGames/World.sav.backup", content: "content-is-not-read-on-an-idempotent-retry", consistency: BackupConsistencyAtomicPublish},
	})
	retried, err := repository.Publish(context.Background(), retryRequest)
	if err != nil {
		t.Fatalf("retry same publication: %v", err)
	}
	if retried.Bundle != first.Bundle || !reflect.DeepEqual(retried.Manifest, first.Manifest) {
		t.Fatalf("idempotent retry changed publication: first %#v, retry %#v", first, retried)
	}

	conflict := firstRequest
	conflict.Idempotency = testIdempotency(t, "request-retry", map[string]any{"manifest": "backup-retry", "changed": true})
	if _, err := repository.Publish(context.Background(), conflict); !errors.Is(err, ErrPublicationConflict) {
		t.Fatalf("conflicting retry returned %v", err)
	}

	objects, err := os.ReadDir(filepath.Join(repository.root, "objects"))
	if err != nil {
		t.Fatalf("read published objects: %v", err)
	}
	if len(objects) != 1 || objects[0].Name() != "backup-retry" {
		t.Fatalf("retry created unexpected objects: %#v", objects)
	}
}

func TestIdempotencyRequestUsesCanonicalRequestDigest(t *testing.T) {
	first, err := NewIdempotencyRequest("admin", "backup.run", "same-key", map[string]any{"server": "world", "profile": "default"})
	if err != nil {
		t.Fatalf("create first idempotency request: %v", err)
	}
	second, err := NewIdempotencyRequest("admin", "backup.run", "same-key", map[string]any{"profile": "default", "server": "world"})
	if err != nil {
		t.Fatalf("create reordered idempotency request: %v", err)
	}
	if err := CheckIdempotency(first, second); err != nil {
		t.Fatalf("canonical equivalent requests conflict: %v", err)
	}
	changed, err := NewIdempotencyRequest("admin", "backup.run", "same-key", map[string]any{"server": "other", "profile": "default"})
	if err != nil {
		t.Fatalf("create changed idempotency request: %v", err)
	}
	if err := CheckIdempotency(first, changed); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed request returned %v", err)
	}
}

func TestBackupDefinitionYAMLRoundTripAndRejectsGeneratedFields(t *testing.T) {
	want := testBackupDefinition()
	data, err := ExportBackupDefinitionYAML(want)
	if err != nil {
		t.Fatalf("export backup definition: %v", err)
	}
	got, err := ImportBackupDefinitionYAML(data)
	if err != nil {
		t.Fatalf("import exported backup definition: %v\n%s", err, data)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("yaml round trip changed definition\nwant: %#v\ngot: %#v", want, got)
	}

	withGeneratedField := string(data) + "sha256: forbidden\n"
	if _, err := ImportBackupDefinitionYAML([]byte(withGeneratedField)); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("generated manifest field returned %v", err)
	}

	wrongVersion := strings.Replace(string(data), BackupDefinitionAPIVersion, "rsdw-c2.petzko.dev/v2", 1)
	if _, err := ImportBackupDefinitionYAML([]byte(wrongVersion)); err == nil || !strings.Contains(err.Error(), "unsupported backup definition apiVersion") {
		t.Fatalf("unsupported yaml version returned %v", err)
	}

	multipleDocuments := string(data) + "---\n" + string(data)
	if _, err := ImportBackupDefinitionYAML([]byte(multipleDocuments)); err == nil || !strings.Contains(err.Error(), "exactly one document") {
		t.Fatalf("multiple yaml documents returned %v", err)
	}
}

func TestLocalStorageBackendCannotBeRemoved(t *testing.T) {
	backend := NewLocalStorageBackend(StorageBackendConnected)
	if err := backend.Validate(); err != nil {
		t.Fatalf("valid local backend was rejected: %v", err)
	}
	if backend.CanRemove() {
		t.Fatal("local backend is removable")
	}
	backend.Removable = true
	if err := backend.Validate(); err == nil || !strings.Contains(err.Error(), "cannot be removable") {
		t.Fatalf("removable local backend returned %v", err)
	}
}

func TestLocalBackupRepositoryRecoversValidStagingAndCleansInvalidStaging(t *testing.T) {
	root := t.TempDir()
	repository := testRepository(t, LocalBackupRepositoryConfig{Root: root})
	request := testPublicationRequest(t, "backup-recovered", "request-recovered", []testItem{
		{name: "world", path: "SaveGames/World.sav.backup", content: "recover-me", consistency: BackupConsistencyStableRead},
	})
	staged, err := repository.createStage(context.Background(), request)
	if err != nil {
		t.Fatalf("create interrupted staging fixture: %v", err)
	}
	invalidStage := filepath.Join(root, ".staging", "invalid")
	if err := os.Mkdir(invalidStage, 0o700); err != nil {
		t.Fatalf("create invalid staging fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(invalidStage, "partial"), []byte("partial"), 0o600); err != nil {
		t.Fatalf("write invalid staging fixture: %v", err)
	}
	if _, err := os.Stat(repository.objectDirectory(staged.Manifest.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged backup was already published: %v", err)
	}

	restarted := testRepository(t, LocalBackupRepositoryConfig{Root: root})
	recovered, err := restarted.Recover(context.Background())
	if err != nil {
		t.Fatalf("recover staged backup: %v", err)
	}
	if len(recovered) != 1 || recovered[0].Bundle != staged.Bundle {
		t.Fatalf("recovered publications = %#v", recovered)
	}
	entries := readBundle(t, restarted, recovered[0].Bundle.Key)
	if string(entries[recovered[0].Manifest.Items[0].ObjectKey]) != "recover-me" {
		t.Fatal("recovered bundle does not contain staged content")
	}
	if _, err := os.Stat(invalidStage); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid staging data remains: %v", err)
	}
	stagingEntries, err := os.ReadDir(filepath.Join(root, ".staging"))
	if err != nil {
		t.Fatalf("read staging after recovery: %v", err)
	}
	if len(stagingEntries) != 0 {
		t.Fatalf("staging is not empty after recovery: %#v", stagingEntries)
	}
}

type testItem struct {
	name        string
	path        string
	content     string
	consistency BackupItemConsistency
}

func testBackupDefinition() BackupDefinition {
	return BackupDefinition{
		APIVersion: BackupDefinitionAPIVersion,
		Kind:       BackupDefinitionKind,
		ID:         "dragonwilds-world",
		Revision:   1,
		Name:       "Dragonwilds world",
		ServerType: "dragonwilds",
		Strategy:   BackupStrategyLogicalFiles,
		Items: []BackupItemSpec{
			{
				Name:        "world",
				Requirement: BackupItemRequired,
				Sources: []BackupSourceRule{
					{ServerState: BackupServerRunning, Collector: BackupCollectorServerFiles, Path: "RSDragonwilds/Saved/SaveGames"},
					{ServerState: BackupServerStopped, Collector: BackupCollectorServerFiles, Path: "RSDragonwilds/Saved/SaveGames"},
				},
			},
		},
	}
}

func testRepository(t *testing.T, config LocalBackupRepositoryConfig) *LocalBackupRepository {
	t.Helper()
	repository, err := NewLocalBackupRepository(config)
	if err != nil {
		t.Fatalf("create local backup repository: %v", err)
	}
	return repository
}

func testPublicationRequest(t *testing.T, manifestID, key string, items []testItem) PublishBackupRequest {
	t.Helper()
	publicationItems := make([]BackupPublicationItem, 0, len(items))
	for _, item := range items {
		publicationItems = append(publicationItems, BackupPublicationItem{
			Name:        item.name,
			SourcePath:  item.path,
			Consistency: item.consistency,
			Content:     strings.NewReader(item.content),
		})
	}
	return PublishBackupRequest{
		Idempotency: testIdempotency(t, key, map[string]any{"manifest": manifestID}),
		Manifest: BackupManifestDraft{
			ID:                 manifestID,
			DefinitionID:       "dragonwilds-world",
			DefinitionRevision: 1,
			ServerID:           "test-world",
			ServerType:         "dragonwilds",
			Source:             BackupSourceRunningBAK,
			CreatedAt:          time.Date(2026, time.September, 19, 12, 0, 0, 0, time.UTC),
		},
		Items: publicationItems,
	}
}

func testIdempotency(t *testing.T, key string, request any) IdempotencyRequest {
	t.Helper()
	idempotency, err := NewIdempotencyRequest("admin", "backup.publish", key, request)
	if err != nil {
		t.Fatalf("create idempotency request: %v", err)
	}
	return idempotency
}

func readBundle(t *testing.T, repository *LocalBackupRepository, objectKey string) map[string][]byte {
	t.Helper()
	bundle, err := repository.OpenBundle(context.Background(), objectKey)
	if err != nil {
		t.Fatalf("open backup bundle: %v", err)
	}
	data, err := io.ReadAll(bundle)
	if err != nil {
		bundle.Close()
		t.Fatalf("read backup bundle: %v", err)
	}
	if err := bundle.Close(); err != nil {
		t.Fatalf("close backup bundle: %v", err)
	}
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("read backup zip: %v", err)
	}
	entries := make(map[string][]byte, len(archive.File))
	for _, file := range archive.File {
		reader, err := file.Open()
		if err != nil {
			t.Fatalf("open zip entry %q: %v", file.Name, err)
		}
		content, err := io.ReadAll(reader)
		if err != nil {
			reader.Close()
			t.Fatalf("read zip entry %q: %v", file.Name, err)
		}
		if err := reader.Close(); err != nil {
			t.Fatalf("close zip entry %q: %v", file.Name, err)
		}
		entries[file.Name] = content
	}
	return entries
}
