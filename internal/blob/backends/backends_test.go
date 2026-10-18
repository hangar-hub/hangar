package backends_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/blob"
	"github.com/hangar-hub/hangar/internal/blob/backends"
	"github.com/hangar-hub/hangar/internal/blob/blobtest"
	"github.com/hangar-hub/hangar/internal/config"
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

func TestS3OptionsProxyModes(t *testing.T) {
	o := backends.S3Options(config.S3Config{Bucket: "hangar", DownloadMode: config.DownloadRedir, UploadMode: config.UploadDirect})
	require.False(t, o.ProxyUploads)
	require.False(t, o.ProxyDownloads)
	o = backends.S3Options(config.S3Config{Bucket: "hangar", DownloadMode: config.DownloadProxy, UploadMode: config.UploadProxy})
	require.True(t, o.ProxyUploads, "upload_mode proxy stops presigned uploads")
	require.True(t, o.ProxyDownloads, "download_mode proxy stops presigned downloads, zip exports included")
}

func TestOpenS3(t *testing.T) {
	blobtest.RequireS3(t)
	srv := blobtest.S3TestServer(t)
	_ = blobtest.S3(t) // creates the bucket
	cfg := config.StorageConfig{Backend: config.BackendS3, S3: config.S3Config{
		Endpoint: srv.Endpoint, Region: srv.Region, Bucket: srv.Bucket, Prefix: "backends",
		AccessKeyID: srv.AccessKey, SecretAccessKey: srv.SecretKey, UsePathStyle: true,
		DownloadMode: blob.DownloadRedirect, UploadMode: config.UploadDirect, PresignTTL: time.Minute,
	}}
	s, err := backends.Open(context.Background(), cfg)
	require.NoError(t, err)
	d := backends.Downloads(cfg, s)
	require.Equal(t, 60, d.ExpiresIn())
	u, ok, err := d.RedirectURL(context.Background(), "lfs/x", "x.bin")
	require.NoError(t, err)
	require.True(t, ok)
	require.Contains(t, u, "X-Amz-Signature")

	_, err = s.PresignPut(context.Background(), "lfs/x", 1, nil, time.Minute)
	require.NoError(t, err, "upload_mode direct presigns uploads")

	cfg.S3.DownloadMode = blob.DownloadProxy
	_, ok, err = backends.Downloads(cfg, s).RedirectURL(context.Background(), "lfs/x", "x.bin")
	require.NoError(t, err)
	require.False(t, ok)

	cfg.S3.UploadMode = config.UploadProxy
	s, err = backends.Open(context.Background(), cfg)
	require.NoError(t, err)
	_, err = s.PresignPut(context.Background(), "lfs/x", 1, nil, time.Minute)
	require.ErrorIs(t, err, blob.ErrNotSupported, "upload_mode proxy makes callers fall back to Hangar URLs")
	_, err = s.PresignGet(context.Background(), "lfs/x", time.Minute, "x.bin")
	require.ErrorIs(t, err, blob.ErrNotSupported, "download_mode proxy makes zip exports stream too")

	cfg.S3.Bucket = "hangar-no-such-bucket"
	_, err = backends.Open(context.Background(), cfg)
	require.Error(t, err)
}

func TestS3OptionsRetries(t *testing.T) {
	opts := backends.S3Options(config.S3Config{Retries: 3, StallTimeout: 5 * time.Minute})
	require.Equal(t, 3, opts.Retries)
	require.Equal(t, 5*time.Minute, opts.StallTimeout)
	require.Equal(t, -1, backends.S3Options(config.S3Config{}).Retries, "storage.s3.retries 0 turns retries off")
	def := config.Default().Storage.S3
	require.Equal(t, 5, backends.S3Options(def).Retries)
	require.Equal(t, 2*time.Minute, backends.S3Options(def).StallTimeout)
}
