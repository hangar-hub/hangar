package blobtest

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	tcminio "github.com/testcontainers/testcontainers-go/modules/minio"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/blob/s3"
)

// Environment variables that turn on tests against S3.
//
// HANGAR_TEST_MINIO=1 starts a MinIO container with testcontainers (needs
// Docker). HANGAR_TEST_S3_ENDPOINT, with HANGAR_TEST_S3_ACCESS_KEY,
// HANGAR_TEST_S3_SECRET_KEY and optionally HANGAR_TEST_S3_BUCKET and
// HANGAR_TEST_S3_REGION, uses an existing S3-compatible server instead.
const (
	EnvMinIO       = "HANGAR_TEST_MINIO"
	EnvMinIOImage  = "HANGAR_TEST_MINIO_IMAGE"
	EnvS3Endpoint  = "HANGAR_TEST_S3_ENDPOINT"
	EnvS3AccessKey = "HANGAR_TEST_S3_ACCESS_KEY"
	EnvS3SecretKey = "HANGAR_TEST_S3_SECRET_KEY"
	EnvS3Bucket    = "HANGAR_TEST_S3_BUCKET"
	EnvS3Region    = "HANGAR_TEST_S3_REGION"
	// MinIOImage is the image testcontainers starts. MinIO stopped
	// publishing community images in 2025; this is a maintained build of
	// the same server.
	MinIOImage = "pgsty/minio:RELEASE.2026-08-04T00-00-00Z"
)

// S3Enabled reports whether S3 tests should run.
func S3Enabled() bool {
	return os.Getenv(EnvS3Endpoint) != "" || os.Getenv(EnvMinIO) == "1"
}

// S3Server describes the test S3 server.
type S3Server struct {
	Endpoint  string
	AccessKey string
	SecretKey string
	Bucket    string
	Region    string
}

var (
	s3Once   sync.Once
	s3Server S3Server
	s3Err    error
)

// S3TestServer returns the shared test S3 server, starting MinIO on first
// use. It fails the test if S3 tests are not enabled or the server does not
// start. Testcontainers' reaper removes the container when the test
// process exits.
func S3TestServer(t testing.TB) S3Server {
	t.Helper()
	if !S3Enabled() {
		t.Fatalf("S3 tests are not enabled: set %s=1 (needs Docker) or %s", EnvMinIO, EnvS3Endpoint)
	}
	s3Once.Do(func() {
		if ep := os.Getenv(EnvS3Endpoint); ep != "" {
			s3Server = S3Server{
				Endpoint: ep, AccessKey: os.Getenv(EnvS3AccessKey), SecretKey: os.Getenv(EnvS3SecretKey),
				Bucket: os.Getenv(EnvS3Bucket), Region: os.Getenv(EnvS3Region),
			}
			if s3Server.Bucket == "" {
				s3Server.Bucket = "hangar-test"
			}
			return
		}
		image := os.Getenv(EnvMinIOImage)
		if image == "" {
			image = MinIOImage
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		c, err := tcminio.Run(ctx, image, tcminio.WithUsername("hangar"), tcminio.WithPassword("hangar-secret"))
		if err != nil {
			s3Err = fmt.Errorf("run %s: %w", image, err)
			return
		}
		addr, err := c.ConnectionString(ctx)
		if err != nil {
			s3Err = err
			return
		}
		s3Server = S3Server{Endpoint: "http://" + addr, AccessKey: "hangar", SecretKey: "hangar-secret", Bucket: "hangar-test", Region: "us-east-1"}
	})
	if s3Err != nil {
		t.Fatalf("start test S3 server: %v", s3Err)
	}
	return s3Server
}

// S3 returns an empty S3 Blob store on the test server: the shared bucket
// with a fresh key prefix, so tests do not see each other's objects.
func S3(t testing.TB) *s3.Store {
	t.Helper()
	srv := S3TestServer(t)
	opts := srv.Options()
	opts.Prefix = "test-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	st, err := s3.New(opts)
	if err != nil {
		t.Fatalf("open test S3 store: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := st.EnsureBucket(ctx, srv.Region); err != nil {
		t.Fatalf("create test bucket: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = st.List(ctx, "", func(i blob.Info) error { return st.Delete(ctx, i.Key) })
	})
	return st
}

// Options returns s3.Options for the server, without a prefix.
func (s S3Server) Options() s3.Options {
	return s3.Options{
		Endpoint: s.Endpoint, Region: s.Region, Bucket: s.Bucket,
		AccessKeyID: s.AccessKey, SecretAccessKey: s.SecretKey, UsePathStyle: true,
	}
}
