// Package s3 is the S3-compatible Blob store backend, for AWS S3, MinIO,
// Ceph RGW, Cloudflare R2, Backblaze B2, Garage and others (OPS-1). It uses
// minio-go, streams every upload and download, and presigns downloads,
// single PUTs and multipart parts so file bytes need not pass through
// Hangar (DOWN-2).
package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/jamesagarside/hangar/internal/blob"
)

// Options configures the S3 backend. They mirror storage.s3 in the
// configuration.
type Options struct {
	// Endpoint is a host with an optional port, or a URL whose scheme
	// chooses HTTP or HTTPS. HTTPS is the default.
	Endpoint string
	Region   string
	Bucket   string
	// Prefix is prepended to every key, so several Instances can share a
	// bucket.
	Prefix string
	// AccessKeyID and SecretAccessKey are static credentials. When empty,
	// the AWS environment variables, the shared credentials file and the
	// instance, IRSA or Workload Identity roles are tried in that order.
	AccessKeyID     string
	SecretAccessKey string
	// UsePathStyle selects path-style bucket addressing, as MinIO and some
	// other stores need.
	UsePathStyle bool
	// Retries is how many times a failed part or object upload is sent
	// again, after a wait that starts at RetryBackoff and doubles up to 30
	// seconds. Zero means DefaultRetries; a negative value turns retries
	// off. A body that cannot be rewound is spooled to SpoolDir as it is
	// sent, so a retry can replay it.
	Retries int
	// RetryBackoff is the first wait before a retry. Zero means one second.
	RetryBackoff time.Duration
	// StallTimeout is how long an upload request may go without sending a
	// body byte or getting its response before it is cancelled and
	// retried. It is also the response header timeout of every request.
	// Zero means DefaultStallTimeout.
	StallTimeout time.Duration
	// SpoolDir holds upload parts while they are sent, so a failed request
	// can be retried. Empty means the system temporary directory.
	SpoolDir string
	// Transport overrides the HTTP transport, for tests.
	Transport http.RoundTripper
}

// singlePutMax is the largest object Put sends in one request. Larger
// objects stream through a multipart upload.
const singlePutMax = blob.DefaultPartSize

// Store is the S3 Blob store.
type Store struct {
	client *minio.Client
	core   *minio.Core
	bucket string
	region string
	prefix string

	retries      int
	retryBackoff time.Duration
	stallTimeout time.Duration
	spoolDir     string

	// bucketSeen is when a request last showed that the bucket exists, in
	// Unix nanoseconds, so Stat can tell a missing object from a missing
	// bucket without asking every time.
	bucketSeen atomic.Int64
}

// bucketRecheck is how long Stat trusts that the bucket exists before it
// checks again on a missing object.
const bucketRecheck = time.Minute

var (
	_ blob.Store          = (*Store)(nil)
	_ blob.VerifiedPutter = (*Store)(nil)
)

// New connects to the bucket. It does not make a request; call Check to
// test the connection.
func New(opts Options) (*Store, error) {
	if opts.Bucket == "" {
		return nil, errors.New("s3 blob store: the bucket is empty")
	}
	host, secure, err := parseEndpoint(opts.Endpoint)
	if err != nil {
		return nil, err
	}
	var creds *credentials.Credentials
	if opts.AccessKeyID != "" || opts.SecretAccessKey != "" {
		creds = credentials.NewStaticV4(opts.AccessKeyID, opts.SecretAccessKey, "")
	} else {
		creds = credentials.NewChainCredentials([]credentials.Provider{
			&credentials.EnvAWS{},
			&credentials.FileAWSCredentials{},
			&credentials.IAM{},
		})
	}
	lookup := minio.BucketLookupAuto
	if opts.UsePathStyle {
		lookup = minio.BucketLookupPath
	}
	st := &Store{
		bucket: opts.Bucket, region: opts.Region,
		retries: opts.Retries, retryBackoff: opts.RetryBackoff, stallTimeout: opts.StallTimeout, spoolDir: opts.SpoolDir,
	}
	switch {
	case st.retries == 0:
		st.retries = DefaultRetries
	case st.retries < 0:
		st.retries = 0
	}
	if st.retryBackoff <= 0 {
		st.retryBackoff = defaultRetryBackoff
	}
	if st.stallTimeout <= 0 {
		st.stallTimeout = DefaultStallTimeout
	}
	transport := opts.Transport
	if transport == nil {
		tr, err := minio.DefaultTransport(secure)
		if err != nil {
			return nil, fmt.Errorf("s3 blob store: %w", err)
		}
		// minio-go waits one minute for response headers. On a slow
		// uplink the last few megabytes of a part can still be in the
		// socket buffer when that clock starts, so use the stall timeout.
		tr.ResponseHeaderTimeout = st.stallTimeout
		transport = tr
	}
	core, err := minio.NewCore(host, &minio.Options{
		Creds:        creds,
		Secure:       secure,
		Region:       opts.Region,
		BucketLookup: lookup,
		Transport:    transport,
	})
	if err != nil {
		return nil, fmt.Errorf("s3 blob store: %w", err)
	}
	st.client, st.core = core.Client, core
	st.prefix = strings.Trim(opts.Prefix, "/")
	if st.prefix != "" {
		st.prefix += "/"
	}
	return st, nil
}

func parseEndpoint(endpoint string) (host string, secure bool, err error) {
	if endpoint == "" {
		return "s3.amazonaws.com", true, nil
	}
	if !strings.Contains(endpoint, "://") {
		return endpoint, true, nil
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return "", false, fmt.Errorf("s3 blob store: invalid endpoint %q", endpoint)
	}
	switch u.Scheme {
	case "https":
		return u.Host, true, nil
	case "http":
		return u.Host, false, nil
	default:
		return "", false, fmt.Errorf("s3 blob store: endpoint %q must use http or https", endpoint)
	}
}

// Check makes sure the bucket exists and the credentials can reach it,
// with a bucket-level request (HeadBucket), and explains the usual
// configuration mistakes: a missing bucket, a bucket in another region,
// and credentials from another account or without access.
func (s *Store) Check(ctx context.Context) error {
	ok, err := s.bucketExists(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("s3 blob store: bucket %q does not exist: create it, or set storage.s3.bucket to an existing bucket", s.bucket)
	}
	return nil
}

// bucketExists asks S3 whether the bucket exists and records the answer
// for Stat.
func (s *Store) bucketExists(ctx context.Context) (bool, error) {
	ok, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil {
		return false, s.bucketError(err)
	}
	if ok {
		s.bucketSeen.Store(time.Now().UnixNano())
	}
	return ok, nil
}

// bucketError explains an error from a bucket-level request.
func (s *Store) bucketError(err error) error {
	var resp minio.ErrorResponse
	if !errors.As(err, &resp) {
		return fmt.Errorf("s3 blob store: reach bucket %q: %w", s.bucket, err)
	}
	if resp.Region != "" && resp.Region != s.region && (resp.StatusCode == http.StatusMovedPermanently || resp.StatusCode == http.StatusBadRequest ||
		resp.Code == "PermanentRedirect" || resp.Code == "AuthorizationHeaderMalformed" || resp.Code == "InvalidRegion") {
		configured := ""
		if s.region != "" {
			configured = fmt.Sprintf(", not %q", s.region)
		}
		return fmt.Errorf("s3 blob store: bucket %q is in region %q%s: set storage.s3.region to %q: %w",
			s.bucket, resp.Region, configured, resp.Region, &apiError{code: "WrongRegion", err: err})
	}
	switch resp.StatusCode {
	case http.StatusMovedPermanently:
		return fmt.Errorf("s3 blob store: bucket %q is not at this endpoint (HTTP 301): check storage.s3.endpoint and storage.s3.region: %w",
			s.bucket, &apiError{code: "PermanentRedirect", err: err})
	case http.StatusForbidden:
		return fmt.Errorf("s3 blob store: access to bucket %q is denied (HTTP 403): check that the credentials are valid, "+
			"belong to the account that owns the bucket or one its policy allows, and that the bucket name is right: %w",
			s.bucket, &apiError{code: "AccessDenied", err: err})
	}
	return fmt.Errorf("s3 blob store: reach bucket %q: %w", s.bucket, mapErr(err, s.bucket))
}

// EnsureBucket creates the bucket if it does not exist. Tests and the
// quickstart Compose example use it; production buckets are created by
// the operator.
func (s *Store) EnsureBucket(ctx context.Context, region string) error {
	ok, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	err = s.client.MakeBucket(ctx, s.bucket, minio.MakeBucketOptions{Region: region})
	if err != nil && minio.ToErrorResponse(err).Code == "BucketAlreadyOwnedByYou" {
		return nil
	}
	return err
}

func (s *Store) objectKey(key string) (string, error) {
	if err := blob.ValidateKey(key); err != nil {
		return "", err
	}
	return s.prefix + key, nil
}

// apiError is an S3 error response that the blob package has no error for.
// It implements blob.CodedError, so spans record its S3 error code rather
// than its text.
type apiError struct {
	code string
	err  error
}

func (e *apiError) Error() string     { return e.err.Error() }
func (e *apiError) Unwrap() error     { return e.err }
func (e *apiError) ErrorCode() string { return "s3." + e.code }

// mapErr turns S3 errors into the blob package's errors.
func mapErr(err error, what string) error {
	if err == nil {
		return nil
	}
	var resp minio.ErrorResponse
	if !errors.As(err, &resp) {
		return err
	}
	switch resp.Code {
	case "NoSuchBucket":
		return fmt.Errorf("s3 blob store: bucket does not exist: %w", &apiError{code: resp.Code, err: err})
	case "NoSuchKey", "NoSuchUpload", "NotFound":
		return fmt.Errorf("%w: %s", blob.ErrNotFound, what)
	case "InvalidRange":
		return blob.ErrInvalidRange
	}
	switch resp.StatusCode {
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", blob.ErrNotFound, what)
	case http.StatusRequestedRangeNotSatisfiable:
		return blob.ErrInvalidRange
	}
	if resp.Code == "" {
		return err
	}
	return &apiError{code: resp.Code, err: err}
}

func infoFrom(key string, oi minio.ObjectInfo) blob.Info {
	return blob.Info{Key: key, Size: oi.Size, ModTime: oi.LastModified.UTC(), ETag: strings.Trim(oi.ETag, `"`)}
}

// Stat implements blob.Store.
func (s *Store) Stat(ctx context.Context, key string) (blob.Info, error) {
	k, err := s.objectKey(key)
	if err != nil {
		return blob.Info{}, err
	}
	oi, err := s.client.StatObject(ctx, s.bucket, k, minio.StatObjectOptions{})
	if err != nil {
		err = mapErr(err, key)
		if errors.Is(err, blob.ErrNotFound) {
			return blob.Info{}, s.notFound(ctx, err)
		}
		return blob.Info{}, err
	}
	s.bucketSeen.Store(time.Now().UnixNano())
	return infoFrom(key, oi), nil
}

// notFound checks that a missing object is not a missing bucket. A HEAD
// request has no error body, and AWS S3 answers HEAD for an object in a
// missing bucket with a bare 404, the same as for a missing object (MinIO
// adds an error code header). The bucket is checked at most once a minute
// while it exists.
func (s *Store) notFound(ctx context.Context, err error) error {
	if time.Since(time.Unix(0, s.bucketSeen.Load())) < bucketRecheck {
		return err
	}
	ok, berr := s.bucketExists(ctx)
	switch {
	case berr != nil:
		return berr
	case !ok:
		return fmt.Errorf("s3 blob store: bucket %q does not exist: %w", s.bucket,
			&apiError{code: "NoSuchBucket", err: minio.ErrorResponse{Code: "NoSuchBucket", StatusCode: http.StatusNotFound, BucketName: s.bucket}})
	}
	return err
}

// Get implements blob.Store. The request is made before Get returns, so
// a missing object or a bad range is reported here, not on the first Read.
func (s *Store) Get(ctx context.Context, key string, rng *blob.Range) (io.ReadCloser, blob.Info, error) {
	k, err := s.objectKey(key)
	if err != nil {
		return nil, blob.Info{}, err
	}
	opts := minio.GetObjectOptions{}
	if rng != nil {
		if rng.Start < 0 {
			return nil, blob.Info{}, blob.ErrInvalidRange
		}
		spec := "bytes=" + strconv.FormatInt(rng.Start, 10) + "-"
		if rng.End >= 0 {
			if rng.End < rng.Start {
				return nil, blob.Info{}, blob.ErrInvalidRange
			}
			spec += strconv.FormatInt(rng.End, 10)
		}
		opts.Set("Range", spec)
	}
	body, oi, header, err := s.core.GetObject(ctx, s.bucket, k, opts)
	if err != nil {
		return nil, blob.Info{}, mapErr(err, key)
	}
	info := infoFrom(key, oi)
	if rng != nil {
		total, ok := totalFromContentRange(header.Get("Content-Range"))
		if !ok {
			_ = body.Close()
			return nil, blob.Info{}, fmt.Errorf("s3 blob store: ranged GET of %s returned no Content-Range", key)
		}
		info.Size = total
	}
	return body, info, nil
}

// totalFromContentRange parses the total size from "bytes a-b/total".
func totalFromContentRange(v string) (int64, bool) {
	i := strings.LastIndexByte(v, '/')
	if i < 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(v[i+1:], 10, 64)
	return n, err == nil
}

// Put implements blob.Store. Objects up to 16 MiB, and objects of unknown
// size that turn out that small, go in one request; larger ones stream
// through a multipart upload, so memory use stays bounded whatever the size.
func (s *Store) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	k, err := s.objectKey(key)
	if err != nil {
		return err
	}
	if size > blob.MaxObjectSize {
		return blob.ErrTooLarge
	}
	if size > singlePutMax {
		return blob.PutMultipart(ctx, s, key, r, size)
	}
	// Read one more byte than allowed to detect a longer body.
	limit := singlePutMax
	if size >= 0 {
		limit = size
	}
	buf, err := io.ReadAll(io.LimitReader(blob.ContextReader(ctx, r), limit+1))
	if err != nil {
		return err
	}
	if size < 0 && int64(len(buf)) > singlePutMax {
		return s.putUnknownSize(ctx, key, io.MultiReader(bytes.NewReader(buf), r))
	}
	if size >= 0 && int64(len(buf)) != size {
		return fmt.Errorf("%w: read %d bytes, want %d", blob.ErrSizeMismatch, len(buf), size)
	}
	return s.putBytes(ctx, k, buf)
}

func (s *Store) putBytes(ctx context.Context, objectKey string, buf []byte) error {
	h := sha256.Sum256(buf)
	sum := hex.EncodeToString(h[:])
	body := newReplayBody(ctx, bytes.NewReader(buf), int64(len(buf)), s.spoolDir, s.retries > 0)
	defer body.close()
	err := s.upload(ctx, body, func(ctx context.Context, r io.Reader) error {
		_, err := s.core.PutObject(ctx, s.bucket, objectKey, r, int64(len(buf)), "", sum,
			minio.PutObjectOptions{DisableMultipart: true, ContentType: blob.SafeContentType})
		return err
	})
	return mapErr(err, objectKey)
}

// unknownSizePartSize returns the part size for part n of an upload of
// unknown size: 16 MiB, doubling every 2,000 parts up to 128 MiB. That
// covers objects of over 700 GiB while holding at most one part in memory.
func unknownSizePartSize(n int) int64 {
	shift := min((n-1)/2000, 3)
	return blob.DefaultPartSize << shift
}

// putUnknownSize streams a body of unknown length through a multipart
// upload, buffering one part at a time.
func (s *Store) putUnknownSize(ctx context.Context, key string, r io.Reader) error {
	id, err := s.CreateMultipart(ctx, key)
	if err != nil {
		return err
	}
	abort := func(cause error) error {
		_ = s.AbortMultipart(context.WithoutCancel(ctx), key, id)
		return cause
	}
	r = blob.ContextReader(ctx, r)
	var parts []blob.Part
	buf := make([]byte, 0, blob.DefaultPartSize)
	for n := 1; ; n++ {
		if n > blob.MaxParts {
			return abort(blob.ErrTooLarge)
		}
		size := unknownSizePartSize(n)
		if int64(cap(buf)) < size {
			buf = make([]byte, 0, size)
		}
		buf = buf[:size]
		read, err := io.ReadFull(r, buf)
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
			return abort(err)
		}
		if read == 0 && n > 1 {
			break
		}
		etag, perr := s.PutPart(ctx, key, id, n, bytes.NewReader(buf[:read]), int64(read))
		if perr != nil {
			return abort(perr)
		}
		parts = append(parts, blob.Part{Number: n, ETag: etag, Size: int64(read)})
		if int64(read) < size {
			break
		}
	}
	if err := s.CompleteMultipart(ctx, key, id, parts); err != nil {
		return abort(err)
	}
	return nil
}

// PutVerified implements blob.VerifiedPutter. Small objects are hashed in
// memory before the upload; larger ones stream through a multipart upload
// that is completed only when the SHA-256 matches.
func (s *Store) PutVerified(ctx context.Context, key string, r io.Reader, size int64, sum []byte) error {
	k, err := s.objectKey(key)
	if err != nil {
		return err
	}
	if len(sum) != sha256.Size {
		return fmt.Errorf("put verified %s: want a %d-byte SHA-256", key, sha256.Size)
	}
	if size < 0 {
		return fmt.Errorf("put verified %s: the size must be known", key)
	}
	if size > singlePutMax {
		return blob.PutVerifiedMultipart(ctx, s, key, r, size, sum)
	}
	buf, err := io.ReadAll(io.LimitReader(blob.ContextReader(ctx, r), size+1))
	if err != nil {
		return err
	}
	if int64(len(buf)) != size {
		return fmt.Errorf("%w: read %d bytes, want %d", blob.ErrSizeMismatch, len(buf), size)
	}
	if got := sha256.Sum256(buf); !bytes.Equal(got[:], sum) {
		return fmt.Errorf("%w: got %s, want %s", blob.ErrChecksumMismatch, hex.EncodeToString(got[:]), hex.EncodeToString(sum))
	}
	return s.putBytes(ctx, k, buf)
}

// Delete implements blob.Store.
func (s *Store) Delete(ctx context.Context, key string) error {
	k, err := s.objectKey(key)
	if err != nil {
		return err
	}
	err = s.client.RemoveObject(ctx, s.bucket, k, minio.RemoveObjectOptions{})
	if err = mapErr(err, key); errors.Is(err, blob.ErrNotFound) {
		return nil
	}
	return err
}

// List implements blob.Store. S3 lists keys in UTF-8 binary order, which is
// Go's string order.
func (s *Store) List(ctx context.Context, prefix string, fn func(blob.Info) error) error {
	lctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for oi := range s.client.ListObjects(lctx, s.bucket, minio.ListObjectsOptions{Prefix: s.prefix + prefix, Recursive: true}) {
		if oi.Err != nil {
			return mapErr(oi.Err, prefix)
		}
		key := strings.TrimPrefix(oi.Key, s.prefix)
		if strings.HasSuffix(key, "/") {
			// A zero-byte directory marker some tools create; not a key.
			continue
		}
		if err := fn(infoFrom(key, oi)); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// Copy implements blob.Store. ComposeObject copies server-side, in parts
// for objects over 5 GiB. The copy's Content-Type is replaced with
// blob.SafeContentType rather than copied, so a source stored with another
// type does not pass it on.
func (s *Store) Copy(ctx context.Context, srcKey, dstKey string) error {
	src, err := s.objectKey(srcKey)
	if err != nil {
		return err
	}
	dst, err := s.objectKey(dstKey)
	if err != nil {
		return err
	}
	_, err = s.client.ComposeObject(ctx,
		minio.CopyDestOptions{
			Bucket: s.bucket, Object: dst, ContentType: blob.SafeContentType,
			ReplaceMetadata: true, UserMetadata: map[string]string{"Content-Type": blob.SafeContentType},
		},
		minio.CopySrcOptions{Bucket: s.bucket, Object: src})
	return mapErr(err, srcKey)
}

// CreateMultipart implements blob.Store. The upload's Content-Type is set
// here, by Hangar, so clients uploading parts through presigned URLs cannot
// choose it.
func (s *Store) CreateMultipart(ctx context.Context, key string) (string, error) {
	k, err := s.objectKey(key)
	if err != nil {
		return "", err
	}
	id, err := s.core.NewMultipartUpload(ctx, s.bucket, k, minio.PutObjectOptions{ContentType: blob.SafeContentType})
	return id, mapErr(err, key)
}

// PresignPart implements blob.Store.
func (s *Store) PresignPart(ctx context.Context, key, uploadID string, part int, ttl time.Duration) (string, error) {
	k, err := s.objectKey(key)
	if err != nil {
		return "", err
	}
	if part < 1 || part > blob.MaxParts {
		return "", fmt.Errorf("part number %d is out of range 1-%d", part, blob.MaxParts)
	}
	q := url.Values{"partNumber": {strconv.Itoa(part)}, "uploadId": {uploadID}}
	u, err := s.client.Presign(ctx, http.MethodPut, s.bucket, k, ttl, q)
	if err != nil {
		return "", mapErr(err, key)
	}
	return u.String(), nil
}

// PutPart implements blob.Store.
func (s *Store) PutPart(ctx context.Context, key, uploadID string, part int, r io.Reader, size int64) (string, error) {
	k, err := s.objectKey(key)
	if err != nil {
		return "", err
	}
	if part < 1 || part > blob.MaxParts {
		return "", fmt.Errorf("part number %d is out of range 1-%d", part, blob.MaxParts)
	}
	if size < 0 || size > blob.MaxPartSize {
		return "", fmt.Errorf("part %d: the size must be known and at most 5 GiB", part)
	}
	body := newReplayBody(ctx, r, size, s.spoolDir, s.retries > 0)
	var etag string
	err = s.upload(ctx, body, func(ctx context.Context, br io.Reader) error {
		op, err := s.core.PutObjectPart(ctx, s.bucket, k, uploadID, part, br, size, minio.PutObjectPartOptions{})
		etag = op.ETag
		return err
	})
	read := body.sourceRead()
	body.close()
	if err != nil {
		if errors.Is(err, blob.ErrSizeMismatch) {
			return "", fmt.Errorf("%w: part %d read %d bytes, want %d", blob.ErrSizeMismatch, part, read, size)
		}
		return "", fmt.Errorf("upload part %d: %w", part, mapErr(err, key))
	}
	if read != size {
		return "", fmt.Errorf("%w: part %d read %d bytes, want %d", blob.ErrSizeMismatch, part, read, size)
	}
	if n, _ := io.ReadFull(r, make([]byte, 1)); n > 0 {
		return "", fmt.Errorf("%w: part %d is longer than %d bytes", blob.ErrSizeMismatch, part, size)
	}
	return strings.Trim(etag, `"`), nil
}

// ListParts implements blob.Store.
func (s *Store) ListParts(ctx context.Context, key, uploadID string) ([]blob.Part, error) {
	k, err := s.objectKey(key)
	if err != nil {
		return nil, err
	}
	var parts []blob.Part
	marker := 0
	for {
		res, err := s.core.ListObjectParts(ctx, s.bucket, k, uploadID, marker, 1000)
		if err != nil {
			return nil, mapErr(err, key)
		}
		for _, p := range res.ObjectParts {
			parts = append(parts, blob.Part{Number: p.PartNumber, ETag: strings.Trim(p.ETag, `"`), Size: p.Size})
		}
		if !res.IsTruncated {
			return parts, nil
		}
		marker = res.NextPartNumberMarker
	}
}

// CompleteMultipart implements blob.Store.
func (s *Store) CompleteMultipart(ctx context.Context, key, uploadID string, parts []blob.Part) error {
	k, err := s.objectKey(key)
	if err != nil {
		return err
	}
	if len(parts) == 0 {
		return errors.New("complete multipart upload: no parts")
	}
	cp := make([]minio.CompletePart, len(parts))
	for i, p := range parts {
		cp[i] = minio.CompletePart{PartNumber: p.Number, ETag: strings.Trim(p.ETag, `"`)}
	}
	_, err = s.core.CompleteMultipartUpload(ctx, s.bucket, k, uploadID, cp, minio.PutObjectOptions{})
	return mapErr(err, key)
}

// AbortMultipart implements blob.Store.
func (s *Store) AbortMultipart(ctx context.Context, key, uploadID string) error {
	k, err := s.objectKey(key)
	if err != nil {
		return err
	}
	return mapErr(s.core.AbortMultipartUpload(ctx, s.bucket, k, uploadID), key)
}

// PresignGet implements blob.Store. The URL overrides the response's
// Content-Type with blob.SafeContentType and its Content-Disposition with an
// attachment naming filename, whatever the object was stored with, so a
// browser never renders an uploaded file (HTML, SVG) on the bucket's
// origin.
func (s *Store) PresignGet(ctx context.Context, key string, ttl time.Duration, filename string) (string, error) {
	k, err := s.objectKey(key)
	if err != nil {
		return "", err
	}
	q := url.Values{}
	q.Set("response-content-type", blob.SafeContentType)
	q.Set("response-content-disposition", blob.ContentDisposition(filename))
	u, err := s.client.PresignedGetObject(ctx, s.bucket, k, ttl, q)
	if err != nil {
		return "", mapErr(err, key)
	}
	return u.String(), nil
}

// PresignPut implements blob.Store. Headers always carries Content-Type
// blob.SafeContentType. With sum, the URL signs that Content-Type and an
// x-amz-checksum-sha256 header, the client must send both, and the store
// rejects a body with another SHA-256 or a request with another
// Content-Type. Without sum nothing is signed, because huggingface_hub
// sends no headers on a basic upload (docs/research/s3-presigned-checksum.md)
// and a signed header it leaves out fails the upload; the stored type is
// then whatever the client sent, which is safe because every download
// forces blob.SafeContentType (PresignGet, blob.Downloads.Serve). Objects
// over 5 GiB need multipart.
func (s *Store) PresignPut(ctx context.Context, key string, size int64, sum []byte, ttl time.Duration) (blob.PresignedRequest, error) {
	k, err := s.objectKey(key)
	if err != nil {
		return blob.PresignedRequest{}, err
	}
	if size > blob.MaxSinglePut {
		return blob.PresignedRequest{}, blob.ErrNotSupported
	}
	headers := http.Header{}
	out := map[string]string{"Content-Type": blob.SafeContentType}
	if sum != nil {
		if len(sum) != sha256.Size {
			return blob.PresignedRequest{}, fmt.Errorf("presign put %s: want a %d-byte SHA-256", key, sha256.Size)
		}
		v := base64.StdEncoding.EncodeToString(sum)
		headers.Set("Content-Type", blob.SafeContentType)
		headers.Set("x-amz-checksum-sha256", v)
		out["x-amz-checksum-sha256"] = v
	}
	u, err := s.client.PresignHeader(ctx, http.MethodPut, s.bucket, k, ttl, nil, headers)
	if err != nil {
		return blob.PresignedRequest{}, mapErr(err, key)
	}
	return blob.PresignedRequest{URL: u.String(), Headers: out}, nil
}

// The S3 backend does not implement blob.UploadLister: MinIO only lists
// multipart uploads for an exact object name. Abandoned multipart uploads
// are aborted by the upload.expire Job from their upload rows, and by the
// bucket's lifecycle rule (AbortIncompleteMultipartUpload) on AWS, or
// MinIO's stale upload expiry (24 hours by default).
