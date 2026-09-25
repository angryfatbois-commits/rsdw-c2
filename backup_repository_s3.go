package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	minio "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// s3ObjectClient is the narrow slice of the minio-go client this repository
// uses. Tests substitute a fake implementation so unit tests never touch a
// real network or bucket.
type s3ObjectClient interface {
	BucketExists(ctx context.Context, bucketName string) (bool, error)
	PutObject(ctx context.Context, bucketName, objectName string, reader io.Reader, size int64, opts minio.PutObjectOptions) (minio.UploadInfo, error)
	GetObject(ctx context.Context, bucketName, objectName string, opts minio.GetObjectOptions) (io.ReadCloser, error)
	StatObject(ctx context.Context, bucketName, objectName string, opts minio.StatObjectOptions) (minio.ObjectInfo, error)
	RemoveObject(ctx context.Context, bucketName, objectName string, opts minio.RemoveObjectOptions) error
	ListObjects(ctx context.Context, bucketName string, opts minio.ListObjectsOptions) <-chan minio.ObjectInfo
}

// minioClientAdapter adapts *minio.Client's GetObject (which returns
// *minio.Object, not io.ReadCloser) to the narrower s3ObjectClient interface.
type minioClientAdapter struct {
	client *minio.Client
}

func (a minioClientAdapter) BucketExists(ctx context.Context, bucketName string) (bool, error) {
	return a.client.BucketExists(ctx, bucketName)
}

func (a minioClientAdapter) PutObject(ctx context.Context, bucketName, objectName string, reader io.Reader, size int64, opts minio.PutObjectOptions) (minio.UploadInfo, error) {
	return a.client.PutObject(ctx, bucketName, objectName, reader, size, opts)
}

func (a minioClientAdapter) GetObject(ctx context.Context, bucketName, objectName string, opts minio.GetObjectOptions) (io.ReadCloser, error) {
	return a.client.GetObject(ctx, bucketName, objectName, opts)
}

func (a minioClientAdapter) StatObject(ctx context.Context, bucketName, objectName string, opts minio.StatObjectOptions) (minio.ObjectInfo, error) {
	return a.client.StatObject(ctx, bucketName, objectName, opts)
}

func (a minioClientAdapter) RemoveObject(ctx context.Context, bucketName, objectName string, opts minio.RemoveObjectOptions) error {
	return a.client.RemoveObject(ctx, bucketName, objectName, opts)
}

func (a minioClientAdapter) ListObjects(ctx context.Context, bucketName string, opts minio.ListObjectsOptions) <-chan minio.ObjectInfo {
	return a.client.ListObjects(ctx, bucketName, opts)
}

type S3BackupRepositoryConfig struct {
	Endpoint           string
	AccessKeyID        string
	SecretAccessKey    string
	Bucket             string
	Region             string
	PathPrefix         string
	UseSSL             bool
	MaxRepositoryBytes int64
	MaxBackupBytes     int64
	MaxItemBytes       int64
}

// S3BackupRepository implements BackupRepository against an S3-compatible
// bucket. Bundles are assembled on local disk exactly like the local
// filesystem backend (reusing createDeterministicBundle/writePayload), then
// uploaded as two objects: "<prefix>objects/<manifestID>/bundle.zip" and
// "<prefix>objects/<manifestID>/publication.json". There is no multipart
// upload path; the default 256 MiB bundle limit is well under minio-go's
// 16 MiB single-PUT threshold... actually above it, but still far under the
// library's automatic multipart cutover only matters for memory use, not
// correctness, so a single PutObject call per bundle is sufficient.
type S3BackupRepository struct {
	client             s3ObjectClient
	bucket             string
	prefix             string
	maxRepositoryBytes int64
	maxBackupBytes     int64
	maxItemBytes       int64
	mu                 sync.Mutex
}

func NewS3BackupRepository(config S3BackupRepositoryConfig) (*S3BackupRepository, error) {
	if strings.TrimSpace(config.Endpoint) == "" {
		return nil, errors.New("S3 backend endpoint is required")
	}
	if strings.TrimSpace(config.Bucket) == "" {
		return nil, errors.New("S3 backend bucket is required")
	}
	if strings.TrimSpace(config.AccessKeyID) == "" || strings.TrimSpace(config.SecretAccessKey) == "" {
		return nil, errors.New("S3 backend credentials are required")
	}
	client, err := minio.New(config.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(config.AccessKeyID, config.SecretAccessKey, ""),
		Secure: config.UseSSL,
		Region: config.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("create S3 client: %w", err)
	}
	return newS3BackupRepositoryWithClient(minioClientAdapter{client: client}, config), nil
}

func newS3BackupRepositoryWithClient(client s3ObjectClient, config S3BackupRepositoryConfig) *S3BackupRepository {
	prefix := strings.Trim(config.PathPrefix, "/")
	if prefix != "" {
		prefix += "/"
	}
	return &S3BackupRepository{
		client:             client,
		bucket:             config.Bucket,
		prefix:             prefix,
		maxRepositoryBytes: config.MaxRepositoryBytes,
		maxBackupBytes:     config.MaxBackupBytes,
		maxItemBytes:       config.MaxItemBytes,
	}
}

func (repository *S3BackupRepository) bundleKey(manifestID string) string {
	return repository.prefix + objectKeyForManifest(manifestID)
}

func (repository *S3BackupRepository) publicationKey(manifestID string) string {
	return repository.prefix + "objects/" + manifestID + "/publication.json"
}

func (repository *S3BackupRepository) objectsPrefix() string {
	return repository.prefix + "objects/"
}

func (repository *S3BackupRepository) readPublication(ctx context.Context, manifestID string) (PublishedBackup, error) {
	object, err := repository.client.GetObject(ctx, repository.bucket, repository.publicationKey(manifestID), minio.GetObjectOptions{})
	if err != nil {
		return PublishedBackup{}, err
	}
	defer object.Close()
	data, err := io.ReadAll(object)
	if err != nil {
		return PublishedBackup{}, fmt.Errorf("read backup publication: %w", err)
	}
	var published PublishedBackup
	if err := json.Unmarshal(data, &published); err != nil {
		return PublishedBackup{}, fmt.Errorf("decode backup publication: %w", err)
	}
	if err := validatePublishedBackup(published); err != nil {
		return PublishedBackup{}, err
	}
	return published, nil
}

func (repository *S3BackupRepository) publicationExists(ctx context.Context, manifestID string) (bool, error) {
	_, err := repository.client.StatObject(ctx, repository.bucket, repository.publicationKey(manifestID), minio.StatObjectOptions{})
	if err == nil {
		return true, nil
	}
	if minioErrorCode(err) == "NoSuchKey" {
		return false, nil
	}
	return false, err
}

func minioErrorCode(err error) string {
	if err == nil {
		return ""
	}
	response := minio.ToErrorResponse(err)
	return response.Code
}

func (repository *S3BackupRepository) Publish(ctx context.Context, request PublishBackupRequest) (PublishedBackup, error) {
	if err := ctx.Err(); err != nil {
		return PublishedBackup{}, err
	}
	if err := validateIdempotencyRequest(request.Idempotency); err != nil {
		return PublishedBackup{}, err
	}
	if err := validateManifestDraft(request.Manifest); err != nil {
		return PublishedBackup{}, err
	}

	if exists, err := repository.publicationExists(ctx, request.Manifest.ID); err != nil {
		return PublishedBackup{}, fmt.Errorf("inspect backup publication: %w", err)
	} else if exists {
		existing, err := repository.readPublication(ctx, request.Manifest.ID)
		if err != nil {
			return PublishedBackup{}, fmt.Errorf("read existing backup publication: %w", err)
		}
		if err := matchPublication(existing, request.Idempotency, request.Manifest.ID); err != nil {
			return PublishedBackup{}, err
		}
		return existing, nil
	}

	staged, stageDirectory, err := repository.stageBundle(ctx, request)
	if stageDirectory != "" {
		defer os.RemoveAll(stageDirectory)
	}
	if err != nil {
		return PublishedBackup{}, err
	}

	repository.mu.Lock()
	defer repository.mu.Unlock()

	if exists, err := repository.publicationExists(ctx, request.Manifest.ID); err != nil {
		return PublishedBackup{}, fmt.Errorf("inspect backup publication: %w", err)
	} else if exists {
		existing, err := repository.readPublication(ctx, request.Manifest.ID)
		if err != nil {
			return PublishedBackup{}, fmt.Errorf("read concurrent backup publication: %w", err)
		}
		if err := matchPublishedBackups(existing, staged); err != nil {
			return PublishedBackup{}, err
		}
		return existing, nil
	}

	usage, err := repository.usageLocked(ctx)
	if err != nil {
		return PublishedBackup{}, err
	}
	if repository.maxRepositoryBytes > 0 && (usage > repository.maxRepositoryBytes || staged.Bundle.Size > repository.maxRepositoryBytes-usage) {
		return PublishedBackup{}, fmt.Errorf("%w: repository uses %d bytes and the new bundle needs %d of %d bytes", ErrBackupQuotaExceeded, usage, staged.Bundle.Size, repository.maxRepositoryBytes)
	}

	bundlePath := filepath.Join(stageDirectory, "bundle.zip")
	bundleFile, err := os.Open(bundlePath)
	if err != nil {
		return PublishedBackup{}, fmt.Errorf("open staged bundle: %w", err)
	}
	defer bundleFile.Close()
	if _, err := repository.client.PutObject(ctx, repository.bucket, repository.bundleKey(staged.Manifest.ID), bundleFile, staged.Bundle.Size, minio.PutObjectOptions{ContentType: "application/zip"}); err != nil {
		return PublishedBackup{}, fmt.Errorf("upload backup bundle: %w", err)
	}

	publicationData, err := json.Marshal(staged)
	if err != nil {
		return PublishedBackup{}, fmt.Errorf("encode backup publication: %w", err)
	}
	if _, err := repository.client.PutObject(ctx, repository.bucket, repository.publicationKey(staged.Manifest.ID), strings.NewReader(string(publicationData)), int64(len(publicationData)), minio.PutObjectOptions{ContentType: "application/json"}); err != nil {
		return PublishedBackup{}, fmt.Errorf("upload backup publication: %w", err)
	}
	return staged, nil
}

// stageBundle assembles the manifest and bundle.zip on local disk using the
// same item validation and deterministic bundling logic as the local
// filesystem backend, returning the staged PublishedBackup and the temp
// directory containing "bundle.zip" (caller uploads then removes it).
func (repository *S3BackupRepository) stageBundle(ctx context.Context, request PublishBackupRequest) (PublishedBackup, string, error) {
	if len(request.Items) == 0 {
		return PublishedBackup{}, "", errors.New("backup publication requires at least one item")
	}
	items := append([]BackupPublicationItem(nil), request.Items...)
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })

	temporaryDirectory, err := os.MkdirTemp("", ".rsdw-s3-stage-"+request.Idempotency.Identity()+"-")
	if err != nil {
		return PublishedBackup{}, "", fmt.Errorf("create S3 staging directory: %w", err)
	}
	if err := os.Chmod(temporaryDirectory, 0o700); err != nil {
		os.RemoveAll(temporaryDirectory)
		return PublishedBackup{}, "", fmt.Errorf("secure S3 staging directory: %w", err)
	}
	payloadDirectory := filepath.Join(temporaryDirectory, "payloads")
	if err := os.Mkdir(payloadDirectory, 0o700); err != nil {
		os.RemoveAll(temporaryDirectory)
		return PublishedBackup{}, "", fmt.Errorf("create S3 payload staging directory: %w", err)
	}

	manifestItems := make([]BackupManifestItem, 0, len(items))
	seenNames := make(map[string]struct{}, len(items))
	var rawSize int64
	for index, item := range items {
		if err := ctx.Err(); err != nil {
			return PublishedBackup{}, temporaryDirectory, err
		}
		if err := validateBackupID("publication item name", item.Name); err != nil {
			return PublishedBackup{}, temporaryDirectory, fmt.Errorf("items[%d]: %w", index, err)
		}
		if _, exists := seenNames[item.Name]; exists {
			return PublishedBackup{}, temporaryDirectory, fmt.Errorf("items[%d]: duplicate item name %q", index, item.Name)
		}
		seenNames[item.Name] = struct{}{}
		if item.Kind == "" {
			item.Kind = BackupItemFile
		}
		if item.Kind != BackupItemFile && item.Kind != BackupItemDirectory {
			return PublishedBackup{}, temporaryDirectory, fmt.Errorf("items[%d]: unsupported item kind %q", index, item.Kind)
		}
		if err := ValidateBackupRelativePath(item.SourcePath); err != nil {
			return PublishedBackup{}, temporaryDirectory, fmt.Errorf("items[%d]: %w", index, err)
		}
		if !validItemConsistency(item.Consistency) {
			return PublishedBackup{}, temporaryDirectory, fmt.Errorf("items[%d]: unsupported consistency %q", index, item.Consistency)
		}
		if item.Content == nil {
			return PublishedBackup{}, temporaryDirectory, fmt.Errorf("items[%d]: content is required", index)
		}

		objectKey := opaqueItemObjectKey(item.Name, item.SourcePath)
		payloadPath := filepath.Join(payloadDirectory, strings.TrimPrefix(objectKey, "items/"))
		size, digest, err := repository.writePayload(ctx, payloadPath, item.Content, rawSize)
		if err != nil {
			return PublishedBackup{}, temporaryDirectory, fmt.Errorf("stage item %q: %w", item.Name, err)
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
		return PublishedBackup{}, temporaryDirectory, fmt.Errorf("encode backup manifest: %w", err)
	}
	manifestData = append(manifestData, '\n')

	bundlePath := filepath.Join(temporaryDirectory, "bundle.zip")
	if err := createDeterministicBundle(bundlePath, manifestData, payloadDirectory, manifestItems); err != nil {
		return PublishedBackup{}, temporaryDirectory, err
	}
	bundleSize, bundleDigest, err := fileDigest(bundlePath)
	if err != nil {
		return PublishedBackup{}, temporaryDirectory, fmt.Errorf("digest backup bundle: %w", err)
	}
	if repository.maxBackupBytes > 0 && bundleSize > repository.maxBackupBytes {
		return PublishedBackup{}, temporaryDirectory, fmt.Errorf("%w: bundle is %d bytes and the per-backup limit is %d", ErrBackupQuotaExceeded, bundleSize, repository.maxBackupBytes)
	}
	for _, item := range manifestItems {
		if repository.maxItemBytes > 0 && item.Size > repository.maxItemBytes {
			return PublishedBackup{}, temporaryDirectory, fmt.Errorf("%w: staged item exceeds the limit of %d bytes", ErrBackupQuotaExceeded, repository.maxItemBytes)
		}
	}

	if err := os.RemoveAll(payloadDirectory); err != nil {
		return PublishedBackup{}, temporaryDirectory, fmt.Errorf("remove staged item payloads: %w", err)
	}

	return PublishedBackup{
		Idempotency: request.Idempotency,
		Manifest:    manifest,
		Bundle: BackupObject{
			Key:    objectKeyForManifest(manifest.ID),
			Size:   bundleSize,
			SHA256: bundleDigest,
		},
	}, temporaryDirectory, nil
}

func (repository *S3BackupRepository) writePayload(ctx context.Context, destination string, source io.Reader, existingSize int64) (int64, string, error) {
	return writeBackupPayload(ctx, destination, source, existingSize, repository.maxItemBytes, repository.maxBackupBytes)
}

// Recover lists published backups under the bucket prefix. Unlike the local
// backend, S3 has no crash-recovery for interrupted uploads: an interrupted
// Publish may leave an orphan bundle.zip without a matching publication.json
// (or vice versa). Recover treats incomplete pairs as not-yet-published and
// silently skips them rather than failing the whole recovery pass, since a
// retried Publish with the same idempotency key will detect and complete
// (or safely re-upload over) the orphaned pair.
func (repository *S3BackupRepository) Recover(ctx context.Context) ([]PublishedBackup, error) {
	manifestIDs := map[string]struct{}{}
	for object := range repository.client.ListObjects(ctx, repository.bucket, minio.ListObjectsOptions{Prefix: repository.objectsPrefix(), Recursive: true}) {
		if object.Err != nil {
			return nil, fmt.Errorf("list backup objects: %w", object.Err)
		}
		if !strings.HasSuffix(object.Key, "/publication.json") {
			continue
		}
		key := strings.TrimPrefix(object.Key, repository.prefix)
		parts := strings.Split(key, "/")
		if len(parts) != 3 || parts[0] != "objects" || parts[2] != "publication.json" {
			continue
		}
		manifestIDs[parts[1]] = struct{}{}
	}

	ids := make([]string, 0, len(manifestIDs))
	for id := range manifestIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	recovered := make([]PublishedBackup, 0, len(ids))
	for _, manifestID := range ids {
		if err := validateBackupID("manifest id", manifestID); err != nil {
			continue
		}
		if exists, err := repository.bundleExists(ctx, manifestID); err != nil {
			return nil, fmt.Errorf("inspect backup bundle %q: %w", manifestID, err)
		} else if !exists {
			continue
		}
		published, err := repository.readPublication(ctx, manifestID)
		if err != nil {
			return nil, fmt.Errorf("read backup publication %q: %w", manifestID, err)
		}
		recovered = append(recovered, published)
	}
	return recovered, nil
}

func (repository *S3BackupRepository) bundleExists(ctx context.Context, manifestID string) (bool, error) {
	_, err := repository.client.StatObject(ctx, repository.bucket, repository.bundleKey(manifestID), minio.StatObjectOptions{})
	if err == nil {
		return true, nil
	}
	if minioErrorCode(err) == "NoSuchKey" {
		return false, nil
	}
	return false, err
}

func (repository *S3BackupRepository) OpenBundle(ctx context.Context, objectKey string) (io.ReadCloser, error) {
	manifestID, err := manifestIDFromObjectKey(objectKey)
	if err != nil {
		return nil, err
	}
	return repository.client.GetObject(ctx, repository.bucket, repository.bundleKey(manifestID), minio.GetObjectOptions{})
}

func (repository *S3BackupRepository) Usage(ctx context.Context) (int64, error) {
	ok, err := repository.client.BucketExists(ctx, repository.bucket)
	if err != nil {
		return 0, fmt.Errorf("S3 bucket is not reachable: %w", err)
	}
	if !ok {
		return 0, errors.New("S3 bucket does not exist or is not accessible")
	}
	probeKey := repository.prefix + ".health-" + newBackupID("probe")
	if _, err := repository.client.PutObject(ctx, repository.bucket, probeKey, strings.NewReader("1"), 1, minio.PutObjectOptions{}); err != nil {
		return 0, fmt.Errorf("S3 bucket is not writable: %w", err)
	}
	if err := repository.client.RemoveObject(ctx, repository.bucket, probeKey, minio.RemoveObjectOptions{}); err != nil {
		return 0, fmt.Errorf("S3 bucket cleanup failed: %w", err)
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	return repository.usageLocked(ctx)
}

func (repository *S3BackupRepository) usageLocked(ctx context.Context) (int64, error) {
	manifestIDs := map[string]struct{}{}
	for object := range repository.client.ListObjects(ctx, repository.bucket, minio.ListObjectsOptions{Prefix: repository.objectsPrefix(), Recursive: true}) {
		if object.Err != nil {
			return 0, fmt.Errorf("list backup objects: %w", object.Err)
		}
		if !strings.HasSuffix(object.Key, "/publication.json") {
			continue
		}
		key := strings.TrimPrefix(object.Key, repository.prefix)
		parts := strings.Split(key, "/")
		if len(parts) != 3 || parts[0] != "objects" || parts[2] != "publication.json" {
			continue
		}
		manifestIDs[parts[1]] = struct{}{}
	}
	var usage int64
	for manifestID := range manifestIDs {
		published, err := repository.readPublication(ctx, manifestID)
		if err != nil {
			return 0, fmt.Errorf("read backup publication %q: %w", manifestID, err)
		}
		if published.Bundle.Size > int64(^uint64(0)>>1)-usage {
			return 0, errors.New("backup repository usage overflow")
		}
		usage += published.Bundle.Size
	}
	return usage, nil
}
