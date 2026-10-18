package hfapitest

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
)

// standInPrefix is where the stand-in serves upload URLs.
const standInPrefix = "/lfs-standin/"

// maxParts is the most parts one multipart upload may have, as on S3.
const maxParts = 10000

// lfsStandIn answers git-lfs batch upload requests and stores into
// repo.LFSStore. It stands in for internal/gitserver, which is W3's, so
// huggingface_hub's non-Xet upload path works in tests. Every upload URL
// carries a one-time secret, because clients send no Authorization header
// on them (contract §7.6).
//
// Objects over multipartThreshold, when the client offers the `multipart`
// transfer, get Hugging Face's multipart action: `chunk_size` and numbered
// part URLs in the header, and the completion URL as the href. The parts go
// through repo.LFSStore's multipart upload. Other objects get a single PUT
// URL. Downloads go through resolve. Like internal/gitserver, it asks for
// the bytes of a stored object the caller may not reference
// (repo.Service.Referable), and records the uploader once they match.
type lfsStandIn struct {
	lfs   repo.LFSStore
	repos repo.Service
	base  string
	// multipartThreshold is the size above which uploads are multipart;
	// zero turns multipart off. partSize is the part size.
	multipartThreshold int64
	partSize           int64

	mu      sync.Mutex
	pending map[string]pendingUpload
}

type pendingUpload struct {
	oid  string
	size int64
	// userID is the uploader the batch answered.
	userID uuid.UUID
	// multipart is set for a multipart upload.
	multipart *multipartUpload
}

// multipartUpload is a multipart upload in progress.
type multipartUpload struct {
	id       uuid.UUID
	partSize int64
	parts    int
	// etags holds the ETag each part PUT returned, by part number.
	etags map[int]string
}

// sizeOf returns the size of part n (1-based) of an object of total bytes.
func (m *multipartUpload) sizeOf(n int, total int64) int64 {
	if n < m.parts {
		return m.partSize
	}
	return total - int64(m.parts-1)*m.partSize
}

func lfsJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/vnd.git-lfs+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ServeHTTP serves POST {repo}.git/info/lfs/objects/batch.
func (l *lfsStandIn) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/info/lfs/objects/batch") {
		lfsJSON(w, http.StatusNotFound, map[string]string{"message": "Not found."})
		return
	}
	if httpx.Principal(r.Context()).IsAnonymous() {
		w.Header().Set("WWW-Authenticate", `Basic realm="Hangar"`)
		lfsJSON(w, http.StatusUnauthorized, map[string]string{"message": "Authentication required."})
		return
	}
	var req struct {
		Operation string   `json:"operation"`
		Transfers []string `json:"transfers"`
		Objects   []struct {
			OID  string `json:"oid"`
			Size int64  `json:"size"`
		} `json:"objects"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		lfsJSON(w, http.StatusBadRequest, map[string]string{"message": "Invalid batch request."})
		return
	}
	p := httpx.Principal(r.Context())
	oids := make([]string, 0, len(req.Objects))
	for _, o := range req.Objects {
		oids = append(oids, o.OID)
	}
	var target *repo.Repo
	if id, ok := batchRepo(r.URL.Path); ok {
		if rp, err := l.repos.Get(r.Context(), id); err == nil {
			target = rp
		}
	}
	referable, err := l.repos.Referable(r.Context(), p, target, oids)
	if err != nil {
		lfsJSON(w, http.StatusInternalServerError, map[string]string{"message": "Could not check the objects."})
		return
	}
	objects := make([]map[string]any, 0, len(req.Objects))
	for _, o := range req.Objects {
		obj := map[string]any{"oid": o.OID, "size": o.Size}
		if !model.IsSHA256(o.OID) || o.Size < 0 {
			obj["error"] = map[string]any{"code": 422, "message": "Invalid oid or size."}
			objects = append(objects, obj)
			continue
		}
		if _, err := l.lfs.Stat(r.Context(), o.OID); err == nil && (req.Operation != "upload" || referable[o.OID]) {
			objects = append(objects, obj)
			continue
		}
		if req.Operation != "upload" {
			obj["error"] = map[string]any{"code": 404, "message": "Object does not exist."}
			objects = append(objects, obj)
			continue
		}
		up := pendingUpload{oid: o.OID, size: o.Size, userID: p.UserID}
		if l.multipartThreshold > 0 && o.Size > l.multipartThreshold && slices.Contains(req.Transfers, "multipart") {
			partSize := max(l.partSize, (o.Size+maxParts-1)/maxParts)
			u, err := l.lfs.CreateUpload(r.Context(), repo.UploadParams{OID: o.OID, Size: o.Size, Multipart: true, PartSize: partSize,
				UserID: p.UserID})
			if err != nil {
				obj["error"] = map[string]any{"code": 500, "message": "Could not start the upload."}
				objects = append(objects, obj)
				continue
			}
			up.multipart = &multipartUpload{id: u.ID, partSize: u.PartSize, parts: u.PartCount, etags: map[int]string{}}
		}
		secret := l.register(up)
		upload := map[string]any{"href": l.base + standInPrefix + secret}
		if m := up.multipart; m != nil {
			// The href is the completion URL; the parts are numbered
			// header entries (contract §7.6).
			upload["href"] = l.base + standInPrefix + secret + "/complete"
			header := map[string]any{"chunk_size": strconv.FormatInt(m.partSize, 10)}
			for n := 1; n <= m.parts; n++ {
				header[fmt.Sprintf("%05d", n)] = l.base + standInPrefix + secret + "/part/" + strconv.Itoa(n)
			}
			upload["header"] = header
		}
		obj["actions"] = map[string]any{"upload": upload}
		objects = append(objects, obj)
	}
	lfsJSON(w, http.StatusOK, map[string]any{"transfer": "basic", "objects": objects})
}

// batchRepo returns the Repo a batch URL names: /{ns}/{name}.git/... or
// /datasets/{ns}/{name}.git/....
func batchRepo(path string) (model.RepoID, bool) {
	name, ok := strings.CutSuffix(strings.TrimPrefix(path, "/"), ".git/info/lfs/objects/batch")
	if !ok {
		return model.RepoID{}, false
	}
	typ := model.RepoTypeModel
	if rest, ok := strings.CutPrefix(name, "datasets/"); ok {
		typ, name = model.RepoTypeDataset, rest
	}
	ns, n, ok := strings.Cut(name, "/")
	if !ok {
		return model.RepoID{}, false
	}
	return model.RepoID{Type: typ, Namespace: ns, Name: n}, true
}

// register records an upload under a new one-time secret.
func (l *lfsStandIn) register(up pendingUpload) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	secret := hex.EncodeToString(b)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pending == nil {
		l.pending = map[string]pendingUpload{}
	}
	l.pending[secret] = up
	return secret
}

// uploads serves the upload URLs, which clients call without credentials:
// PUT {secret} for a single-part upload, PUT {secret}/part/{n} for a part,
// and POST {secret}/complete to finish a multipart upload.
func (l *lfsStandIn) uploads(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest, ok := strings.CutPrefix(r.URL.Path, standInPrefix)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		secret, action, _ := strings.Cut(rest, "/")
		switch {
		case action == "" && r.Method == http.MethodPut:
			l.putSingle(w, r, secret)
		case strings.HasPrefix(action, "part/") && r.Method == http.MethodPut:
			l.putPart(w, r, secret, strings.TrimPrefix(action, "part/"))
		case action == "complete" && r.Method == http.MethodPost:
			l.complete(w, r, secret)
		default:
			http.Error(w, "unknown upload", http.StatusNotFound)
		}
	})
}

// take removes and returns a pending upload of the given kind.
func (l *lfsStandIn) take(secret string, multipart bool) (pendingUpload, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	up, found := l.pending[secret]
	if !found || (up.multipart != nil) != multipart {
		return pendingUpload{}, false
	}
	delete(l.pending, secret)
	return up, true
}

func (l *lfsStandIn) putSingle(w http.ResponseWriter, r *http.Request, secret string) {
	up, found := l.take(secret, false)
	if !found {
		http.Error(w, "unknown upload", http.StatusNotFound)
		return
	}
	if _, err := l.lfs.Ingest(r.Context(), up.oid, up.size, r.Body); err != nil {
		writeIngestError(w, err)
		return
	}
	if err := l.lfs.RecordUploader(r.Context(), up.oid, up.userID); err != nil {
		writeIngestError(w, err)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(0))
	w.WriteHeader(http.StatusOK)
}

// putPart stores one part. A part may be sent again, as clients retry
// failed parts, until the upload completes.
func (l *lfsStandIn) putPart(w http.ResponseWriter, r *http.Request, secret, num string) {
	l.mu.Lock()
	up, found := l.pending[secret]
	l.mu.Unlock()
	n, err := strconv.Atoi(num)
	if !found || up.multipart == nil || err != nil || n < 1 || n > up.multipart.parts {
		http.Error(w, "unknown upload part", http.StatusNotFound)
		return
	}
	m := up.multipart
	want := m.sizeOf(n, up.size)
	// Parts are at most one part size, so reading one into memory is bounded.
	data, err := io.ReadAll(io.LimitReader(r.Body, want+1))
	if err != nil {
		http.Error(w, "could not read the part", http.StatusBadRequest)
		return
	}
	if int64(len(data)) != want {
		http.Error(w, fmt.Sprintf("part %d must be %d bytes, got %d", n, want, len(data)), http.StatusBadRequest)
		return
	}
	etag, err := l.lfs.PutPart(r.Context(), m.id, n, bytes.NewReader(data), want)
	if err != nil {
		http.Error(w, "could not store the part", http.StatusInternalServerError)
		return
	}
	l.mu.Lock()
	m.etags[n] = etag
	l.mu.Unlock()
	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Length", strconv.Itoa(0))
	w.WriteHeader(http.StatusOK)
}

// complete assembles the parts in the order the client lists them, checks
// every ETag, and stores the object only if its SHA-256 and size match.
func (l *lfsStandIn) complete(w http.ResponseWriter, r *http.Request, secret string) {
	up, found := l.take(secret, true)
	if !found {
		lfsJSON(w, http.StatusNotFound, map[string]string{"message": "Unknown upload."})
		return
	}
	m := up.multipart
	abort := func(status int, msg string) {
		_ = l.lfs.AbortUpload(r.Context(), m.id)
		lfsJSON(w, status, map[string]string{"message": msg})
	}
	var req struct {
		OID   string `json:"oid"`
		Parts []struct {
			PartNumber int    `json:"partNumber"`
			ETag       string `json:"etag"`
		} `json:"parts"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		abort(http.StatusBadRequest, "Invalid completion request.")
		return
	}
	if req.OID != up.oid {
		abort(http.StatusBadRequest, "The completion is for a different object.")
		return
	}
	if len(req.Parts) != m.parts {
		abort(http.StatusBadRequest, fmt.Sprintf("Expected %d parts, got %d.", m.parts, len(req.Parts)))
		return
	}
	l.mu.Lock()
	etags := maps.Clone(m.etags)
	l.mu.Unlock()
	parts := make([]blob.Part, 0, len(req.Parts))
	for i, p := range req.Parts {
		if p.PartNumber != i+1 {
			abort(http.StatusBadRequest, "Parts must be listed in order, starting at 1.")
			return
		}
		if stored, ok := etags[p.PartNumber]; !ok || stored != p.ETag {
			abort(http.StatusBadRequest, fmt.Sprintf("Part %d is missing or its ETag does not match.", p.PartNumber))
			return
		}
		parts = append(parts, blob.Part{Number: p.PartNumber, ETag: p.ETag, Size: m.sizeOf(p.PartNumber, up.size)})
	}
	if _, err := l.lfs.CompleteUpload(r.Context(), m.id, parts); err != nil {
		writeIngestError(w, err)
		return
	}
	lfsJSON(w, http.StatusOK, map[string]any{"oid": up.oid, "size": up.size})
}

// writeIngestError answers a failed store: 400 when the bytes do not match
// the declared SHA-256 or size.
func writeIngestError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	if errors.Is(err, repo.ErrChecksumMismatch) {
		status = http.StatusBadRequest
	}
	http.Error(w, err.Error(), status)
}
