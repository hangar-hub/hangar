package gitserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/config"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/telemetry"
)

// LFS media type and transfer adapters.
const (
	lfsMediaType      = "application/vnd.git-lfs+json"
	transferBasic     = "basic"
	transferMultipart = "multipart"
	// maxBatchObjects bounds one batch request; git-lfs sends 100.
	maxBatchObjects = 1000
	// maxBatchBody bounds the batch request body.
	maxBatchBody = 4 << 20
	// maxSmallBody bounds verify and completion request bodies.
	maxSmallBody = 2 << 20
	// maxParts is S3's limit on parts per upload.
	maxParts = 10000
	// minPartSize is the default smallest part.
	minPartSize = 16 << 20
)

// serveLFS routes the git-lfs API under `<repo>.git/info/lfs/`.
func (s *Server) serveLFS(w http.ResponseWriter, r *http.Request) {
	rp, err := parseRepoPath(r.URL.EscapedPath())
	if err != nil || len(rp.Rest) < 2 || rp.Rest[0] != "info" || rp.Rest[1] != "lfs" {
		httpx.SetRoute(r.Context(), "/{lfs}")
		lfsError(w, r, http.StatusNotFound, "Not found.")
		return
	}
	base := routePrefix(rp.ID) + ".git/info/lfs"
	rest := rp.Rest[2:]
	switch {
	case len(rest) >= 1 && rest[0] == "locks":
		httpx.SetRoute(r.Context(), base+"/locks")
		lfsError(w, r, http.StatusNotFound, "Locking is not supported")
	case len(rest) == 2 && rest[0] == "objects" && rest[1] == "batch":
		httpx.SetRoute(r.Context(), base+"/objects/batch")
		if r.Method != http.MethodPost {
			lfsMethodNotAllowed(w, r, "POST")
			return
		}
		s.batch(w, r, rp)
	case len(rest) == 2 && rest[0] == "objects":
		httpx.SetRoute(r.Context(), base+"/objects/{oid}")
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			s.download(w, r, rp, rest[1])
		case http.MethodPut:
			s.upload(w, r, rp, rest[1])
		default:
			lfsMethodNotAllowed(w, r, "GET, HEAD, PUT")
		}
	case len(rest) == 1 && rest[0] == "verify":
		httpx.SetRoute(r.Context(), base+"/verify")
		if r.Method != http.MethodPost {
			lfsMethodNotAllowed(w, r, "POST")
			return
		}
		s.verify(w, r, rp)
	case len(rest) == 2 && rest[0] == "uploads":
		httpx.SetRoute(r.Context(), base+"/uploads/{id}")
		if r.Method != http.MethodPost {
			lfsMethodNotAllowed(w, r, "POST")
			return
		}
		s.complete(w, r, rp, rest[1])
	case len(rest) == 3 && rest[0] == "uploads":
		httpx.SetRoute(r.Context(), base+"/uploads/{id}/{part}")
		if r.Method != http.MethodPut {
			lfsMethodNotAllowed(w, r, "PUT")
			return
		}
		s.putPart(w, r, rp, rest[1], rest[2])
	default:
		httpx.SetRoute(r.Context(), base+"/{path}")
		lfsError(w, r, http.StatusNotFound, "Not found.")
	}
}

func lfsMethodNotAllowed(w http.ResponseWriter, r *http.Request, allow string) {
	w.Header().Set("Allow", allow)
	lfsError(w, r, http.StatusMethodNotAllowed, "Method not allowed.")
}

// lfsError writes a git-lfs error body, `{"message": ..., "request_id": ...}`.
// A 401 carries Basic challenges so git-lfs asks for credentials.
func lfsError(w http.ResponseWriter, r *http.Request, status int, message string) {
	if status == http.StatusUnauthorized {
		w.Header().Set("LFS-Authenticate", `Basic realm="`+Realm+`"`)
		w.Header().Set("WWW-Authenticate", `Basic realm="`+Realm+`"`)
	}
	writeLFS(w, r, status, map[string]string{"message": message, "request_id": httpx.RequestID(r.Context())})
}

func writeLFS(w http.ResponseWriter, r *http.Request, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		status = http.StatusInternalServerError
		b = []byte(`{"message":"Internal server error."}`)
	}
	w.Header().Set("Content-Type", lfsMediaType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(b)
	}
}

func (s *Server) lfsFailure(w http.ResponseWriter, r *http.Request, err error) {
	var ae *accessError
	if errors.As(err, &ae) {
		lfsError(w, r, ae.status, ae.message)
		return
	}
	s.log.ErrorContext(r.Context(), "git-lfs request failed", "error", err)
	lfsError(w, r, http.StatusInternalServerError, "Internal server error.")
}

// batchRequest is a git-lfs Batch API request (docs/api/batch.md).
type batchRequest struct {
	Operation string        `json:"operation"`
	Transfers []string      `json:"transfers,omitempty"`
	Ref       *batchRef     `json:"ref,omitempty"`
	Objects   []batchObject `json:"objects"`
	HashAlgo  string        `json:"hash_algo,omitempty"`
}

type batchRef struct {
	Name string `json:"name"`
}

type batchObject struct {
	OID  string `json:"oid"`
	Size int64  `json:"size"`
}

// errBatch is a request-level batch error with its status.
type errBatch struct {
	status  int
	message string
}

func (e *errBatch) Error() string { return e.message }

// parseBatch decodes and validates the request-level parts of a batch
// request. Object-level problems are reported per object later.
func parseBatch(body []byte) (*batchRequest, error) {
	var req batchRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, &errBatch{http.StatusUnprocessableEntity, "The batch request is not valid JSON: " + err.Error()}
	}
	if req.Operation != "download" && req.Operation != "upload" {
		return nil, &errBatch{http.StatusUnprocessableEntity, "The operation must be download or upload."}
	}
	if req.HashAlgo != "" && req.HashAlgo != "sha256" {
		return nil, &errBatch{http.StatusConflict, "Only the sha256 hash algorithm is supported."}
	}
	if len(req.Objects) > maxBatchObjects {
		return nil, &errBatch{http.StatusRequestEntityTooLarge, fmt.Sprintf("A batch request can name at most %d objects.", maxBatchObjects)}
	}
	return &req, nil
}

func (req *batchRequest) offers(transfer string) bool {
	if len(req.Transfers) == 0 {
		return transfer == transferBasic
	}
	for _, t := range req.Transfers {
		if t == transfer {
			return true
		}
	}
	return false
}

// batchResponse is a git-lfs Batch API response.
type batchResponse struct {
	Transfer string           `json:"transfer"`
	Objects  []objectResponse `json:"objects"`
	HashAlgo string           `json:"hash_algo"`
}

type objectResponse struct {
	OID           string             `json:"oid"`
	Size          int64              `json:"size"`
	Authenticated bool               `json:"authenticated,omitempty"`
	Actions       map[string]*action `json:"actions,omitempty"`
	Error         *objectError       `json:"error,omitempty"`
}

type action struct {
	Href      string            `json:"href"`
	Header    map[string]string `json:"header,omitempty"`
	ExpiresIn int64             `json:"expires_in,omitempty"`
	ExpiresAt string            `json:"expires_at,omitempty"`
}

type objectError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func objErr(o batchObject, code int, message string) objectResponse {
	return objectResponse{OID: o.OID, Size: o.Size, Error: &objectError{Code: code, Message: message}}
}

func validObject(o batchObject) bool { return model.IsSHA256(o.OID) && o.Size >= 0 }

// batch answers POST .../objects/batch for git-lfs and huggingface_hub.
func (s *Server) batch(w http.ResponseWriter, r *http.Request, rp repoPath) {
	ct, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";")
	ct = strings.ToLower(strings.TrimSpace(ct))
	if ct != lfsMediaType && ct != "application/json" {
		lfsError(w, r, http.StatusUnsupportedMediaType, "Expected Content-Type "+lfsMediaType+".")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBatchBody+1))
	if err != nil {
		lfsError(w, r, http.StatusBadRequest, "The request body could not be read.")
		return
	}
	if len(body) > maxBatchBody {
		lfsError(w, r, http.StatusRequestEntityTooLarge, "The batch request is too large.")
		return
	}
	req, err := parseBatch(body)
	if err != nil {
		var be *errBatch
		errors.As(err, &be)
		lfsError(w, r, be.status, be.message)
		return
	}
	action := auth.ActionReadFiles
	if req.Operation == "upload" {
		action = auth.ActionWrite
	}
	rec, err := s.lookup(r.Context(), rp.ID, action)
	if err != nil {
		s.lfsFailure(w, r, err)
		return
	}
	var resp *batchResponse
	if req.Operation == "download" {
		resp, err = s.batchDownload(r.Context(), rec, req)
	} else {
		resp, err = s.batchUpload(r.Context(), rec, req)
	}
	if err != nil {
		s.lfsFailure(w, r, err)
		return
	}
	writeLFS(w, r, http.StatusOK, resp)
}

// lfsBase is the absolute URL of a Repo's LFS endpoint.
func (s *Server) lfsBase(rec *repo.Repo) string {
	return strings.TrimRight(s.cfg.PublicURL, "/") + "/" + rec.ID.Path() + ".git/info/lfs"
}

func (s *Server) signed(rawURL, scope string, ttl time.Duration) (*action, error) {
	href, err := s.deps.Signer.Sign(rawURL, scope, ttl)
	if err != nil {
		return nil, err
	}
	return &action{Href: href, ExpiresIn: int64(ttl.Seconds())}, nil
}

// Signed URL scopes. Each names the Repo, so a URL cannot be replayed
// against another Repo, and the object or upload it covers.
func scopeDownload(rec *repo.Repo, oid string) string {
	return "lfs-download:" + rec.UUID.String() + ":" + oid
}

func scopeUpload(rec *repo.Repo, oid string, size int64) string {
	return "lfs-upload:" + rec.UUID.String() + ":" + oid + ":" + strconv.FormatInt(size, 10)
}

func scopeVerify(rec *repo.Repo, oid string, size int64) string {
	return "lfs-verify:" + rec.UUID.String() + ":" + oid + ":" + strconv.FormatInt(size, 10)
}

func scopePart(rec *repo.Repo, id uuid.UUID) string {
	return "lfs-part:" + rec.UUID.String() + ":" + id.String()
}

func scopeComplete(rec *repo.Repo, id uuid.UUID) string {
	return "lfs-complete:" + rec.UUID.String() + ":" + id.String()
}

// notInArsenal is the message for objects a Partial mirror does not hold.
func notInArsenal(rec *repo.Repo) string {
	return "Not in arsenal. Pull this file with: hangar pull " + rec.ID.Path() + " --include <path>, or clone with GIT_LFS_SKIP_SMUDGE=1"
}

func (s *Server) batchDownload(ctx context.Context, rec *repo.Repo, req *batchRequest) (*batchResponse, error) {
	var oids []string
	for _, o := range req.Objects {
		if validObject(o) {
			oids = append(oids, o.OID)
		}
	}
	refs, err := s.deps.Repos.LFSRefs(ctx, rec.ID, oids)
	if err != nil {
		return nil, fmt.Errorf("lfs refs: %w", err)
	}
	stored, err := s.deps.LFS.StatMany(ctx, oids)
	if err != nil {
		return nil, fmt.Errorf("stat lfs objects: %w", err)
	}
	resp := &batchResponse{Transfer: transferBasic, HashAlgo: "sha256", Objects: make([]objectResponse, 0, len(req.Objects))}
	for _, o := range req.Objects {
		if !validObject(o) {
			resp.Objects = append(resp.Objects, objErr(o, http.StatusUnprocessableEntity, "The oid must be a lower-case SHA-256 and the size must not be negative."))
			continue
		}
		ref, ok := refs[o.OID]
		if !ok {
			resp.Objects = append(resp.Objects, objErr(o, http.StatusNotFound, "Object does not exist in this Repo."))
			continue
		}
		obj, have := stored[o.OID]
		if !ref.Present || !have || !obj.Verified {
			if ref.Present && have && !obj.Verified {
				resp.Objects = append(resp.Objects, objErr(o, http.StatusNotFound, "Object is still being verified. Try again shortly."))
				continue
			}
			if s.deps.PullThrough != nil {
				href, ok, err := s.deps.PullThrough.DownloadURL(ctx, httpx.Principal(ctx), rec, o.OID, o.Size)
				if err != nil {
					return nil, fmt.Errorf("pull-through: %w", err)
				}
				if ok {
					resp.Objects = append(resp.Objects, objectResponse{OID: o.OID, Size: o.Size, Authenticated: true,
						Actions: map[string]*action{"download": {Href: href}}})
					continue
				}
			}
			resp.Objects = append(resp.Objects, objErr(o, http.StatusNotFound, notInArsenal(rec)))
			continue
		}
		if obj.Size != o.Size {
			resp.Objects = append(resp.Objects, objErr(o, http.StatusUnprocessableEntity, fmt.Sprintf("The size does not match: the stored object has %d bytes.", obj.Size)))
			continue
		}
		a, err := s.downloadAction(ctx, rec, o.OID)
		if err != nil {
			return nil, err
		}
		resp.Objects = append(resp.Objects, objectResponse{OID: o.OID, Size: o.Size, Authenticated: true, Actions: map[string]*action{"download": a}})
	}
	return resp, nil
}

// downloadAction returns a presigned Blob store URL in redirect mode where
// the store supports it, and a signed Hangar URL otherwise.
func (s *Server) downloadAction(ctx context.Context, rec *repo.Repo, oid string) (*action, error) {
	ttl := s.cfg.DownloadURLTTL
	if s.cfg.DownloadMode == config.DownloadRedir {
		href, err := s.deps.LFS.PresignGet(ctx, oid, ttl, "")
		if err == nil {
			return &action{Href: href, ExpiresIn: int64(ttl.Seconds())}, nil
		}
		if !errors.Is(err, blob.ErrNotSupported) {
			return nil, fmt.Errorf("presign download: %w", err)
		}
	}
	return s.signed(s.lfsBase(rec)+"/objects/"+oid, scopeDownload(rec, oid), ttl)
}

// partSize returns the multipart part size for an object.
func (s *Server) partSize(size int64) int64 {
	if s.cfg.PartSize > 0 {
		return max(s.cfg.PartSize, (size+maxParts-1)/maxParts)
	}
	p := max(int64(minPartSize), (size+maxParts-1)/maxParts)
	const mib = 1 << 20
	return (p + mib - 1) / mib * mib
}

func (s *Server) batchUpload(ctx context.Context, rec *repo.Repo, req *batchRequest) (*batchResponse, error) {
	if rec.Mirror {
		return nil, &accessError{status: http.StatusForbidden, message: rec.ID.Path() + " is a Mirror and is read-only. Detach it from its Upstream to upload to it."}
	}
	var oids []string
	for _, o := range req.Objects {
		if validObject(o) {
			oids = append(oids, o.OID)
		}
	}
	stored, err := s.deps.LFS.StatMany(ctx, oids)
	if err != nil {
		return nil, fmt.Errorf("stat lfs objects: %w", err)
	}
	var newBytes int64
	multipart := false
	for _, o := range req.Objects {
		if obj, ok := stored[o.OID]; validObject(o) && (!ok || !obj.Verified) {
			newBytes += o.Size
			if req.offers(transferMultipart) && s.cfg.MultipartThreshold > 0 && o.Size > s.cfg.MultipartThreshold {
				multipart = true
			}
		}
	}
	if s.deps.Quota != nil && newBytes > 0 {
		if err := s.deps.Quota.CheckQuota(ctx, rec.NamespaceID, newBytes); err != nil {
			return nil, &accessError{status: http.StatusInsufficientStorage, message: err.Error()}
		}
	}
	resp := &batchResponse{Transfer: transferBasic, HashAlgo: "sha256", Objects: make([]objectResponse, 0, len(req.Objects))}
	if multipart {
		resp.Transfer = transferMultipart
	}
	for _, o := range req.Objects {
		if !validObject(o) {
			resp.Objects = append(resp.Objects, objErr(o, http.StatusUnprocessableEntity, "The oid must be a lower-case SHA-256 and the size must not be negative."))
			continue
		}
		if obj, ok := stored[o.OID]; ok && obj.Verified {
			if obj.Size != o.Size {
				resp.Objects = append(resp.Objects, objErr(o, http.StatusUnprocessableEntity, fmt.Sprintf("The size does not match: the stored object has %d bytes.", obj.Size)))
				continue
			}
			// Deduplication: the object is stored, so there is nothing to upload.
			resp.Objects = append(resp.Objects, objectResponse{OID: o.OID, Size: o.Size})
			continue
		}
		var up *action
		if multipart {
			up, err = s.multipartAction(ctx, rec, o)
		} else {
			up, err = s.basicUploadAction(ctx, rec, o)
		}
		if err != nil {
			return nil, err
		}
		verify, err := s.signed(s.lfsBase(rec)+"/verify", scopeVerify(rec, o.OID, o.Size), s.cfg.UploadURLTTL)
		if err != nil {
			return nil, err
		}
		resp.Objects = append(resp.Objects, objectResponse{OID: o.OID, Size: o.Size, Authenticated: true,
			Actions: map[string]*action{"upload": up, "verify": verify}})
	}
	return resp, nil
}

// basicUploadAction returns a presigned PUT where the Blob store supports
// one for this size, and a signed Hangar URL that streams into the store
// otherwise.
func (s *Server) basicUploadAction(ctx context.Context, rec *repo.Repo, o batchObject) (*action, error) {
	ttl := s.cfg.UploadURLTTL
	pr, err := s.deps.LFS.PresignPut(ctx, o.OID, o.Size, ttl)
	if err == nil {
		return &action{Href: pr.URL, Header: pr.Headers, ExpiresIn: int64(ttl.Seconds())}, nil
	}
	if !errors.Is(err, blob.ErrNotSupported) {
		return nil, fmt.Errorf("presign upload: %w", err)
	}
	return s.signed(s.lfsBase(rec)+"/objects/"+o.OID+"?size="+strconv.FormatInt(o.Size, 10), scopeUpload(rec, o.OID, o.Size), ttl)
}

// multipartAction starts a multipart upload for Hugging Face's multipart
// transfer: the href is the completion URL, and the header holds
// `chunk_size` and one zero-padded part number per part URL. Go encodes map
// keys in sorted order, so part URLs appear in part order, which the hf
// custom transfer agent relies on.
func (s *Server) multipartAction(ctx context.Context, rec *repo.Repo, o batchObject) (*action, error) {
	ttl := s.cfg.UploadURLTTL
	partSize := s.partSize(o.Size)
	up, err := s.deps.LFS.CreateUpload(ctx, repo.UploadParams{
		OID: o.OID, Size: o.Size, RepoUUID: rec.UUID, UserID: httpx.Principal(ctx).UserID,
		Multipart: true, PartSize: partSize, TTL: ttl,
	})
	if err != nil {
		return nil, fmt.Errorf("create upload: %w", err)
	}
	header := map[string]string{"chunk_size": strconv.FormatInt(up.PartSize, 10)}
	for n := 1; n <= up.PartCount; n++ {
		href, err := s.deps.LFS.PresignPart(ctx, up.ID, n, ttl)
		if errors.Is(err, blob.ErrNotSupported) {
			var a *action
			a, err = s.signed(fmt.Sprintf("%s/uploads/%s/%d", s.lfsBase(rec), up.ID, n), scopePart(rec, up.ID), ttl)
			if a != nil {
				href = a.Href
			}
		}
		if err != nil {
			return nil, fmt.Errorf("part URL: %w", err)
		}
		header[fmt.Sprintf("%05d", n)] = href
	}
	a, err := s.signed(fmt.Sprintf("%s/uploads/%s", s.lfsBase(rec), up.ID), scopeComplete(rec, up.ID), ttl)
	if err != nil {
		return nil, err
	}
	a.Header = header
	return a, nil
}

// signedRepo checks a signed URL's signature and scope and returns the
// Repo it was issued for. Signed URLs need no credentials.
func (s *Server) signedRepo(w http.ResponseWriter, r *http.Request, rp repoPath, scope func(*repo.Repo) string) (*repo.Repo, bool) {
	got, err := s.deps.Signer.Verify(r.URL)
	if err != nil {
		lfsError(w, r, http.StatusForbidden, "The link is invalid or has expired. Start the transfer again.")
		return nil, false
	}
	rec, err := s.deps.Repos.Get(r.Context(), rp.ID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			lfsError(w, r, http.StatusNotFound, notFoundMessage(rp.ID))
			return nil, false
		}
		s.lfsFailure(w, r, err)
		return nil, false
	}
	httpx.SetRepo(r.Context(), rec.ID)
	if got != scope(rec) {
		lfsError(w, r, http.StatusForbidden, "The link is invalid or has expired. Start the transfer again.")
		return nil, false
	}
	return rec, true
}

// download streams an object for a signed download URL, with ranges.
func (s *Server) download(w http.ResponseWriter, r *http.Request, rp repoPath, oid string) {
	if !model.IsSHA256(oid) {
		lfsError(w, r, http.StatusNotFound, "Not found.")
		return
	}
	rec, ok := s.signedRepo(w, r, rp, func(rec *repo.Repo) string { return scopeDownload(rec, oid) })
	if !ok {
		return
	}
	obj, err := s.deps.LFS.Stat(r.Context(), oid)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			lfsError(w, r, http.StatusNotFound, "Object does not exist.")
			return
		}
		s.lfsFailure(w, r, err)
		return
	}
	rng, err := blob.ParseRange(r.Header.Get("Range"), obj.Size)
	if err != nil {
		w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(obj.Size, 10))
		lfsError(w, r, http.StatusRequestedRangeNotSatisfiable, "The range is not satisfiable.")
		return
	}
	h := w.Header()
	h.Set("Accept-Ranges", "bytes")
	h.Set("ETag", `"`+oid+`"`)
	blob.SetDownloadHeaders(h, "")
	h.Set("Cache-Control", "private, max-age=0")
	length, status := obj.Size, http.StatusOK
	if rng != nil {
		length, status = rng.Length(obj.Size), http.StatusPartialContent
		h.Set("Content-Range", rng.ContentRange(obj.Size))
	}
	h.Set("Content-Length", strconv.FormatInt(length, 10))
	if r.Method == http.MethodHead {
		w.WriteHeader(status)
		return
	}
	rc, _, err := s.deps.LFS.Open(r.Context(), oid, rng)
	if err != nil {
		h.Del("Content-Length")
		h.Del("Content-Range")
		s.lfsFailure(w, r, err)
		return
	}
	defer rc.Close()
	w.WriteHeader(status)
	n, err := io.Copy(w, rc)
	if s.deps.Instruments != nil {
		s.deps.Instruments.DownloadBytes.Add(r.Context(), n, metric.WithAttributes(
			attribute.String(telemetry.MetricProtocol, string(telemetry.ProtocolLFS)),
			attribute.String(telemetry.MetricMode, config.DownloadProxy),
			attribute.String(telemetry.MetricRepoType, string(rec.ID.Type))))
		if rng == nil && err == nil {
			s.deps.Instruments.DownloadFiles.Add(r.Context(), 1, metric.WithAttributes(
				attribute.String(telemetry.MetricProtocol, string(telemetry.ProtocolLFS)),
				attribute.String(telemetry.MetricRepoType, string(rec.ID.Type))))
		}
	}
	if err != nil {
		s.log.WarnContext(r.Context(), "lfs download ended early", "error", err)
	}
}

// upload streams a basic-transfer PUT into the LFS store, which keeps the
// object only if its SHA-256 and size match.
func (s *Server) upload(w http.ResponseWriter, r *http.Request, rp repoPath, oid string) {
	if !model.IsSHA256(oid) {
		lfsError(w, r, http.StatusNotFound, "Not found.")
		return
	}
	size, err := strconv.ParseInt(r.URL.Query().Get("size"), 10, 64)
	if err != nil || size < 0 {
		lfsError(w, r, http.StatusForbidden, "The link is invalid or has expired. Start the transfer again.")
		return
	}
	if _, ok := s.signedRepo(w, r, rp, func(rec *repo.Repo) string { return scopeUpload(rec, oid, size) }); !ok {
		return
	}
	if r.ContentLength >= 0 && r.ContentLength != size {
		lfsError(w, r, http.StatusBadRequest, fmt.Sprintf("Content-Length is %d but the object has %d bytes.", r.ContentLength, size))
		return
	}
	obj, err := s.deps.LFS.Ingest(r.Context(), oid, size, r.Body)
	if err != nil {
		if errors.Is(err, repo.ErrChecksumMismatch) || errors.Is(err, repo.ErrInvalid) {
			lfsError(w, r, http.StatusUnprocessableEntity, "The uploaded content does not match its SHA-256 or size, so it was not stored.")
			return
		}
		s.lfsFailure(w, r, err)
		return
	}
	s.countUpload(r.Context(), obj.Size)
	writeLFS(w, r, http.StatusOK, map[string]any{"oid": obj.OID, "size": obj.Size})
}

func (s *Server) countUpload(ctx context.Context, n int64) {
	if s.deps.Instruments != nil {
		s.deps.Instruments.UploadBytes.Add(ctx, n, metric.WithAttributes(attribute.String(telemetry.MetricProtocol, string(telemetry.ProtocolLFS))))
	}
}

// verify answers the verify action: it checks that the object is stored
// with the declared size, re-hashing it if nothing has verified it yet.
func (s *Server) verify(w http.ResponseWriter, r *http.Request, rp repoPath) {
	var body batchObject
	if err := json.NewDecoder(io.LimitReader(r.Body, maxSmallBody)).Decode(&body); err != nil || !validObject(body) {
		lfsError(w, r, http.StatusUnprocessableEntity, "The verify request must have an oid and a size.")
		return
	}
	if _, ok := s.signedRepo(w, r, rp, func(rec *repo.Repo) string { return scopeVerify(rec, body.OID, body.Size) }); !ok {
		return
	}
	obj, err := s.deps.LFS.Stat(r.Context(), body.OID)
	if err == nil && obj.Verified && obj.Size == body.Size {
		writeLFS(w, r, http.StatusOK, map[string]any{"oid": obj.OID, "size": obj.Size})
		return
	}
	if err != nil && !errors.Is(err, repo.ErrNotFound) {
		s.lfsFailure(w, r, err)
		return
	}
	obj, err = s.deps.LFS.Verify(r.Context(), body.OID, body.Size)
	switch {
	case err == nil:
		s.countUpload(r.Context(), obj.Size)
		writeLFS(w, r, http.StatusOK, map[string]any{"oid": obj.OID, "size": obj.Size})
	case errors.Is(err, repo.ErrNotFound):
		lfsError(w, r, http.StatusNotFound, "Object does not exist. Upload it again.")
	case errors.Is(err, repo.ErrChecksumMismatch):
		lfsError(w, r, http.StatusUnprocessableEntity, "The stored object does not match its SHA-256 or size, so it was discarded. Upload it again.")
	default:
		s.lfsFailure(w, r, err)
	}
}

// putPart stores one part of a multipart upload through Hangar, for Blob
// stores that cannot presign part URLs.
func (s *Server) putPart(w http.ResponseWriter, r *http.Request, rp repoPath, rawID, rawPart string) {
	id, err := uuid.Parse(rawID)
	part, perr := strconv.Atoi(rawPart)
	if err != nil || perr != nil || part < 1 || part > maxParts {
		lfsError(w, r, http.StatusNotFound, "Not found.")
		return
	}
	if _, ok := s.signedRepo(w, r, rp, func(rec *repo.Repo) string { return scopePart(rec, id) }); !ok {
		return
	}
	if r.ContentLength < 0 {
		lfsError(w, r, http.StatusLengthRequired, "Content-Length is required.")
		return
	}
	etag, err := s.deps.LFS.PutPart(r.Context(), id, part, r.Body, r.ContentLength)
	if err != nil {
		switch {
		case errors.Is(err, repo.ErrNotFound), errors.Is(err, blob.ErrNotFound):
			lfsError(w, r, http.StatusNotFound, "The upload does not exist or has expired.")
		case errors.Is(err, repo.ErrInvalid), errors.Is(err, blob.ErrSizeMismatch):
			lfsError(w, r, http.StatusBadRequest, "The part was refused: "+err.Error())
		default:
			s.lfsFailure(w, r, err)
		}
		return
	}
	s.countUpload(r.Context(), r.ContentLength)
	w.Header().Set("ETag", etag)
	w.WriteHeader(http.StatusOK)
}

// completion is the body of the multipart completion POST.
type completion struct {
	OID   string `json:"oid"`
	Parts []struct {
		PartNumber int    `json:"partNumber"`
		ETag       string `json:"etag"`
	} `json:"parts"`
}

// complete assembles a multipart upload. The object becomes visible only if
// its SHA-256 and size match.
func (s *Server) complete(w http.ResponseWriter, r *http.Request, rp repoPath, rawID string) {
	id, err := uuid.Parse(rawID)
	if err != nil {
		lfsError(w, r, http.StatusNotFound, "Not found.")
		return
	}
	if _, ok := s.signedRepo(w, r, rp, func(rec *repo.Repo) string { return scopeComplete(rec, id) }); !ok {
		return
	}
	var body completion
	if err := json.NewDecoder(io.LimitReader(r.Body, maxSmallBody)).Decode(&body); err != nil || len(body.Parts) == 0 {
		lfsError(w, r, http.StatusUnprocessableEntity, "The completion request must list the uploaded parts.")
		return
	}
	up, err := s.deps.LFS.GetUpload(r.Context(), id)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			lfsError(w, r, http.StatusNotFound, "The upload does not exist or has expired.")
			return
		}
		s.lfsFailure(w, r, err)
		return
	}
	if body.OID != up.OID {
		lfsError(w, r, http.StatusUnprocessableEntity, "The oid does not match the upload.")
		return
	}
	parts := make([]blob.Part, 0, len(body.Parts))
	for _, p := range body.Parts {
		parts = append(parts, blob.Part{Number: p.PartNumber, ETag: p.ETag})
	}
	obj, err := s.deps.LFS.CompleteUpload(r.Context(), id, parts)
	if err != nil {
		switch {
		case errors.Is(err, repo.ErrChecksumMismatch):
			lfsError(w, r, http.StatusUnprocessableEntity, "The uploaded content does not match its SHA-256 or size, so it was not stored.")
		case errors.Is(err, repo.ErrNotFound), errors.Is(err, blob.ErrNotFound):
			lfsError(w, r, http.StatusNotFound, "The upload does not exist or has expired.")
		case errors.Is(err, repo.ErrInvalid):
			lfsError(w, r, http.StatusUnprocessableEntity, "The parts were refused: "+err.Error())
		default:
			s.lfsFailure(w, r, err)
		}
		return
	}
	writeLFS(w, r, http.StatusOK, map[string]any{"oid": obj.OID, "size": obj.Size})
}
