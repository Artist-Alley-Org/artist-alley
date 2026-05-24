// Package s3 is the S3-API implementation of storage.Backend.
//
// Works against AWS S3, MinIO, Backblaze B2's S3 endpoint, Cloudflare
// R2, Wasabi, and any other vendor that speaks the S3 protocol. Pick
// the right Endpoint + PathStyle combination for your provider:
//
//   - AWS S3: Endpoint="" (or leave unset), PathStyle=false.
//   - MinIO (local dev / self-hosted): Endpoint="http://minio:9000",
//     PathStyle=true.
//   - Cloudflare R2: Endpoint="https://<acc>.r2.cloudflarestorage.com",
//     PathStyle=false.
//   - Backblaze B2 (S3 API): Endpoint="https://s3.<region>.backblazeb2.com",
//     PathStyle=false.
//
// On-bucket layout matches storage.ObjectPath so an FS-rooted install
// and an S3-rooted install have byte-identical key namespaces.
package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/mscrnt/artist-alley/app/internal/storage"
)

// Config bundles the s3-backend parameters that come from the env at
// boot. Bucket is the only required field; everything else has
// defaults appropriate for AWS S3.
type Config struct {
	Bucket       string // required
	Region       string // defaults to "us-east-1"
	Endpoint     string // "" for AWS, host:port URL for MinIO/R2/B2/etc.
	AccessKey    string // empty = use the default credential chain (env, IRSA, etc.)
	SecretKey    string // empty = use the default credential chain
	UsePathStyle bool   // true for MinIO and most self-hosted; false for AWS/R2/B2
}

// Backend talks S3 via the AWS SDK Go v2 client.
type Backend struct {
	bucket   string
	cli      *awss3.Client
	presign  *awss3.PresignClient
}

// New constructs and validates an s3 backend. Verifies the bucket
// exists (or attempts to create it on MinIO; AWS will refuse without
// the right grants — that's the caller's problem). Returns an error
// rather than a partially-initialised backend on any failure so boot
// fails loudly instead of producing a backend that lies about its
// status.
func New(ctx context.Context, c Config) (*Backend, error) {
	if c.Bucket == "" {
		return nil, errors.New("s3: bucket is required")
	}
	if c.Region == "" {
		c.Region = "us-east-1"
	}

	awsCfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(c.Region),
	)
	if err != nil {
		return nil, fmt.Errorf("s3: load config: %w", err)
	}
	if c.AccessKey != "" && c.SecretKey != "" {
		awsCfg.Credentials = credentials.NewStaticCredentialsProvider(c.AccessKey, c.SecretKey, "")
	}

	cli := awss3.NewFromConfig(awsCfg, func(o *awss3.Options) {
		if c.Endpoint != "" {
			o.BaseEndpoint = aws.String(c.Endpoint)
		}
		o.UsePathStyle = c.UsePathStyle
	})

	// Ping the bucket. HeadBucket is the cheapest existence check
	// that doesn't list objects.
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err = cli.HeadBucket(pingCtx, &awss3.HeadBucketInput{Bucket: aws.String(c.Bucket)})
	if err != nil {
		return nil, fmt.Errorf("s3: head bucket %q: %w", c.Bucket, err)
	}

	return &Backend{
		bucket:  c.Bucket,
		cli:     cli,
		presign: awss3.NewPresignClient(cli),
	}, nil
}

// Name returns the stable backend identifier persisted in storage_objects.backend.
func (b *Backend) Name() string { return "s3" }

func (b *Backend) keyFor(hash, variant string) string {
	return storage.ObjectPath(hash, variant)
}

// Put uploads the reader. For now we use the simple PutObject path,
// which buffers in the SDK for small objects and uses the streaming
// transport for large ones. When we wire up TUS-resumable uploads in
// Phase 1.4.C we'll switch to s3manager.Uploader for multipart.
func (b *Backend) Put(ctx context.Context, hash, variant string, r io.Reader) (*storage.ObjectInfo, error) {
	if err := storage.ValidatePair(hash, variant); err != nil {
		return nil, err
	}
	out, err := b.cli.PutObject(ctx, &awss3.PutObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(b.keyFor(hash, variant)),
		Body:   r,
	})
	if err != nil {
		return nil, fmt.Errorf("s3: PutObject: %w", err)
	}
	// HEAD to get the size — PutObject doesn't return ContentLength.
	st, err := b.Stat(ctx, hash, variant)
	if err != nil {
		return nil, err
	}
	if out.ETag != nil {
		st.ETag = aws.ToString(out.ETag)
	}
	return st, nil
}

// Get returns a stream of the full object.
func (b *Backend) Get(ctx context.Context, hash, variant string) (io.ReadCloser, *storage.ObjectInfo, error) {
	if err := storage.ValidatePair(hash, variant); err != nil {
		return nil, nil, err
	}
	out, err := b.cli.GetObject(ctx, &awss3.GetObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(b.keyFor(hash, variant)),
	})
	if err != nil {
		if isNotFoundError(err) {
			return nil, nil, storage.ErrNotFound
		}
		return nil, nil, err
	}
	info := &storage.ObjectInfo{
		Size:        aws.ToInt64(out.ContentLength),
		ContentType: aws.ToString(out.ContentType),
		ETag:        aws.ToString(out.ETag),
	}
	if out.LastModified != nil {
		info.ModifiedAt = out.LastModified.UTC()
	}
	if info.ContentType == "" {
		info.ContentType = "application/octet-stream"
	}
	return out.Body, info, nil
}

// GetRange uses S3's HTTP Range support.
func (b *Backend) GetRange(ctx context.Context, hash, variant string, offset, length int64) (io.ReadCloser, error) {
	if err := storage.ValidatePair(hash, variant); err != nil {
		return nil, err
	}
	if offset < 0 {
		return nil, errors.New("s3: negative offset")
	}
	var rangeHeader string
	if length <= 0 {
		rangeHeader = fmt.Sprintf("bytes=%d-", offset)
	} else {
		rangeHeader = fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)
	}
	out, err := b.cli.GetObject(ctx, &awss3.GetObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(b.keyFor(hash, variant)),
		Range:  aws.String(rangeHeader),
	})
	if err != nil {
		if isNotFoundError(err) {
			return nil, storage.ErrNotFound
		}
		return nil, err
	}
	return out.Body, nil
}

// Delete is idempotent in S3 too — deleting a missing key returns no
// error from S3 itself, so we don't have to special-case it.
func (b *Backend) Delete(ctx context.Context, hash, variant string) error {
	if err := storage.ValidatePair(hash, variant); err != nil {
		return err
	}
	_, err := b.cli.DeleteObject(ctx, &awss3.DeleteObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(b.keyFor(hash, variant)),
	})
	return err
}

// Stat returns metadata via HEAD.
func (b *Backend) Stat(ctx context.Context, hash, variant string) (*storage.ObjectInfo, error) {
	if err := storage.ValidatePair(hash, variant); err != nil {
		return nil, err
	}
	out, err := b.cli.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(b.keyFor(hash, variant)),
	})
	if err != nil {
		if isNotFoundError(err) {
			return nil, storage.ErrNotFound
		}
		return nil, err
	}
	info := &storage.ObjectInfo{
		Size:        aws.ToInt64(out.ContentLength),
		ContentType: aws.ToString(out.ContentType),
		ETag:        aws.ToString(out.ETag),
	}
	if out.LastModified != nil {
		info.ModifiedAt = out.LastModified.UTC()
	}
	if info.ContentType == "" {
		info.ContentType = "application/octet-stream"
	}
	return info, nil
}

// PresignGet returns a time-limited URL the caller can GET directly
// from the backend, bypassing our app server.
func (b *Backend) PresignGet(ctx context.Context, hash, variant string, ttl time.Duration) (string, error) {
	if err := storage.ValidatePair(hash, variant); err != nil {
		return "", err
	}
	req, err := b.presign.PresignGetObject(ctx, &awss3.GetObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(b.keyFor(hash, variant)),
	}, func(p *awss3.PresignOptions) { p.Expires = ttl })
	if err != nil {
		return "", err
	}
	return req.URL, nil
}

// PresignPut mints a URL the caller can PUT to directly. Used by the
// upload path when we want to offload large transfers from the app.
func (b *Backend) PresignPut(ctx context.Context, hash, variant string, ttl time.Duration) (string, error) {
	if err := storage.ValidatePair(hash, variant); err != nil {
		return "", err
	}
	req, err := b.presign.PresignPutObject(ctx, &awss3.PutObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(b.keyFor(hash, variant)),
	}, func(p *awss3.PresignOptions) { p.Expires = ttl })
	if err != nil {
		return "", err
	}
	return req.URL, nil
}

// isNotFoundError catches the various "missing key" shapes the AWS
// SDK reports. HeadObject returns smithy.OperationError wrapping an
// http.ResponseError with status 404; GetObject returns a typed
// NoSuchKey.
func isNotFoundError(err error) bool {
	var nsk *s3types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var notFound *s3types.NotFound
	if errors.As(err, &notFound) {
		return true
	}
	return false
}
