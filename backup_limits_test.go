package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"
)

type listingBackupRepository struct {
	BackupRepository
	calls int
	err   error
}

func (repository *listingBackupRepository) Usage(context.Context) (int64, error) {
	repository.calls++
	return 123, repository.err
}

func TestBackupListingUsesOneStorageHealthResult(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(fmt.Sprint(unavailable), func(t *testing.T) {
			app := backupTestApp(t)
			repository := &listingBackupRepository{BackupRepository: app.backups.repositories[backupLocalBackendID]}
			if unavailable {
				repository.err = errors.New("storage offline")
			}
			app.backups.repositories[backupLocalBackendID] = repository
			response := backupJSONRequest(t, app, http.MethodGet, "/api/backups", nil)
			if response.Code != http.StatusOK {
				t.Fatalf("listing = %d: %s", response.Code, response.Body.String())
			}
			view := decodeBackupResponse[struct {
				Available    bool             `json:"available"`
				StorageError string           `json:"storageError"`
				Storage      []StorageBackend `json:"storage"`
				Limits       struct {
					UsedBytes      *int64 `json:"usedBytes"`
					MaxBundleBytes int64  `json:"maxBundleBytes"`
				} `json:"limits"`
			}](t, response)
			if repository.calls != 1 || view.Available == unavailable || len(view.Storage) != 1 || view.Limits.MaxBundleBytes != 256<<20 {
				t.Fatalf("listing health = %+v, checks = %d", view, repository.calls)
			}
			if unavailable {
				if view.Limits.UsedBytes != nil || view.StorageError != "storage offline" || view.Storage[0].Status != StorageBackendDisconnected {
					t.Fatalf("unavailable listing = %+v", view)
				}
			} else if view.Limits.UsedBytes == nil || *view.Limits.UsedBytes != 123 || view.StorageError != "" || view.Storage[0].Status != StorageBackendConnected {
				t.Fatalf("healthy listing = %+v", view)
			}
		})
	}
}

func TestBackupLimitsRejectWithoutDiskLeaks(t *testing.T) {
	for _, limit := range []string{"repository", "bundle", "item"} {
		t.Run(limit, func(t *testing.T) {
			repository := testRepository(t, LocalBackupRepositoryConfig{Root: t.TempDir()})
			request := func(id string) PublishBackupRequest {
				return testPublicationRequest(t, id, id, []testItem{{name: "world", path: "world.sav.backup", content: "world-data", consistency: BackupConsistencyStableRead}})
			}
			first, err := repository.Publish(context.Background(), request("first"))
			if err != nil {
				t.Fatal(err)
			}
			switch limit {
			case "repository":
				repository.maxRepositoryBytes = first.Bundle.Size
			case "bundle":
				repository.maxBackupBytes = 1
			case "item":
				repository.maxItemBytes = 1
			}
			for attempt := range 3 {
				if _, err := repository.Publish(context.Background(), request(fmt.Sprintf("rejected-%d", attempt))); !errors.Is(err, ErrBackupQuotaExceeded) {
					t.Fatalf("quota error = %v", err)
				}
				entries, err := os.ReadDir(filepath.Join(repository.root, ".staging"))
				if err != nil || len(entries) != 0 {
					t.Fatalf("rejected backup leaked staging files: %v, %v", entries, err)
				}
			}
			objects, err := os.ReadDir(filepath.Join(repository.root, "objects"))
			if err != nil || len(objects) != 1 {
				t.Fatalf("objects after rejection = %v, %v", objects, err)
			}
			if string(readBundle(t, repository, first.Bundle.Key)[first.Manifest.Items[0].ObjectKey]) != "world-data" {
				t.Fatal("quota rejection changed published data")
			}
		})
	}
}

func TestBackupLimitsSeparateItemsFromBundle(t *testing.T) {
	repository := testRepository(t, LocalBackupRepositoryConfig{Root: t.TempDir(), MaxItemBytes: 4, MaxBackupBytes: 4096})
	request := testPublicationRequest(t, "multi", "multi", []testItem{
		{name: "one", path: "one.sav.backup", content: "1234", consistency: BackupConsistencyStableRead},
		{name: "two", path: "two.sav.backup", content: "5678", consistency: BackupConsistencyStableRead},
	})
	published, err := repository.Publish(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	entries := readBundle(t, repository, published.Bundle.Key)
	if published.Bundle.Size <= 8 || len(entries) != 3 {
		t.Fatalf("bundle = %+v; entries = %d", published.Bundle, len(entries))
	}
	for _, item := range published.Manifest.Items {
		if item.Size != 4 || len(entries[item.ObjectKey]) != 4 {
			t.Fatalf("item = %+v", item)
		}
	}
}

func TestBackupRecoveryRemovesQuotaRejectedStage(t *testing.T) {
	repository := testRepository(t, LocalBackupRepositoryConfig{Root: t.TempDir()})
	request := testPublicationRequest(t, "staged-quota", "staged-quota", []testItem{{name: "world", path: "world.sav.backup", content: "data", consistency: BackupConsistencyStableRead}})
	if _, err := repository.createStage(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	restarted := testRepository(t, LocalBackupRepositoryConfig{Root: repository.root, MaxRepositoryBytes: 1})
	publications, err := restarted.Recover(context.Background())
	if err != nil || len(publications) != 0 {
		t.Fatalf("recovery = %v, %v", publications, err)
	}
	entries, err := os.ReadDir(filepath.Join(repository.root, ".staging"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("quota stage remains: %v, %v", entries, err)
	}
}

func TestBackupStorageDetectsLostOrCorruptRepository(t *testing.T) {
	for _, damage := range []string{"missing", "corrupt", "unwritable"} {
		t.Run(damage, func(t *testing.T) {
			if damage == "unwritable" && os.Geteuid() == 0 {
				t.Skip("root bypasses directory permissions")
			}
			repository := testRepository(t, LocalBackupRepositoryConfig{Root: filepath.Join(t.TempDir(), "backups")})
			app := &App{backups: &backupController{available: true, repositories: map[string]BackupRepository{backupLocalBackendID: repository}, local: repository, root: repository.root}}
			if !app.backupAvailable() {
				t.Fatal("healthy repository unavailable")
			}
			switch damage {
			case "missing":
				if err := os.Rename(repository.root, repository.root+"-offline"); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				if err := os.Mkdir(filepath.Join(repository.root, "objects", "invalid"), 0700); err != nil {
					t.Fatal(err)
				}
			case "unwritable":
				staging := filepath.Join(repository.root, ".staging")
				if err := os.Chmod(staging, 0500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(staging, 0700) })
			}
			if _, err := app.backupStorageUsage(context.Background()); err == nil {
				t.Fatal("storage failure ignored")
			}
			if app.backupAvailable() || app.backupBackend().Status != StorageBackendDisconnected {
				t.Fatal("damaged storage reported connected")
			}
		})
	}
}

func TestBackupUsageChecksMetadataAndSizeWhileRecoveryChecksIntegrity(t *testing.T) {
	for _, damage := range []string{"none", "same-size", "missing", "truncated", "corrupt-metadata", "invalid-metadata", "nonregular"} {
		t.Run(damage, func(t *testing.T) {
			repository := testRepository(t, LocalBackupRepositoryConfig{Root: t.TempDir()})
			request := testPublicationRequest(t, "health", "health", []testItem{{name: "world", path: "world.sav.backup", content: "world-data", consistency: BackupConsistencyStableRead}})
			published, err := repository.Publish(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			directory := repository.objectDirectory(published.Manifest.ID)
			bundle := filepath.Join(directory, "bundle.zip")
			switch damage {
			case "same-size":
				data, readErr := os.ReadFile(bundle)
				if readErr != nil {
					t.Fatal(readErr)
				}
				data[len(data)/2] ^= 0xff
				err = os.WriteFile(bundle, data, 0600)
			case "missing":
				err = os.Remove(bundle)
			case "truncated":
				err = os.Truncate(bundle, published.Bundle.Size-1)
			case "corrupt-metadata":
				err = os.WriteFile(filepath.Join(directory, ".publication.json"), []byte("{"), 0600)
			case "invalid-metadata":
				err = os.WriteFile(filepath.Join(directory, ".publication.json"), []byte("{}"), 0600)
			case "nonregular":
				if err = os.Remove(bundle); err == nil {
					err = os.Mkdir(bundle, 0700)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			usage, usageErr := repository.Usage(context.Background())
			available := damage == "none" || damage == "same-size"
			if available {
				if usageErr != nil || usage != published.Bundle.Size {
					t.Fatalf("metadata and size check = %d, %v; want %d, nil", usage, usageErr, published.Bundle.Size)
				}
			} else if usageErr == nil {
				t.Fatal("damaged metadata or bundle size accepted")
			}
			app := &App{backups: &backupController{available: true, repositories: map[string]BackupRepository{backupLocalBackendID: repository}, local: repository}}
			if (app.backupBackend().Status == StorageBackendConnected) != available {
				t.Fatal("backend status disagrees with metadata and size health")
			}
			_, recoveryErr := repository.Recover(context.Background())
			if (recoveryErr == nil) != (damage == "none") {
				t.Fatalf("recovery integrity check = %v for %s", recoveryErr, damage)
			}
		})
	}
}

func TestBackupControllerConfiguredLimits(t *testing.T) {
	t.Setenv("RSDW_BACKUPS_ENABLED", "true")
	t.Setenv("RSDW_BACKUP_PATH", t.TempDir())
	for _, name := range []string{"RSDW_BACKUP_MAX_REPOSITORY_BYTES", "RSDW_BACKUP_MAX_ITEM_BYTES", "RSDW_BACKUP_MAX_BUNDLE_BYTES"} {
		t.Setenv(name, "")
	}
	controller, err := newBackupController(nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if controller.maxBytes != 9<<30 || controller.maxItem != 32<<20 || controller.maxBundle != 256<<20 {
		t.Fatalf("defaults = %+v", controller)
	}
	t.Setenv("RSDW_BACKUP_MAX_REPOSITORY_BYTES", "8192")
	t.Setenv("RSDW_BACKUP_MAX_ITEM_BYTES", "4")
	t.Setenv("RSDW_BACKUP_MAX_BUNDLE_BYTES", "4096")
	controller, err = newBackupController(nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if controller.local.maxRepositoryBytes != 8192 || controller.local.maxItemBytes != 4 || controller.local.maxBackupBytes != 4096 {
		t.Fatal("configured limits did not reach repository")
	}
	t.Setenv("RSDW_BACKUP_MAX_ITEM_BYTES", "4 trailing")
	if _, err := newBackupController(nil, true); err == nil {
		t.Fatal("invalid configured limit accepted")
	}
}

func TestBackupChartDefaultsReserveStagingCapacity(t *testing.T) {
	data, err := os.ReadFile("charts/rsdw-c2/values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		Backups struct {
			MaxRepositoryBytes int64 `json:"maxRepositoryBytes"`
			MaxItemBytes       int64 `json:"maxItemBytes"`
			MaxBundleBytes     int64 `json:"maxBundleBytes"`
			Persistence        struct {
				Size string `json:"size"`
			} `json:"persistence"`
		} `json:"backups"`
	}
	if err := yaml.Unmarshal(data, &values); err != nil {
		t.Fatal(err)
	}
	backups := values.Backups
	if backups.MaxRepositoryBytes != 9<<30 || backups.MaxBundleBytes != 256<<20 || backups.Persistence.Size != "10Gi" {
		t.Fatalf("chart staging capacity defaults = %+v", backups)
	}
	if backups.MaxRepositoryBytes != backupDefaultMaxBytes || backups.MaxBundleBytes != backupDefaultBundleSize || backups.MaxItemBytes != backupDefaultItemSize {
		t.Fatal("chart and runtime backup limits disagree")
	}
}

func TestDemoBackupCaptureBoundsAndCancellation(t *testing.T) {
	for _, kind := range []BackupItemKind{BackupItemFile, BackupItemDirectory} {
		app := &App{demo: true, backups: &backupController{maxItem: 10, maxBundle: 4096}}
		definition := demoCollectionDefinition(kind, "settings.ini")
		if _, err := app.collectDemoBackup(context.Background(), Server{ID: "demo"}, definition, BackupSourceStoppedSAV); !errors.Is(err, ErrBackupQuotaExceeded) {
			t.Fatalf("oversized %s demo item accepted: %v", kind, err)
		}
		app.backups.maxItem = 4096
		app.backups.maxBundle = 10
		if _, err := app.collectDemoBackup(context.Background(), Server{ID: "demo"}, definition, BackupSourceStoppedSAV); !errors.Is(err, ErrBackupQuotaExceeded) {
			t.Fatalf("oversized %s demo bundle accepted: %v", kind, err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := app.collectDemoBackup(ctx, Server{ID: "demo"}, definition, BackupSourceStoppedSAV); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled demo capture = %v", err)
		}
	}
	combined := demoCollectionDefinition(BackupItemFile, "settings.ini")
	second := combined.Items[0]
	second.Name = "second"
	combined.Items = append(combined.Items, second)
	bounded := &App{demo: true, backups: &backupController{maxItem: 64, maxBundle: 70}}
	if _, err := bounded.collectDemoBackup(context.Background(), Server{ID: "demo"}, combined, BackupSourceStoppedSAV); !errors.Is(err, ErrBackupQuotaExceeded) {
		t.Fatalf("combined demo items exceeded bundle bound: %v", err)
	}
	root := t.TempDir()
	world := filepath.Join(root, ".demo-worlds", "demo")
	if err := os.MkdirAll(world, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(world, "settings.ini"), []byte("restored data exceeds bound"), 0600); err != nil {
		t.Fatal(err)
	}
	app := &App{demo: true, backups: &backupController{root: root, maxItem: 4}}
	if _, err := app.collectDemoBackup(context.Background(), Server{ID: "demo"}, demoCollectionDefinition(BackupItemFile, "settings.ini"), BackupSourceStoppedSAV); !errors.Is(err, ErrBackupQuotaExceeded) {
		t.Fatalf("oversized restored item accepted: %v", err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(world, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := app.collectDemoBackup(context.Background(), Server{ID: "demo"}, demoCollectionDefinition(BackupItemDirectory, "escape"), BackupSourceStoppedSAV); err == nil {
		t.Fatal("symlink directory accepted")
	}
}
