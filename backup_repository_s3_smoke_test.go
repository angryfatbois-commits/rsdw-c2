package main

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestS3BackupRepositoryLiveSmoke exercises S3BackupRepository against a
// real S3-compatible bucket. It is opt-in: unset any of the required
// environment variables and this test skips cleanly rather than failing CI,
// since it requires real infrastructure and network access.
//
// Required environment variables:
//
//	RSDW_S3_TEST_ENDPOINT         host[:port], no scheme
//	RSDW_S3_TEST_BUCKET           an existing bucket the credentials can write to
//	RSDW_S3_TEST_ACCESS_KEY_ID
//	RSDW_S3_TEST_SECRET_ACCESS_KEY
//
// Optional:
//
//	RSDW_S3_TEST_REGION
//	RSDW_S3_TEST_USE_SSL          defaults to true; set "false" to disable
//	RSDW_S3_TEST_PREFIX           defaults to a unique smoke-test prefix, cleaned up after the run
func TestS3BackupRepositoryLiveSmoke(t *testing.T) {
	endpoint := strings.TrimSpace(os.Getenv("RSDW_S3_TEST_ENDPOINT"))
	bucket := strings.TrimSpace(os.Getenv("RSDW_S3_TEST_BUCKET"))
	accessKeyID := strings.TrimSpace(os.Getenv("RSDW_S3_TEST_ACCESS_KEY_ID"))
	secretAccessKey := strings.TrimSpace(os.Getenv("RSDW_S3_TEST_SECRET_ACCESS_KEY"))
	if endpoint == "" || bucket == "" || accessKeyID == "" || secretAccessKey == "" {
		t.Skip("RSDW_S3_TEST_ENDPOINT, RSDW_S3_TEST_BUCKET, RSDW_S3_TEST_ACCESS_KEY_ID, and RSDW_S3_TEST_SECRET_ACCESS_KEY must all be set to run the live S3 smoke test")
	}
	useSSL := true
	if value := strings.TrimSpace(os.Getenv("RSDW_S3_TEST_USE_SSL")); value != "" {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			t.Fatalf("RSDW_S3_TEST_USE_SSL must be true or false: %v", err)
		}
		useSSL = parsed
	}
	prefix := strings.TrimSpace(os.Getenv("RSDW_S3_TEST_PREFIX"))
	if prefix == "" {
		prefix = "rsdw-c2-smoke-" + newBackupID("run")
	}

	repository, err := NewS3BackupRepository(S3BackupRepositoryConfig{
		Endpoint:           endpoint,
		AccessKeyID:        accessKeyID,
		SecretAccessKey:    secretAccessKey,
		Bucket:             bucket,
		Region:             strings.TrimSpace(os.Getenv("RSDW_S3_TEST_REGION")),
		PathPrefix:         prefix,
		UseSSL:             useSSL,
		MaxRepositoryBytes: 1 << 30,
		MaxBackupBytes:     1 << 20,
		MaxItemBytes:       1 << 20,
	})
	if err != nil {
		t.Fatalf("create S3 repository: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := repository.Usage(ctx); err != nil {
		t.Fatalf("bucket is not reachable or writable: %v", err)
	}

	request := testPublicationRequest(t, "smoke-"+newBackupID("manifest"), "smoke-"+newBackupID("request"), []testItem{
		{name: "world", path: "RSDragonwilds/Saved/SaveGames/World.sav.backup", content: "live smoke test payload", consistency: BackupConsistencyAtomicPublish},
	})
	published, err := repository.Publish(ctx, request)
	if err != nil {
		t.Fatalf("publish against live bucket: %v", err)
	}
	t.Logf("published bundle %s (%d bytes) under prefix %q", published.Bundle.Key, published.Bundle.Size, prefix)

	reader, err := repository.OpenBundle(ctx, published.Bundle.Key)
	if err != nil {
		t.Fatalf("open bundle from live bucket: %v", err)
	}
	data := make([]byte, published.Bundle.Size)
	if _, err := reader.Read(data); err != nil {
		_ = reader.Close()
		t.Fatalf("read bundle from live bucket: %v", err)
	}
	_ = reader.Close()

	recovered, err := repository.Recover(ctx)
	if err != nil {
		t.Fatalf("recover from live bucket: %v", err)
	}
	found := false
	for _, candidate := range recovered {
		if candidate.Manifest.ID == published.Manifest.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("recover did not surface the published manifest %s among %d entries", published.Manifest.ID, len(recovered))
	}

	usage, err := repository.Usage(ctx)
	if err != nil {
		t.Fatalf("usage after publish: %v", err)
	}
	if usage < published.Bundle.Size {
		t.Fatalf("usage %d is smaller than the published bundle size %d", usage, published.Bundle.Size)
	}

	t.Logf("live S3 smoke test passed against endpoint %s bucket %s prefix %s", endpoint, bucket, prefix)
}
