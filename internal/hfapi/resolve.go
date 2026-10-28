package hfapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/blob"
	"github.com/hangar-hub/hangar/internal/httpx"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/scan"
	"github.com/hangar-hub/hangar/internal/telemetry"
)

// Download headers (contract §1.2).
const (
	headerLinkedEtag      = "X-Linked-Etag"
	headerLinkedSize      = "X-Linked-Size"
	headerDownloadCounter = "X-HF-Download-Counter"
)

// maxPlainDownload is huggingface_hub's MAX_HTTP_DOWNLOAD_SIZE: it refuses
// plain HTTP downloads above 50 GB when it knows the size, so redirects for
// larger files leave out X-Linked-Size (03-hf-api.md rule 14).
const maxPlainDownload = 50 * 1000 * 1000 * 1000

// serveResolve routes /[datasets/]{ns}/{name}/resolve/{rev}/{path}.
func (h *Handler) serveResolve(w http.ResponseWriter, r *http.Request, p reqPath) {
	t := model.RepoTypeModel
	var before []string
	if p.at(0) == "datasets" {
		t, before, p = model.RepoTypeDataset, []string{"datasets"}, p.shift(1)
	}
	template := "/" + t.URLPrefix() + "{ns}/{name}/resolve/{rev}/{path}"
	switch {
	case p.len() >= 3 && p.at(2) == "resolve":
	case p.len() >= 2 && p.at(1) == "resolve" && h.d.Settings.DefaultNamespace != "":
		httpx.SetRoute(r.Context(), "/"+t.URLPrefix()+"{name}/resolve/{rev}/{path}")
		h.redirectSingleSegment(w, r, before, p.esc)
		return
	default:
		h.notFound(w, r)
		return
	}
	ref := repoRef{id: model.RepoID{Type: t, Namespace: p.at(0), Name: p.at(1)}, before: before, after: p.esc[2:]}
	rev, tail := takeRevision(p.shift(3))
	filePath := tail.joined(0)
	h.route(w, r, template, rev != "", methods{http.MethodGet: func(w http.ResponseWriter, r *http.Request) { h.resolve(w, r, ref, rev, filePath) }})
}

// resolve serves HEAD and GET of one file at a Revision (contract §1.2 to
// §1.6): metadata headers from stored values, ranges, and either the bytes
// or a 302 to a presigned Blob store URL.
func (h *Handler) resolve(w http.ResponseWriter, r *http.Request, ref repoRef, rev, filePath string) {
	ctx := r.Context()
	rp, ok := h.load(w, r, ref, auth.ActionReadFiles)
	if !ok {
		return
	}
	sha, ok := h.resolveRevision(w, r, rp, rev)
	if !ok {
		return
	}
	clean, err := model.CleanRepoPath(filePath)
	if err != nil || clean == "" {
		httpx.HubEntryNotFound(w, r, filePath, rev, sha)
		return
	}
	e, err := h.d.Repos.Entry(ctx, rp.ID, sha, clean)
	if err != nil {
		if errors.Is(err, repo.ErrEntryNotFound) {
			httpx.HubEntryNotFound(w, r, filePath, rev, sha)
			return
		}
		h.serviceError(w, r, rp.ID, err)
		return
	}
	if e.Type != model.EntryFile {
		httpx.HubEntryNotFound(w, r, filePath, rev, sha)
		return
	}
	if r.Method == http.MethodHead && r.Header.Get(headerDownloadCounter) == "1" {
		h.countDownload(ctx, rp, sha, clean)
	}

	hdr := w.Header()
	etag := `"` + e.ETag() + `"`
	hdr.Set(httpx.HeaderRepoCommit, string(sha))
	hdr.Set("ETag", etag)
	hdr.Set(headerLinkedEtag, etag)
	hdr.Set("Accept-Ranges", "bytes")
	// Every download is an attachment of type application/octet-stream
	// with nosniff, whatever the file name says, so an uploaded HTML or
	// SVG file is never rendered on Hangar's origin.
	blob.SetDownloadHeaders(hdr, path.Base(clean))
	hdr.Set("Content-Disposition", contentDisposition(path.Base(clean)))

	// Only a GET is redirected. A presigned URL is signed for GET, so a
	// client that follows a redirected HEAD with another HEAD, as llama.cpp
	// does, would get 403 from the bucket. huggingface_hub downloads from
	// the URL it sent the HEAD to when there is no Location, and its GET
	// then gets the redirect.
	redirect := e.IsLFS() && h.d.Settings.RedirectDownloads
	if r.Method == http.MethodGet && !h.allowDownload(w, r, rp, sha, clean) {
		return
	}
	if e.IsLFS() && !e.Present {
		h.serveNotPresent(w, r, rp, sha, e)
		return
	}
	if r.Method == http.MethodHead {
		// Rule 14: the GET's redirect leaves out X-Linked-Size over 50 GB,
		// so the HEAD leaves out the size as well.
		if !redirect || e.Size <= maxPlainDownload {
			hdr.Set(headerLinkedSize, strconv.FormatInt(e.Size, 10))
			hdr.Set("Content-Length", strconv.FormatInt(e.Size, 10))
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	if redirect {
		u, err := h.d.LFS.PresignGet(ctx, e.LFS.OID, h.d.Settings.PresignTTL, path.Base(clean))
		switch {
		case err == nil:
			if e.Size <= maxPlainDownload {
				hdr.Set(headerLinkedSize, strconv.FormatInt(e.Size, 10))
			}
			hdr.Set("Location", u)
			hdr.Set("Content-Length", "0")
			w.WriteHeader(http.StatusFound)
			h.recordDownload(ctx, rp.ID.Type, "redirect", e.Size, true)
			return
		case !errors.Is(err, blob.ErrNotSupported):
			h.internalError(w, r, err)
			return
		}
	}
	hdr.Set(headerLinkedSize, strconv.FormatInt(e.Size, 10))
	rng, status, ok := parseRange(r.Header.Get("Range"), e.Size)
	if !ok {
		hdr.Set("Content-Range", "bytes */"+strconv.FormatInt(e.Size, 10))
		httpx.WriteHubError(w, r, &httpx.HubError{Status: http.StatusRequestedRangeNotSatisfiable, Message: "The requested range is not satisfiable."})
		return
	}
	rc, _, err := h.d.Repos.OpenFile(ctx, rp.ID, sha, clean, rng)
	if err != nil {
		if errors.Is(err, repo.ErrNotPresent) {
			h.serveNotPresent(w, r, rp, sha, e)
			return
		}
		if errors.Is(err, blob.ErrInvalidRange) {
			hdr.Set("Content-Range", "bytes */"+strconv.FormatInt(e.Size, 10))
			httpx.WriteHubError(w, r, &httpx.HubError{Status: http.StatusRequestedRangeNotSatisfiable, Message: "The requested range is not satisfiable."})
			return
		}
		h.serviceError(w, r, rp.ID, err)
		return
	}
	defer func() { _ = rc.Close() }()
	length := e.Size
	if rng != nil {
		length = rng.End - rng.Start + 1
		hdr.Set("Content-Range", "bytes "+strconv.FormatInt(rng.Start, 10)+"-"+strconv.FormatInt(rng.End, 10)+"/"+strconv.FormatInt(e.Size, 10))
	}
	hdr.Set("Content-Length", strconv.FormatInt(length, 10))
	w.WriteHeader(status)
	n, err := io.Copy(w, io.LimitReader(rc, length))
	h.recordDownload(ctx, rp.ID.Type, "proxy", n, rng == nil || rng.Start == 0)
	if err != nil && !errors.Is(err, context.Canceled) {
		h.log.WarnContext(ctx, "download interrupted", "error", err, "bytes", n, "size", length)
	}
}

// allowDownload enforces scan.block_unsafe_downloads through the shared
// gate: with the setting on, a file the scan marked unsafe, or a file that
// can run code and is not scanned yet, downloads only with accept_risk=1,
// and otherwise gets 403 with a message that says why and how to go ahead.
func (h *Handler) allowDownload(w http.ResponseWriter, r *http.Request, rp *repo.Repo, sha model.CommitSHA, filePath string) bool {
	if httpx.QueryBool(r.URL.Query(), "accept_risk", false) {
		return true
	}
	err := h.d.Downloads.CheckFile(r.Context(), rp.UUID, rp.ID, sha, filePath)
	if scan.IsBlocked(err) {
		httpx.WriteHubError(w, r, &httpx.HubError{Status: http.StatusForbidden,
			Message: err.Error() + " To download it anyway, add accept_risk=1 to the URL."})
		return false
	}
	if err != nil {
		h.internalError(w, r, err)
		return false
	}
	return true
}

// serveNotPresent answers a request for a file of a Partial mirror whose
// LFS object is not in the Arsenal: through Pull-through when it is on,
// and otherwise with NotPresentMessage. Metadata headers are already set.
//
// HEAD answers 200, as for a present file: an EntryNotFound there would
// make huggingface_hub cache the file as missing for this Commit, and keep
// it missing after the file is pulled.
func (h *Handler) serveNotPresent(w http.ResponseWriter, r *http.Request, rp *repo.Repo, sha model.CommitSHA, e model.Entry) {
	ctx := r.Context()
	hdr := w.Header()
	hdr.Set(headerLinkedSize, strconv.FormatInt(e.Size, 10))
	if r.Method == http.MethodHead {
		hdr.Set("Content-Length", strconv.FormatInt(e.Size, 10))
		w.WriteHeader(http.StatusOK)
		return
	}
	if h.d.PullThrough != nil {
		rng, status, ok := parseRange(r.Header.Get("Range"), e.Size)
		if !ok {
			hdr.Set("Content-Range", "bytes */"+strconv.FormatInt(e.Size, 10))
			httpx.WriteHubError(w, r, &httpx.HubError{Status: http.StatusRequestedRangeNotSatisfiable, Message: "The requested range is not satisfiable."})
			return
		}
		rc, err := h.d.PullThrough.File(ctx, httpx.Principal(ctx), rp, sha, e)
		if err == nil {
			defer func() { _ = rc.Close() }()
			h.streamPulled(w, r, rp, rc, e.Size, rng, status)
			return
		}
		if !errors.Is(err, repo.ErrNotPresent) {
			h.serviceError(w, r, rp.ID, err)
			return
		}
	}
	httpx.WriteHubError(w, r, &httpx.HubError{
		Status:     http.StatusConflict,
		Code:       httpx.CodeEntryNotFound,
		Message:    NotPresentMessage(rp.ID, e.Path),
		RepoCommit: sha,
	})
}

// streamPulled writes a file that Pull-through streams from its start as
// it arrives. A ranged request waits for the bytes before the range and
// skips them, so it still gets a real 206 (resumed downloads, safetensors
// header reads).
func (h *Handler) streamPulled(w http.ResponseWriter, r *http.Request, rp *repo.Repo, rc io.Reader, size int64, rng *blob.Range, status int) {
	ctx := r.Context()
	length, start := size, int64(0)
	if rng != nil {
		start, length = rng.Start, rng.End-rng.Start+1
		if _, err := io.CopyN(io.Discard, rc, start); err != nil {
			h.serviceError(w, r, rp.ID, fmt.Errorf("pull-through: read up to byte %d: %w", start, err))
			return
		}
		w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(rng.Start, 10)+"-"+strconv.FormatInt(rng.End, 10)+"/"+strconv.FormatInt(size, 10))
	}
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.WriteHeader(status)
	n, err := io.Copy(w, io.LimitReader(rc, length))
	h.recordDownload(ctx, rp.ID.Type, "proxy", n, start == 0)
	if err != nil && !errors.Is(err, context.Canceled) {
		h.log.WarnContext(ctx, "pull-through download interrupted", "error", err, "bytes", n)
	}
}

// NotPresentMessage is the X-Error-Message of a download of a file that a
// Partial mirror lists but does not hold. huggingface_hub raises it as
// RemoteEntryNotFoundError (X-Error-Code EntryNotFound) with this text.
func NotPresentMessage(id model.RepoID, path string) string {
	return path + " is not in the Arsenal: " + id.String() + " is a Partial mirror, which lists every file but holds only the files that were pulled. " +
		"To download only the files it holds, pass allow_patterns to snapshot_download (--include with hf download). " +
		"To get this file, pull it from the Repo page or run: hangar pull " + id.Path() + " --include '" + path + "'"
}

func (h *Handler) recordDownload(ctx context.Context, t model.RepoType, mode string, n int64, newFile bool) {
	in := h.d.Instruments
	if in == nil {
		return
	}
	proto := attribute.String(telemetry.MetricProtocol, string(telemetry.ProtocolHub))
	rt := attribute.String(telemetry.MetricRepoType, string(t))
	if in.DownloadBytes != nil && n > 0 {
		in.DownloadBytes.Add(ctx, n, metric.WithAttributes(proto, rt, attribute.String(telemetry.MetricMode, mode)))
	}
	if in.DownloadFiles != nil && newFile {
		in.DownloadFiles.Add(ctx, 1, metric.WithAttributes(proto, rt))
	}
}

// parseRange reads a single-range Range header against a file of size.
// It returns the range (nil for the whole file), the status to answer, and
// ok false when the range cannot be satisfied (416). Malformed and
// multi-range headers are ignored, as RFC 9110 allows, and the whole file
// is served.
func parseRange(header string, size int64) (*blob.Range, int, bool) {
	spec, found := strings.CutPrefix(strings.TrimSpace(header), "bytes=")
	if header == "" || !found || strings.Contains(spec, ",") {
		return nil, http.StatusOK, true
	}
	first, last, found := strings.Cut(strings.TrimSpace(spec), "-")
	if !found {
		return nil, http.StatusOK, true
	}
	var start, end int64
	if first == "" {
		n, err := strconv.ParseInt(last, 10, 64)
		if err != nil || n < 0 {
			return nil, http.StatusOK, true
		}
		if n == 0 || size == 0 {
			return nil, 0, false
		}
		start, end = max(size-n, 0), size-1
	} else {
		s, err := strconv.ParseInt(first, 10, 64)
		if err != nil || s < 0 {
			return nil, http.StatusOK, true
		}
		start, end = s, size-1
		if last != "" {
			e, err := strconv.ParseInt(last, 10, 64)
			if err != nil || e < s {
				return nil, http.StatusOK, true
			}
			end = min(e, size-1)
		}
		if start >= size {
			return nil, 0, false
		}
	}
	return &blob.Range{Start: start, End: end}, http.StatusPartialContent, true
}

// contentDisposition names the file the way the Hub does.
func contentDisposition(name string) string {
	ascii := strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	return `attachment; filename*=UTF-8''` + url.PathEscape(name) + `; filename="` + ascii + `";`
}

// dataExtensions are the file types counted as a dataset's data files.
var dataExtensions = map[string]bool{
	".parquet": true, ".csv": true, ".tsv": true, ".json": true, ".jsonl": true, ".ndjson": true,
	".txt": true, ".arrow": true, ".zip": true, ".tar": true, ".gz": true, ".zst": true,
	".xml": true, ".h5": true, ".hdf5": true, ".lance": true, ".avro": true,
}

// countDownload records a download when the file is the Repo's counted
// file (02-architecture.md): `config.json` for models, the first data file
// of the Commit for datasets. Only HEAD requests with the
// X-HF-Download-Counter header count: huggingface_hub makes exactly one per
// hf_hub_download, whether or not the file is cached, so a download counts
// once and never twice.
func (h *Handler) countDownload(ctx context.Context, rp *repo.Repo, sha model.CommitSHA, p string) {
	counted := false
	switch rp.ID.Type {
	case model.RepoTypeModel:
		counted = p == "config.json"
	case model.RepoTypeDataset:
		if isDataFile(p) {
			first, err := h.firstDataFile(ctx, rp, sha)
			if err != nil {
				h.log.WarnContext(ctx, "could not list data files for download counting", "error", err)
				return
			}
			counted = first == p
		}
	}
	if !counted {
		return
	}
	if err := h.d.Community.RecordDownload(ctx, rp.UUID, h.now().UTC()); err != nil {
		h.log.WarnContext(ctx, "could not record a download", "error", err)
	}
}

// dataFileCache caches the first data file of each dataset Commit, keyed
// by Repo and Commit. Commits are immutable, so entries never go stale.
type dataFileCache struct {
	mu sync.Mutex
	m  map[string]string
}

const dataFileCacheMax = 4096

func (h *Handler) firstDataFile(ctx context.Context, rp *repo.Repo, sha model.CommitSHA) (string, error) {
	key := rp.UUID.String() + ":" + string(sha)
	c := &h.dataFiles
	c.mu.Lock()
	p, ok := c.m[key]
	c.mu.Unlock()
	if ok {
		return p, nil
	}
	entries, err := h.d.Repos.Tree(ctx, rp.ID, sha, "", repo.TreeOpts{Recursive: true})
	if err != nil {
		return "", err
	}
	var files []string
	for _, e := range entries {
		if e.Type == model.EntryFile && isDataFile(e.Path) {
			files = append(files, e.Path)
		}
	}
	sort.Strings(files)
	first := ""
	if len(files) > 0 {
		first = files[0]
	}
	c.mu.Lock()
	if c.m == nil || len(c.m) >= dataFileCacheMax {
		c.m = map[string]string{}
	}
	c.m[key] = first
	c.mu.Unlock()
	return first, nil
}

func isDataFile(p string) bool {
	return dataExtensions[strings.ToLower(path.Ext(p))] && path.Base(p) != "dataset_infos.json"
}
