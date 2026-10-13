package blob

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Download modes (storage.s3.download_mode).
const (
	// DownloadRedirect sends clients to presigned URLs, so file bytes do not
	// pass through Hangar (DOWN-2).
	DownloadRedirect = "redirect"
	// DownloadProxy streams file bytes through Hangar.
	DownloadProxy = "proxy"
)

// DefaultPresignTTL is the lifetime of presigned download URLs when the
// configuration does not set one.
const DefaultPresignTTL = time.Hour

// Downloads decides how a stored object reaches a client: a redirect to a
// presigned URL, or bytes streamed through Hangar. The Hub API, the git-lfs
// batch API and browser downloads share it so every protocol follows
// storage.s3.download_mode.
type Downloads struct {
	Store Store
	// Mode is DownloadRedirect or DownloadProxy. Empty means redirect.
	Mode string
	// TTL is the presigned URL lifetime. Zero means DefaultPresignTTL.
	TTL time.Duration
}

// Redirects reports whether downloads should be redirected when the store
// can presign.
func (d Downloads) Redirects() bool { return d.Mode == "" || d.Mode == DownloadRedirect }

// RedirectURL returns a presigned URL for key and true when the client
// should be redirected. It returns "" and false, with no error, when the
// mode is proxy or the store cannot presign (the filesystem backend); the
// caller then streams the object itself. filename sets the download's
// Content-Disposition. The URL is a credential: never log it.
func (d Downloads) RedirectURL(ctx context.Context, key, filename string) (string, bool, error) {
	if !d.Redirects() {
		return "", false, nil
	}
	ttl := d.TTL
	if ttl <= 0 {
		ttl = DefaultPresignTTL
	}
	u, err := d.Store.PresignGet(ctx, key, ttl, filename)
	if errors.Is(err, ErrNotSupported) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("presign download: %w", err)
	}
	return u, true, nil
}

// ExpiresIn returns the presigned URL lifetime in whole seconds, for the
// git-lfs batch response's expires_in.
func (d Downloads) ExpiresIn() int {
	ttl := d.TTL
	if ttl <= 0 {
		ttl = DefaultPresignTTL
	}
	return int(ttl / time.Second)
}
