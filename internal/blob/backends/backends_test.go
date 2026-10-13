package backends_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/blob/backends"
	"github.com/jamesagarside/hangar/internal/blob/blobtest"
	"github.com/jamesagarside/hangar/internal/config"
)

func TestOpenFilesystem(t *testing.T) {
	cfg := config.StorageConfig{Backend: config.BackendFS, FS: config.FSConfig{Path: filepath.Join(t.TempDir(), "b")}}
	s, err := backends.Open(context.Background(), cfg)
	require.NoError(t, err)
	d := backends.Downloads(cfg, s)
	_, ok, err := d.RedirectURL(context.Background(), "lfs/x", "x")
	require.NoError(t, err)
	require.False(t, ok, "the filesystem backend always streams")
}

func TestOpenErrors(t *testing.T) {
	_, err := backends.Open(context.Background(), config.StorageConfig{Backend: "tape"})
	require.Error(t, err)
	_, err = backends.Open(context.Background(), config.StorageConfig{Backend: config.BackendS3})
	require.Error(t, err, "the bucket is required")
}

func TestOpenS3(t *testing.T) {
	if !blobtest.S3Enabled() {
		t.Logf("S3 tests not registered: set %s=1 (needs Docker)", blobtest.EnvMinIO)
		return
	}
	srv := blobtest.S3TestServer(t)
	_ = blobtest.S3(t) // creates the bucket
	cfg := config.StorageConfig{Backend: config.BackendS3, S3: config.S3Config{
		Endpoint: srv.Endpoint, Region: srv.Region, Bucket: srv.Bucket, Prefix: "backends",
		AccessKeyID: srv.AccessKey, SecretAccessKey: srv.SecretKey, UsePathStyle: true,
		DownloadMode: blob.DownloadRedirect, PresignTTL: time.Minute,
	}}
	s, err := backends.Open(context.Background(), cfg)
	require.NoError(t, err)
	d := backends.Downloads(cfg, s)
	require.Equal(t, 60, d.ExpiresIn())
	u, ok, err := d.RedirectURL(context.Background(), "lfs/x", "x.bin")
	require.NoError(t, err)
	require.True(t, ok)
	require.Contains(t, u, "X-Amz-Signature")

	cfg.S3.DownloadMode = blob.DownloadProxy
	_, ok, err = backends.Downloads(cfg, s).RedirectURL(context.Background(), "lfs/x", "x.bin")
	require.NoError(t, err)
	require.False(t, ok)

	cfg.S3.Bucket = "hangar-no-such-bucket"
	_, err = backends.Open(context.Background(), cfg)
	require.Error(t, err)
}
