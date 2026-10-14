package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
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

// SafeContentType is the only media type Hangar stores objects with and
// serves them as. Uploaders choose file names and bytes, so serving an
// object as text/html or image/svg+xml would let anyone who can upload run
// script on the Blob store's origin (stored cross-site scripting). Every
// backend pins it on writes, and every download forces it.
const SafeContentType = "application/octet-stream"

// ContentDisposition returns an attachment Content-Disposition for
// filename, with an RFC 6266 filename* for names that are not plain ASCII.
// An empty filename gives a bare attachment. Downloads are always
// attachments, so a browser saves the file instead of rendering it.
func ContentDisposition(filename string) string {
	if filename == "" {
		return "attachment"
	}
	if v := mime.FormatMediaType("attachment", map[string]string{"filename": filename}); v != "" {
		return v
	}
	return `attachment; filename="download"`
}

// SetDownloadHeaders sets the headers every download of a stored object
// carries, whatever the file is: Content-Type application/octet-stream,
// an attachment Content-Disposition naming filename, and
// X-Content-Type-Options nosniff, so a browser never renders uploaded
// content as a page. It replaces any values already set.
func SetDownloadHeaders(h http.Header, filename string) {
	h.Set("Content-Type", SafeContentType)
	h.Set("Content-Disposition", ContentDisposition(filename))
	h.Set("X-Content-Type-Options", "nosniff")
}

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
// caller then streams the object with Serve. filename names the file in
// the download's attachment Content-Disposition; the URL also forces
// Content-Type application/octet-stream. The URL is a credential: never
// log it.
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

// Serve streams the object at key through Hangar: the proxy download mode,
// and every download from a store that cannot presign. It answers GET and
// HEAD, honours a single-range Range header with 206 (and 416 when the
// range is unsatisfiable), and always sends the headers of
// SetDownloadHeaders, overriding any the caller set. It sets ETag only when
// the caller has not, so protocols that define their own ETag keep it.
//
// When the object does not exist, Serve writes nothing and returns an
// error wrapping ErrNotFound, so the caller answers in its protocol's error
// format. Any other error before the response starts is returned the same
// way; an error while copying the body is returned after the headers are
// sent.
func (d Downloads) Serve(w http.ResponseWriter, r *http.Request, key, filename string) error {
	ctx := r.Context()
	info, err := d.Store.Stat(ctx, key)
	if err != nil {
		return err
	}
	rng, rerr := ParseRange(r.Header.Get("Range"), info.Size)
	status, length := http.StatusOK, info.Size
	if rng != nil {
		status, length = http.StatusPartialContent, rng.Length(info.Size)
	}
	var body io.ReadCloser
	if rerr == nil && r.Method != http.MethodHead {
		if body, _, err = d.Store.Get(ctx, key, rng); err != nil {
			return err
		}
		defer body.Close()
	}

	h := w.Header()
	SetDownloadHeaders(h, filename)
	h.Set("Accept-Ranges", "bytes")
	if h.Get("ETag") == "" && info.ETag != "" {
		h.Set("ETag", `"`+info.ETag+`"`)
	}
	if !info.ModTime.IsZero() {
		h.Set("Last-Modified", info.ModTime.UTC().Format(http.TimeFormat))
	}
	if rerr != nil {
		h.Set("Content-Range", "bytes */"+strconv.FormatInt(info.Size, 10))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return nil
	}
	if rng != nil {
		h.Set("Content-Range", rng.ContentRange(info.Size))
	}
	h.Set("Content-Length", strconv.FormatInt(length, 10))
	w.WriteHeader(status)
	if body == nil {
		return nil
	}
	if _, err := io.Copy(w, body); err != nil {
		return fmt.Errorf("stream %s: %w", key, err)
	}
	return nil
}
