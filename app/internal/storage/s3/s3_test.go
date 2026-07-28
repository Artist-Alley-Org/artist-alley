// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package s3_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/mscrnt/artist-alley/app/internal/storage"
	"github.com/mscrnt/artist-alley/app/internal/storage/s3"
	"github.com/mscrnt/artist-alley/app/internal/storage/storagetest"
)

// TestS3_BackendContract runs the shared storage contract harness
// against a real S3-compatible endpoint. Skips when the test
// environment isn't set up — scripts/test.sh's --with-s3 mode (or any
// CI runner that exports the AA_S3_TEST_* env vars) flips this on.
//
// Each subtest of the contract creates fresh content with random
// hashes, so the test bucket can be shared with other test runs
// without isolation issues.
func TestS3_BackendContract(t *testing.T) {
	endpoint := os.Getenv("AA_S3_TEST_ENDPOINT")
	bucket := os.Getenv("AA_S3_TEST_BUCKET")
	access := os.Getenv("AA_S3_TEST_ACCESS_KEY")
	secret := os.Getenv("AA_S3_TEST_SECRET_KEY")
	if endpoint == "" || bucket == "" || access == "" || secret == "" {
		t.Skip("AA_S3_TEST_* env not set; s3 backend tests skipped (start MinIO via scripts/test.sh --with-s3)")
	}

	// Make sure the bucket exists. Cheaper than asking the caller to
	// pre-provision it for every test run.
	ensureBucket(t, endpoint, bucket, access, secret)

	storagetest.RunBackendContract(t, func(t *testing.T) storage.Backend {
		t.Helper()
		ctx := t.Context()

		b, err := s3.New(ctx, s3.Config{
			Endpoint:     endpoint,
			Bucket:       bucket,
			Region:       "us-east-1",
			AccessKey:    access,
			SecretKey:    secret,
			UsePathStyle: true, // MinIO default
		})
		if err != nil {
			t.Fatalf("s3.New: %v", err)
		}
		return b
	})
}

func TestS3_New_RejectsEmptyBucket(t *testing.T) {
	if _, err := s3.New(context.Background(), s3.Config{}); err == nil {
		t.Errorf("s3.New with empty bucket should error")
	}
}

func TestS3_Name(t *testing.T) {
	// Construct a Backend struct directly via test by going through
	// the real New is hard without a live endpoint; just probe the
	// stable Name() value from the package constant intent. We use
	// the package-level helper to keep this lightweight.
	endpoint := os.Getenv("AA_S3_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("AA_S3_TEST_ENDPOINT not set; Name() check requires a live backend instance")
	}
	bucket := os.Getenv("AA_S3_TEST_BUCKET")
	access := os.Getenv("AA_S3_TEST_ACCESS_KEY")
	secret := os.Getenv("AA_S3_TEST_SECRET_KEY")
	b, err := s3.New(context.Background(), s3.Config{
		Endpoint: endpoint, Bucket: bucket, Region: "us-east-1",
		AccessKey: access, SecretKey: secret, UsePathStyle: true,
	})
	if err != nil {
		t.Fatalf("s3.New: %v", err)
	}
	if got := b.Name(); got != "s3" {
		t.Errorf("Name=%q want s3", got)
	}
}

// ensureBucket creates the test bucket if it doesn't exist. Safe to
// call on every run.
func ensureBucket(t *testing.T, endpoint, bucket, access, secret string) {
	t.Helper()
	ctx := t.Context()

	awsCfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(access, secret, "")),
	)
	if err != nil {
		t.Fatalf("aws config: %v", err)
	}
	cli := awss3.NewFromConfig(awsCfg, func(o *awss3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})

	_, err = cli.HeadBucket(ctx, &awss3.HeadBucketInput{Bucket: aws.String(bucket)})
	if err == nil {
		return // exists
	}
	_, err = cli.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String(bucket)})
	if err != nil {
		// Race-tolerant: another goroutine could have created it.
		var alreadyOwned *s3types.BucketAlreadyOwnedByYou
		if !asTypedErr(err, &alreadyOwned) {
			t.Fatalf("create bucket %q: %v", bucket, err)
		}
	}
}

// asTypedErr is errors.As without a typed unused-variable hassle in tests.
func asTypedErr(err error, target any) bool {
	type unwrappable interface{ Unwrap() error }
	for {
		switch v := target.(type) {
		case **s3types.BucketAlreadyOwnedByYou:
			if e, ok := err.(*s3types.BucketAlreadyOwnedByYou); ok {
				*v = e
				return true
			}
		}
		u, ok := err.(unwrappable)
		if !ok {
			return false
		}
		err = u.Unwrap()
		if err == nil {
			return false
		}
	}
}

// (intentionally unused: keeps random-content seeding logic local to
// the file if we ever decide to switch the contract harness to
// deterministic hashes)
func randomHash() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
