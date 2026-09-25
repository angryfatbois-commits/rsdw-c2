package main

import (
	"context"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"

	minio "github.com/minio/minio-go/v7"
)

// fakeS3Client is an in-memory stand-in for the minio-go client, letting
// S3BackupRepository tests run without any real network or bucket.
type fakeS3Client struct {
	mu           sync.Mutex
	bucketExists bool
	bucketErr    error
	objects      map[string][]byte
	putErr       error
	getErr       error
	statErr      error
	listErr      error
	removeErr    error
	puts         int
}

func newFakeS3Client() *fakeS3Client {
	return &fakeS3Client{bucketExists: true, objects: map[string][]byte{}}
}

func (f *fakeS3Client) BucketExists(context.Context, string) (bool, error) {
	if f.bucketErr != nil {
		return false, f.bucketErr
	}
	return f.bucketExists, nil
}

func (f *fakeS3Client) PutObject(_ context.Context, _, objectName string, reader io.Reader, size int64, _ minio.PutObjectOptions) (minio.UploadInfo, error) {
	if f.putErr != nil {
		return minio.UploadInfo{}, f.putErr
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return minio.UploadInfo{}, err
	}
	f.mu.Lock()
	f.objects[objectName] = data
	f.puts++
	f.mu.Unlock()
	return minio.UploadInfo{Key: objectName, Size: size}, nil
}

func (f *fakeS3Client) GetObject(_ context.Context, _, objectName string, _ minio.GetObjectOptions) (io.ReadCloser, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	f.mu.Lock()
	data, ok := f.objects[objectName]
	f.mu.Unlock()
	if !ok {
		return nil, minio.ErrorResponse{Code: "NoSuchKey", Message: "object does not exist"}
	}
	return io.NopCloser(strings.NewReader(string(data))), nil
}

func (f *fakeS3Client) StatObject(_ context.Context, _, objectName string, _ minio.StatObjectOptions) (minio.ObjectInfo, error) {
	if f.statErr != nil {
		return minio.ObjectInfo{}, f.statErr
	}
	f.mu.Lock()
	data, ok := f.objects[objectName]
	f.mu.Unlock()
	if !ok {
		return minio.ObjectInfo{}, minio.ErrorResponse{Code: "NoSuchKey", Message: "object does not exist"}
	}
	return minio.ObjectInfo{Key: objectName, Size: int64(len(data))}, nil
}

func (f *fakeS3Client) RemoveObject(_ context.Context, _, objectName string, _ minio.RemoveObjectOptions) error {
	if f.removeErr != nil {
		return f.removeErr
	}
	f.mu.Lock()
	delete(f.objects, objectName)
	f.mu.Unlock()
	return nil
}

func (f *fakeS3Client) ListObjects(ctx context.Context, _ string, opts minio.ListObjectsOptions) <-chan minio.ObjectInfo {
	out := make(chan minio.ObjectInfo)
	go func() {
		defer close(out)
		if f.listErr != nil {
			out <- minio.ObjectInfo{Err: f.listErr}
			return
		}
		f.mu.Lock()
		keys := make([]string, 0, len(f.objects))
		for key := range f.objects {
			if opts.Prefix == "" || strings.HasPrefix(key, opts.Prefix) {
				keys = append(keys, key)
			}
		}
		f.mu.Unlock()
		sort.Strings(keys)
		for _, key := range keys {
			select {
			case out <- minio.ObjectInfo{Key: key, Size: int64(len(f.objects[key]))}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

func testS3Repository(client *fakeS3Client, config S3BackupRepositoryConfig) *S3BackupRepository {
	return newS3BackupRepositoryWithClient(client, config)
}

func TestS3BackupRepositoryPublishesAndOpensBundle(t *testing.T) {
	client := newFakeS3Client()
	repository := testS3Repository(client, S3BackupRepositoryConfig{Bucket: "test-bucket", MaxRepositoryBytes: 1 << 30, MaxBackupBytes: 1 << 20, MaxItemBytes: 1 << 20})
	request := testPublicationRequest(t, "s3-backup", "s3-request", []testItem{
		{name: "world", path: "RSDragonwilds/Saved/SaveGames/World.sav.backup", content: "world-data", consistency: BackupConsistencyAtomicPublish},
	})
	published, err := repository.Publish(context.Background(), request)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if published.Bundle.Key != "objects/s3-backup/bundle.zip" {
		t.Fatalf("unexpected bundle key: %s", published.Bundle.Key)
	}
	if _, ok := client.objects["objects/s3-backup/bundle.zip"]; !ok {
		t.Fatal("bundle object was not uploaded")
	}
	if _, ok := client.objects["objects/s3-backup/publication.json"]; !ok {
		t.Fatal("publication object was not uploaded")
	}

	reader, err := repository.OpenBundle(context.Background(), published.Bundle.Key)
	if err != nil {
		t.Fatalf("open bundle: %v", err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}
	if int64(len(data)) != published.Bundle.Size {
		t.Fatalf("bundle size = %d, want %d", len(data), published.Bundle.Size)
	}
}

func TestS3BackupRepositoryUsesPathPrefix(t *testing.T) {
	client := newFakeS3Client()
	repository := testS3Repository(client, S3BackupRepositoryConfig{Bucket: "test-bucket", PathPrefix: "rsdw-backups", MaxRepositoryBytes: 1 << 30, MaxBackupBytes: 1 << 20, MaxItemBytes: 1 << 20})
	request := testPublicationRequest(t, "prefixed", "prefixed-request", []testItem{
		{name: "world", path: "RSDragonwilds/Saved/SaveGames/World.sav.backup", content: "world-data", consistency: BackupConsistencyAtomicPublish},
	})
	if _, err := repository.Publish(context.Background(), request); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if _, ok := client.objects["rsdw-backups/objects/prefixed/bundle.zip"]; !ok {
		t.Fatalf("bundle was not uploaded under the configured prefix: %v", client.objects)
	}
}

func TestS3BackupRepositoryIdempotentRetryReturnsExisting(t *testing.T) {
	client := newFakeS3Client()
	repository := testS3Repository(client, S3BackupRepositoryConfig{Bucket: "test-bucket", MaxRepositoryBytes: 1 << 30, MaxBackupBytes: 1 << 20, MaxItemBytes: 1 << 20})
	request := testPublicationRequest(t, "retry-backup", "retry-key", []testItem{
		{name: "world", path: "world.sav.backup", content: "stable-world", consistency: BackupConsistencyAtomicPublish},
	})
	first, err := repository.Publish(context.Background(), request)
	if err != nil {
		t.Fatalf("first publish: %v", err)
	}
	puts := client.puts
	second, err := repository.Publish(context.Background(), request)
	if err != nil {
		t.Fatalf("retried publish: %v", err)
	}
	if err := matchPublishedBackups(first, second); err != nil {
		t.Fatalf("retry returned a non-matching publication: %v (first=%+v second=%+v)", err, first, second)
	}
	if client.puts != puts {
		t.Fatalf("retry re-uploaded objects: puts went from %d to %d", puts, client.puts)
	}
}

func TestS3BackupRepositoryConflictingRetryIsRejected(t *testing.T) {
	client := newFakeS3Client()
	repository := testS3Repository(client, S3BackupRepositoryConfig{Bucket: "test-bucket", MaxRepositoryBytes: 1 << 30, MaxBackupBytes: 1 << 20, MaxItemBytes: 1 << 20})
	first := testPublicationRequest(t, "conflict-backup", "conflict-key", []testItem{
		{name: "world", path: "world.sav.backup", content: "first-content", consistency: BackupConsistencyAtomicPublish},
	})
	if _, err := repository.Publish(context.Background(), first); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	// Same idempotency key but a request digest that disagrees with the
	// stored publication's idempotency record: matchPublication must reject
	// this as a conflict rather than silently returning the earlier result.
	second := first
	second.Idempotency = testIdempotency(t, "conflict-key", map[string]any{"manifest": "conflict-backup", "distinguishing": "different-request-shape"})
	if _, err := repository.Publish(context.Background(), second); !errors.Is(err, ErrPublicationConflict) {
		t.Fatalf("conflicting retry error = %v, want ErrPublicationConflict", err)
	}
}

func TestS3BackupRepositoryQuotaRejectsOversizedItem(t *testing.T) {
	client := newFakeS3Client()
	repository := testS3Repository(client, S3BackupRepositoryConfig{Bucket: "test-bucket", MaxRepositoryBytes: 1 << 30, MaxBackupBytes: 1 << 20, MaxItemBytes: 4})
	request := testPublicationRequest(t, "oversized-item", "oversized-key", []testItem{
		{name: "world", path: "world.sav.backup", content: "this-content-is-too-large", consistency: BackupConsistencyAtomicPublish},
	})
	if _, err := repository.Publish(context.Background(), request); !errors.Is(err, ErrBackupQuotaExceeded) {
		t.Fatalf("oversized item error = %v, want ErrBackupQuotaExceeded", err)
	}
	if len(client.objects) != 0 {
		t.Fatalf("rejected publish uploaded objects: %v", client.objects)
	}
}

func TestS3BackupRepositoryQuotaRejectsRepositoryOverflow(t *testing.T) {
	client := newFakeS3Client()
	repository := testS3Repository(client, S3BackupRepositoryConfig{Bucket: "test-bucket", MaxRepositoryBytes: 1 << 20, MaxBackupBytes: 1 << 20, MaxItemBytes: 1 << 20})
	first := testPublicationRequest(t, "quota-first", "quota-first-key", []testItem{
		{name: "world", path: "world.sav.backup", content: strings.Repeat("a", 40), consistency: BackupConsistencyAtomicPublish},
	})
	published, err := repository.Publish(context.Background(), first)
	if err != nil {
		t.Fatalf("first publish: %v", err)
	}
	// Shrink the repository quota to just under the size already used, so
	// any additional bundle must overflow it.
	repository.maxRepositoryBytes = published.Bundle.Size
	second := testPublicationRequest(t, "quota-second", "quota-second-key", []testItem{
		{name: "world", path: "world.sav.backup", content: strings.Repeat("b", 40), consistency: BackupConsistencyAtomicPublish},
	})
	if _, err := repository.Publish(context.Background(), second); !errors.Is(err, ErrBackupQuotaExceeded) {
		t.Fatalf("repository overflow error = %v, want ErrBackupQuotaExceeded", err)
	}
	if _, ok := client.objects["objects/quota-second/bundle.zip"]; ok {
		t.Fatal("rejected publish left a bundle object behind")
	}
}

func TestS3BackupRepositoryUsageFailsWhenBucketUnreachable(t *testing.T) {
	client := newFakeS3Client()
	client.bucketErr = errors.New("network unreachable")
	repository := testS3Repository(client, S3BackupRepositoryConfig{Bucket: "test-bucket"})
	if _, err := repository.Usage(context.Background()); err == nil {
		t.Fatal("expected error when bucket is unreachable")
	}
}

func TestS3BackupRepositoryUsageFailsWhenBucketMissing(t *testing.T) {
	client := newFakeS3Client()
	client.bucketExists = false
	repository := testS3Repository(client, S3BackupRepositoryConfig{Bucket: "test-bucket"})
	if _, err := repository.Usage(context.Background()); err == nil {
		t.Fatal("expected error when bucket does not exist")
	}
}

func TestS3BackupRepositoryUsageFailsWhenNotWritable(t *testing.T) {
	client := newFakeS3Client()
	client.putErr = errors.New("access denied")
	repository := testS3Repository(client, S3BackupRepositoryConfig{Bucket: "test-bucket"})
	if _, err := repository.Usage(context.Background()); err == nil {
		t.Fatal("expected error when bucket is not writable")
	}
}

func TestS3BackupRepositoryUsageSumsPublishedBundleSizes(t *testing.T) {
	client := newFakeS3Client()
	repository := testS3Repository(client, S3BackupRepositoryConfig{Bucket: "test-bucket", MaxRepositoryBytes: 1 << 30, MaxBackupBytes: 1 << 20, MaxItemBytes: 1 << 20})
	for _, id := range []string{"usage-one", "usage-two"} {
		request := testPublicationRequest(t, id, id+"-key", []testItem{
			{name: "world", path: "world.sav.backup", content: "some-world-data", consistency: BackupConsistencyAtomicPublish},
		})
		if _, err := repository.Publish(context.Background(), request); err != nil {
			t.Fatalf("publish %s: %v", id, err)
		}
	}
	usage, err := repository.Usage(context.Background())
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if usage <= 0 {
		t.Fatalf("usage = %d, want positive total across published bundles", usage)
	}
}

func TestS3BackupRepositoryRecoverReturnsCompletePublications(t *testing.T) {
	client := newFakeS3Client()
	repository := testS3Repository(client, S3BackupRepositoryConfig{Bucket: "test-bucket", MaxRepositoryBytes: 1 << 30, MaxBackupBytes: 1 << 20, MaxItemBytes: 1 << 20})
	request := testPublicationRequest(t, "recoverable", "recoverable-key", []testItem{
		{name: "world", path: "world.sav.backup", content: "world-data", consistency: BackupConsistencyAtomicPublish},
	})
	published, err := repository.Publish(context.Background(), request)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	recovered, err := repository.Recover(context.Background())
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(recovered) != 1 || recovered[0].Manifest.ID != published.Manifest.ID {
		t.Fatalf("recover = %+v, want one entry matching %s", recovered, published.Manifest.ID)
	}
}

func TestS3BackupRepositoryRecoverSkipsIncompleteUploads(t *testing.T) {
	client := newFakeS3Client()
	repository := testS3Repository(client, S3BackupRepositoryConfig{Bucket: "test-bucket", MaxRepositoryBytes: 1 << 30, MaxBackupBytes: 1 << 20, MaxItemBytes: 1 << 20})
	request := testPublicationRequest(t, "complete", "complete-key", []testItem{
		{name: "world", path: "world.sav.backup", content: "world-data", consistency: BackupConsistencyAtomicPublish},
	})
	if _, err := repository.Publish(context.Background(), request); err != nil {
		t.Fatalf("publish: %v", err)
	}
	// Simulate an interrupted upload: publication.json exists but the bundle
	// never made it, and the reverse case too.
	client.objects["objects/orphan-publication-only/publication.json"] = []byte(`{"manifest":{"id":"orphan-publication-only"}}`)
	client.objects["objects/orphan-bundle-only/bundle.zip"] = []byte("partial-bundle-bytes")

	recovered, err := repository.Recover(context.Background())
	if err != nil {
		t.Fatalf("recover should skip incomplete pairs, not fail: %v", err)
	}
	if len(recovered) != 1 || recovered[0].Manifest.ID != "complete" {
		t.Fatalf("recover = %+v, want only the complete publication", recovered)
	}
}

func TestS3BackupRepositoryRecoverFailsOnListError(t *testing.T) {
	client := newFakeS3Client()
	client.listErr = errors.New("listing failed")
	repository := testS3Repository(client, S3BackupRepositoryConfig{Bucket: "test-bucket"})
	if _, err := repository.Recover(context.Background()); err == nil {
		t.Fatal("expected error when listing fails")
	}
}

func TestS3BackupRepositoryValidatesConfig(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config S3BackupRepositoryConfig
	}{
		{"missing endpoint", S3BackupRepositoryConfig{Bucket: "b", AccessKeyID: "a", SecretAccessKey: "s"}},
		{"missing bucket", S3BackupRepositoryConfig{Endpoint: "s3.example.com", AccessKeyID: "a", SecretAccessKey: "s"}},
		{"missing credentials", S3BackupRepositoryConfig{Endpoint: "s3.example.com", Bucket: "b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewS3BackupRepository(tc.config); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}
