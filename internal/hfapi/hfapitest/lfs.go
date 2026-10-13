package hfapitest

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
)

// standInPrefix is where the stand-in serves upload URLs.
const standInPrefix = "/lfs-standin/"

// lfsStandIn answers git-lfs batch upload requests with single-PUT URLs
// that carry a one-time secret and store into repo.LFSStore. It stands in
// for internal/gitserver, which is W3's, so huggingface_hub's non-Xet
// upload path works in tests. Downloads go through resolve.
type lfsStandIn struct {
	lfs   repo.LFSStore
	repos repo.Service
	base  string

	mu      sync.Mutex
	pending map[string]pendingUpload
}

type pendingUpload struct {
	oid  string
	size int64
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
		Operation string `json:"operation"`
		Objects   []struct {
			OID  string `json:"oid"`
			Size int64  `json:"size"`
		} `json:"objects"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		lfsJSON(w, http.StatusBadRequest, map[string]string{"message": "Invalid batch request."})
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
		if _, err := l.lfs.Stat(r.Context(), o.OID); err == nil {
			objects = append(objects, obj)
			continue
		}
		if req.Operation != "upload" {
			obj["error"] = map[string]any{"code": 404, "message": "Object does not exist."}
			objects = append(objects, obj)
			continue
		}
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		secret := hex.EncodeToString(b)
		l.mu.Lock()
		if l.pending == nil {
			l.pending = map[string]pendingUpload{}
		}
		l.pending[secret] = pendingUpload{oid: o.OID, size: o.Size}
		l.mu.Unlock()
		obj["actions"] = map[string]any{"upload": map[string]any{"href": l.base + standInPrefix + secret}}
		objects = append(objects, obj)
	}
	lfsJSON(w, http.StatusOK, map[string]any{"transfer": "basic", "objects": objects})
}

// uploads serves the upload URLs, which clients call without credentials.
func (l *lfsStandIn) uploads(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secret, ok := strings.CutPrefix(r.URL.Path, standInPrefix)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		l.mu.Lock()
		up, found := l.pending[secret]
		delete(l.pending, secret)
		l.mu.Unlock()
		if r.Method != http.MethodPut || !found {
			http.Error(w, "unknown upload", http.StatusNotFound)
			return
		}
		if _, err := l.lfs.Ingest(r.Context(), up.oid, up.size, r.Body); err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, repo.ErrChecksumMismatch) {
				status = http.StatusBadRequest
			}
			http.Error(w, err.Error(), status)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(0))
		w.WriteHeader(http.StatusOK)
	})
}
