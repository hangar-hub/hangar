// Package backends opens the Blob store backend that the configuration
// names, so cmd/hangar does not depend on each backend package.
package backends

import (
	"context"
	"fmt"

	"github.com/jamesagarside/hangar/internal/blob"
	blobfs "github.com/jamesagarside/hangar/internal/blob/fs"
	"github.com/jamesagarside/hangar/internal/blob/s3"
	"github.com/jamesagarside/hangar/internal/config"
)

// Open returns the configured Blob store. For S3 it checks that the bucket
// exists and the credentials reach it, so a bad configuration fails at
// start-up rather than on the first upload.
func Open(ctx context.Context, cfg config.StorageConfig) (blob.Store, error) {
	switch cfg.Backend {
	case config.BackendFS, "":
		return blobfs.New(cfg.FS.Path)
	case config.BackendS3:
		st, err := s3.New(S3Options(cfg.S3))
		if err != nil {
			return nil, err
		}
		if err := st.Check(ctx); err != nil {
			return nil, err
		}
		return st, nil
	default:
		return nil, fmt.Errorf("unknown storage.backend %q: use fs or s3", cfg.Backend)
	}
}

// S3Options converts storage.s3 into s3.Options.
func S3Options(c config.S3Config) s3.Options {
	return s3.Options{
		Endpoint: c.Endpoint, Region: c.Region, Bucket: c.Bucket, Prefix: c.Prefix,
		AccessKeyID: c.AccessKeyID, SecretAccessKey: c.SecretAccessKey, UsePathStyle: c.UsePathStyle,
		ProxyUploads: c.UploadMode == config.UploadProxy, ProxyDownloads: c.DownloadMode == config.DownloadProxy,
	}
}

// Downloads returns the download policy for the configuration: redirects
// to presigned URLs on S3 in redirect mode, and streaming otherwise.
func Downloads(cfg config.StorageConfig, s blob.Store) blob.Downloads {
	if cfg.Backend != config.BackendS3 {
		return blob.Downloads{Store: s, Mode: blob.DownloadProxy}
	}
	return blob.Downloads{Store: s, Mode: cfg.S3.DownloadMode, TTL: cfg.S3.PresignTTL}
}
