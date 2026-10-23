// Package ollama serves GGUF Repos as an Ollama registry (DOWN-4,
// docs/spec/08-local-tools.md), so `ollama pull <host>/<ns>/<name>:<quant>`
// works with no Modelfile. Manifests are built on the fly: the model layer
// is the GGUF file's LFS object, so no copy is made, and the small layers
// (config, template, parameters, licence) are stored in ollama_blobs.
// Blobs are always streamed through Hangar.
package ollama

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/blob"
	"github.com/hangar-hub/hangar/internal/community"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/httpx"
	"github.com/hangar-hub/hangar/internal/index"
	"github.com/hangar-hub/hangar/internal/index/gguf"
	"github.com/hangar-hub/hangar/internal/index/rangeio"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/scan"
	"github.com/hangar-hub/hangar/internal/telemetry"
)

// Media types of the manifest and its layers.
const (
	MediaTypeManifest  = "application/vnd.docker.distribution.manifest.v2+json"
	MediaTypeConfig    = "application/vnd.docker.container.image.v1+json"
	MediaTypeModel     = "application/vnd.ollama.image.model"
	MediaTypeProjector = "application/vnd.ollama.image.projector"
	MediaTypeTemplate  = "application/vnd.ollama.image.template"
	MediaTypeSystem    = "application/vnd.ollama.image.system"
	MediaTypeParams    = "application/vnd.ollama.image.params"
	MediaTypeLicense   = "application/vnd.ollama.image.license"
)

// TestedOllamaVersion is the Ollama release compat/ollama runs and whose
// client behaviour this package follows.
const TestedOllamaVersion = "0.31.1"

// Limits.
const (
	// maxSmallFile bounds the template, system, params and licence files.
	maxSmallFile = 1 << 20
	// maxInlineModel bounds a GGUF file stored in git rather than LFS,
	// which is kept in ollama_blobs.
	maxInlineModel = 64 << 20
	// tokenTTL is how long a signed blob URL is valid.
	blobURLTTL = time.Hour
)

// PullThrough starts a Pull of files that a Partial mirror does not hold
// (PULL-10). W6 implements it; nil turns Pull-through off.
type PullThrough interface {
	// PullFiles starts, or joins, a Pull of the paths at the Commit and
	// returns when the Pull is queued.
	PullFiles(ctx context.Context, r *repo.Repo, commit model.CommitSHA, paths []string) error
}

// Options configure the Handler.
type Options struct {
	DB         *db.DB
	Repos      repo.Service
	LFS        repo.LFSStore
	Authz      auth.Authz
	OllamaAuth auth.OllamaAuth
	// Signer signs the blob URLs Ollama downloads parts from, because it
	// sends no Authorization header on them.
	Signer *httpx.Signer
	// PublicURL is server.public_url; its scheme is used for the token
	// realm when the request did not come through a TLS-terminating proxy.
	PublicURL string
	// PullThrough is optional.
	PullThrough PullThrough
	// Downloads enforces scan.block_unsafe_downloads on layers that are
	// files of the Repo; optional.
	Downloads *scan.Gate
	// Community counts a download per manifest fetch; optional.
	Community   community.Service
	Instruments *telemetry.Instruments
	Log         *slog.Logger
	// Window is the ranged read size for GGUF headers; zero means 1 MiB.
	Window int64
}

// Handler serves /v2/...
type Handler struct {
	o Options
}

// New returns a Handler.
func New(o Options) (*Handler, error) {
	if o.DB == nil || o.Repos == nil || o.LFS == nil || o.Authz == nil || o.OllamaAuth == nil || o.Signer == nil {
		return nil, errors.New("ollama: DB, Repos, LFS, Authz, OllamaAuth and Signer are required")
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	return &Handler{o: o}, nil
}

// registryError writes the Docker registry error format Ollama reads.
func registryError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	b, _ := json.Marshal(map[string]any{"errors": []map[string]string{{"code": code, "message": message}}})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(b)
	}
}

var digestRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	segs, err := httpx.Segments(r)
	if err != nil || len(segs) == 0 || segs[0] != "v2" {
		registryError(w, r, http.StatusNotFound, "NOT_FOUND", "Not found.")
		return
	}
	segs = segs[1:]
	switch {
	case len(segs) == 0 || (len(segs) == 1 && segs[0] == ""):
		httpx.SetRoute(r.Context(), "/v2/")
		httpx.WriteJSON(w, r, http.StatusOK, map[string]any{})
	case len(segs) == 1 && segs[0] == "token":
		httpx.SetRoute(r.Context(), "/v2/token")
		h.token(w, r)
	case len(segs) >= 4 && (segs[2] == "manifests" || segs[2] == "blobs") || len(segs) >= 3 && segs[2] == "blobs":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			httpx.SetRoute(r.Context(), "/v2/{ns}/{name}/"+segs[2])
			registryError(w, r, http.StatusMethodNotAllowed, "UNSUPPORTED",
				"Hangar does not accept ollama push. Upload GGUF files with the Hub API or git, or use hangar import --from-ollama.")
			return
		}
		if len(segs) != 4 {
			registryError(w, r, http.StatusNotFound, "NOT_FOUND", "Not found.")
			return
		}
		id := model.RepoID{Type: model.RepoTypeModel, Namespace: segs[0], Name: segs[1]}
		if segs[2] == "manifests" {
			httpx.SetRoute(r.Context(), "/v2/{ns}/{name}/manifests/{tag}")
			h.manifest(w, r, id, segs[3])
			return
		}
		httpx.SetRoute(r.Context(), "/v2/{ns}/{name}/blobs/{digest}")
		h.blob(w, r, id, segs[3])
	default:
		registryError(w, r, http.StatusNotFound, "NOT_FOUND", "Not found.")
	}
}

// base returns the scheme and host Ollama used, for the token realm and
// blob URLs. Ollama refuses a realm on another host.
func (h *Handler) base(r *http.Request) string {
	scheme := "http"
	if httpx.IsHTTPS(r) {
		scheme = "https"
	} else if u, err := url.Parse(h.o.PublicURL); err == nil && u.Scheme == "https" && strings.EqualFold(u.Host, r.Host) {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (h *Handler) challenge(w http.ResponseWriter, r *http.Request, id model.RepoID) {
	host := r.Host
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/v2/token",service="%s",scope="repository:%s:pull"`, h.base(r), host, id.String()))
	registryError(w, r, http.StatusUnauthorized, "UNAUTHORIZED", "Authentication required. Register your Ollama public key in Hangar under Settings, Ollama keys.")
}

func target(r *repo.Repo) auth.Target {
	return auth.Target{Repo: &auth.RepoTarget{ID: r.ID, UUID: r.UUID, NamespaceID: r.NamespaceID, Visibility: r.Visibility,
		Gated: r.Gating.Enabled(), Mirror: r.Mirror}}
}

// load finds a Repo the caller may read files of. Callers who cannot
// read it anonymously get a 401 challenge; signed-in callers without
// access get 404, so private names do not leak.
func (h *Handler) load(w http.ResponseWriter, r *http.Request, id model.RepoID, signed bool) (*repo.Repo, bool) {
	ctx := r.Context()
	p := httpx.Principal(ctx)
	rp, err := h.o.Repos.Get(ctx, id)
	if err == nil && rp.DeletedAt != nil {
		err = repo.ErrNotFound
	}
	if err == nil && !signed {
		err = h.o.Authz.Can(ctx, p, auth.ActionReadFiles, target(rp))
	}
	switch {
	case err == nil:
		httpx.SetRepo(ctx, rp.ID)
		return rp, true
	case p.IsAnonymous() && (errors.Is(err, repo.ErrNotFound) || errors.Is(err, auth.ErrNotFound) || errors.Is(err, auth.ErrForbidden) || errors.Is(err, auth.ErrGated)):
		h.challenge(w, r, id)
	case errors.Is(err, auth.ErrGated):
		registryError(w, r, http.StatusForbidden, "DENIED", "This Repo is gated. Request access to it in Hangar first.")
	case errors.Is(err, repo.ErrNotFound), errors.Is(err, auth.ErrNotFound), errors.Is(err, auth.ErrForbidden):
		registryError(w, r, http.StatusNotFound, "NAME_UNKNOWN", "Repo not found. If it is private, make sure your Ollama key is registered and you have access to it.")
	default:
		h.internal(w, r, err)
	}
	return nil, false
}

// internal logs err and answers 500 without details. A request the
// client closed, such as `ollama pull` stopped with Ctrl-C, is answered
// 499 and logged at debug level instead (bellatrix finding 9).
func (h *Handler) internal(w http.ResponseWriter, r *http.Request, err error) {
	if httpx.ClientGone(r, err) {
		h.o.Log.DebugContext(r.Context(), "client closed the request", "error", err.Error())
		registryError(w, r, httpx.StatusClientClosedRequest, "UNKNOWN", httpx.ClientClosedMessage)
		return
	}
	h.o.Log.ErrorContext(r.Context(), "ollama registry request failed", "error", err.Error())
	registryError(w, r, http.StatusInternalServerError, "UNKNOWN", httpx.InternalErrorMessage(r.Context()))
}

// token serves the realm of Ollama's registry auth: it verifies the
// key signature through OllamaAuth and returns a short-lived token.
func (h *Handler) token(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		registryError(w, r, http.StatusMethodNotAllowed, "UNSUPPORTED", "Use GET.")
		return
	}
	q := r.URL.Query()
	scope := q.Get("scope")
	rest, ok := strings.CutPrefix(scope, "repository:")
	name, action, ok2 := strings.Cut(rest, ":")
	if !ok || !ok2 || action != "pull" {
		registryError(w, r, http.StatusBadRequest, "UNSUPPORTED", "The scope must be repository:<namespace>/<name>:pull.")
		return
	}
	id, err := model.ParseRepoID(model.RepoTypeModel, name)
	if err != nil {
		registryError(w, r, http.StatusBadRequest, "NAME_INVALID", "The scope names an invalid Repo.")
		return
	}
	authz := r.Header.Get("Authorization")
	if authz == "" {
		registryError(w, r, http.StatusUnauthorized, "UNAUTHORIZED", "Sign the request with your Ollama key.")
		return
	}
	req := auth.OllamaTokenRequest{Method: http.MethodGet, URL: h.base(r) + r.URL.RequestURI(), Authorization: authz, Repo: id}
	tok, exp, err := h.o.OllamaAuth.IssueRegistryToken(r.Context(), req)
	switch {
	case err == nil:
	case errors.Is(err, auth.ErrInvalidCredentials):
		registryError(w, r, http.StatusUnauthorized, "UNAUTHORIZED", "The Ollama key is not registered in Hangar, or the signature is invalid or stale.")
		return
	case errors.Is(err, auth.ErrNotFound), errors.Is(err, auth.ErrForbidden):
		registryError(w, r, http.StatusNotFound, "NAME_UNKNOWN", "Repo not found, or you cannot read it.")
		return
	case errors.Is(err, auth.ErrGated):
		registryError(w, r, http.StatusForbidden, "DENIED", "This Repo is gated. Request access to it in Hangar first.")
		return
	default:
		h.internal(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, map[string]any{"token": tok, "expires_in": int(time.Until(exp).Seconds()), "issued_at": time.Now().UTC().Format(time.RFC3339)})
}

// Layer is a manifest layer.
type Layer struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

// Manifest is a Docker v2 schema 2 manifest.
type Manifest struct {
	SchemaVersion int     `json:"schemaVersion"`
	MediaType     string  `json:"mediaType"`
	Config        Layer   `json:"config"`
	Layers        []Layer `json:"layers"`
}

// Config is the config blob Ollama reads (types/model ConfigV2).
type Config struct {
	ModelFormat   string   `json:"model_format"`
	ModelFamily   string   `json:"model_family"`
	ModelFamilies []string `json:"model_families"`
	ModelType     string   `json:"model_type"`
	FileType      string   `json:"file_type"`
	Architecture  string   `json:"architecture"`
	OS            string   `json:"os"`
	RootFS        struct {
		Type    string   `json:"type"`
		DiffIDs []string `json:"diff_ids"`
	} `json:"rootfs"`
}

// errNoQuant means the tag names no GGUF file of the Repo.
var errNoQuant = errors.New("no GGUF file matches the tag")

// pickGroup chooses the quant group a tag names: `latest` picks Q4_K_M or
// the closest quant (08-local-tools.md), otherwise the tag is matched
// case-insensitively against group names and GGUF filenames.
func pickGroup(groups []index.QuantGroup, tag string) (index.QuantGroup, error) {
	if len(groups) == 0 {
		return index.QuantGroup{}, fmt.Errorf("%w: the Repo has no GGUF files", errNoQuant)
	}
	if strings.EqualFold(tag, "latest") {
		g, _ := index.PreferredQuant(groups)
		return g, nil
	}
	for _, g := range groups {
		if strings.EqualFold(g.Name, tag) {
			return g, nil
		}
	}
	for _, g := range groups {
		for _, f := range g.Files {
			base := path.Base(f)
			if strings.EqualFold(base, tag) || strings.EqualFold(strings.TrimSuffix(base, path.Ext(base)), tag) {
				return g, nil
			}
		}
	}
	names := make([]string, 0, len(groups))
	for _, g := range groups {
		names = append(names, g.Name)
	}
	return index.QuantGroup{}, fmt.Errorf("%w: %q is not one of %s", errNoQuant, tag, strings.Join(names, ", "))
}

// HumanNumber formats a parameter count as Ollama shows it, such as 3.2B
// or 135M (format.HumanNumber in Ollama).
func HumanNumber(n uint64) string {
	f := float64(n)
	switch {
	case n >= 1_000_000_000:
		v := f / 1e9
		if v == float64(int64(v)) {
			return fmt.Sprintf("%.0fB", v)
		}
		return fmt.Sprintf("%.1fB", v)
	case n >= 1_000_000:
		v := f / 1e6
		if v == float64(int64(v)) {
			return fmt.Sprintf("%.0fM", v)
		}
		return fmt.Sprintf("%.2fM", v)
	case n >= 1000:
		return fmt.Sprintf("%.0fK", f/1e3)
	}
	return strconv.FormatUint(n, 10)
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// storeBlob keeps a generated layer so GET .../blobs/{digest} can serve it.
func (h *Handler) storeBlob(ctx context.Context, mediaType string, b []byte) (Layer, error) {
	d := digestOf(b)
	row := &db.OllamaBlob{Digest: d, MediaType: mediaType, Size: int64(len(b)), Data: b}
	if _, err := h.o.DB.NewInsert().Model(row).On("CONFLICT (digest) DO NOTHING").Exec(ctx); err != nil {
		return Layer{}, err
	}
	return Layer{MediaType: mediaType, Digest: d, Size: int64(len(b))}, nil
}

func (h *Handler) readSmall(ctx context.Context, id model.RepoID, commit model.CommitSHA, e model.Entry) ([]byte, error) {
	if e.Size > maxSmallFile {
		return nil, fmt.Errorf("%s is larger than %d bytes", e.Path, maxSmallFile)
	}
	rc, _, err := h.o.Repos.OpenFile(ctx, id, commit, e.Path, nil)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, maxSmallFile+1))
}

// fileLayer is the layer of a GGUF file: its LFS object, or for a file
// stored in git, its bytes kept in ollama_blobs.
func (h *Handler) fileLayer(ctx context.Context, id model.RepoID, commit model.CommitSHA, e model.Entry, mediaType string) (Layer, error) {
	if e.LFS != nil {
		return Layer{MediaType: mediaType, Digest: "sha256:" + e.LFS.OID, Size: e.LFS.Size}, nil
	}
	if e.Size > maxInlineModel {
		return Layer{}, fmt.Errorf("%s is stored in git rather than LFS and is too large to serve to Ollama", e.Path)
	}
	rc, _, err := h.o.Repos.OpenFile(ctx, id, commit, e.Path, nil)
	if err != nil {
		return Layer{}, err
	}
	b, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		return Layer{}, err
	}
	return h.storeBlob(ctx, mediaType, b)
}

var licenceFileRe = regexp.MustCompile(`(?i)^licen[cs]e(\.(md|txt))?$`)

// build assembles the manifest for a tag at a Commit.
func (h *Handler) build(ctx context.Context, rp *repo.Repo, commit model.CommitSHA, tag string) (*Manifest, []string, error) {
	entries, err := h.o.Repos.Tree(ctx, rp.ID, commit, "", repo.TreeOpts{Recursive: true})
	if err != nil {
		return nil, nil, err
	}
	byPath := map[string]model.Entry{}
	sizes := map[string]int64{}
	for _, e := range entries {
		if e.Type != model.EntryFile {
			continue
		}
		byPath[e.Path] = e
		if strings.EqualFold(path.Ext(e.Path), ".gguf") {
			sizes[e.Path] = e.Size
		}
	}
	g, err := pickGroup(index.GroupQuants(sizes), tag)
	if err != nil {
		return nil, nil, err
	}
	if len(g.Files) > 1 {
		return nil, nil, &splitError{group: g}
	}
	var missing []string
	for _, p := range append(slices.Clone(g.Files), g.Projectors...) {
		if !byPath[p].Present {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		return nil, missing, errNotPresent
	}
	modelEntry := byPath[g.Files[0]]
	modelLayer, err := h.fileLayer(ctx, rp.ID, commit, modelEntry, MediaTypeModel)
	if err != nil {
		return nil, nil, err
	}
	ra := rangeio.New(ctx, modelEntry.Size, h.o.Window, func(ctx context.Context, start, end int64) (io.ReadCloser, error) {
		rc, _, err := h.o.Repos.OpenFile(ctx, rp.ID, commit, modelEntry.Path, &blob.Range{Start: start, End: end})
		return rc, err
	})
	header, err := gguf.Read(ra, modelEntry.Size)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %s: %w", errBadModel, modelEntry.Path, err)
	}
	layers := []Layer{modelLayer}
	if len(g.Projectors) > 0 {
		proj, err := h.fileLayer(ctx, rp.ID, commit, byPath[g.Projectors[0]], MediaTypeProjector)
		if err != nil {
			return nil, nil, err
		}
		layers = append(layers, proj)
	}
	var params []byte
	if e, ok := byPath["template"]; ok {
		b, err := h.readSmall(ctx, rp.ID, commit, e)
		if err != nil {
			return nil, nil, err
		}
		l, err := h.storeBlob(ctx, MediaTypeTemplate, b)
		if err != nil {
			return nil, nil, err
		}
		layers = append(layers, l)
	} else if t, ok, err := MatchTemplate(header.ChatTemplate()); err != nil {
		return nil, nil, err
	} else if ok {
		l, err := h.storeBlob(ctx, MediaTypeTemplate, t.Body)
		if err != nil {
			return nil, nil, err
		}
		layers = append(layers, l)
		params = t.Params
	}
	if e, ok := byPath["system"]; ok {
		b, err := h.readSmall(ctx, rp.ID, commit, e)
		if err != nil {
			return nil, nil, err
		}
		l, err := h.storeBlob(ctx, MediaTypeSystem, b)
		if err != nil {
			return nil, nil, err
		}
		layers = append(layers, l)
	}
	if e, ok := byPath["params"]; ok {
		b, err := h.readSmall(ctx, rp.ID, commit, e)
		if err != nil {
			return nil, nil, err
		}
		if !json.Valid(b) {
			return nil, nil, fmt.Errorf("%w: the params file is not valid JSON", errBadModel)
		}
		params = b
	}
	if len(params) > 0 {
		l, err := h.storeBlob(ctx, MediaTypeParams, params)
		if err != nil {
			return nil, nil, err
		}
		layers = append(layers, l)
	}
	for _, p := range sortedKeys(byPath) {
		if !strings.Contains(p, "/") && licenceFileRe.MatchString(p) {
			b, err := h.readSmall(ctx, rp.ID, commit, byPath[p])
			if err != nil {
				return nil, nil, err
			}
			l, err := h.storeBlob(ctx, MediaTypeLicense, b)
			if err != nil {
				return nil, nil, err
			}
			layers = append(layers, l)
			break
		}
	}
	cfg := Config{ModelFormat: "gguf", ModelFamily: header.Architecture(), ModelType: HumanNumber(uint64(max(0, header.Parameters()))),
		Architecture: "amd64", OS: "linux"}
	if cfg.ModelFamily != "" {
		cfg.ModelFamilies = []string{cfg.ModelFamily}
	} else {
		cfg.ModelFamilies = []string{}
	}
	if _, name := header.FileType(); name != "" {
		cfg.FileType = name
	} else {
		cfg.FileType = g.Name
	}
	cfg.RootFS.Type = "layers"
	for _, l := range layers {
		cfg.RootFS.DiffIDs = append(cfg.RootFS.DiffIDs, l.Digest)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, nil, err
	}
	cfgLayer, err := h.storeBlob(ctx, MediaTypeConfig, raw)
	if err != nil {
		return nil, nil, err
	}
	return &Manifest{SchemaVersion: 2, MediaType: MediaTypeManifest, Config: cfgLayer, Layers: layers}, nil, nil
}

func sortedKeys(m map[string]model.Entry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

var (
	errNotPresent = errors.New("the quant is not in the Arsenal")
	errBadModel   = errors.New("the GGUF file cannot be served")
)

type splitError struct{ group index.QuantGroup }

func (e *splitError) Error() string {
	return fmt.Sprintf("%s is split into %d files (%s ...). Ollama %s loads one model file, so it cannot pull split GGUF models. "+
		"Merge the parts with llama-gguf-split --merge into one file, or choose another quant.",
		e.group.Name, len(e.group.Files), path.Base(e.group.Files[0]), TestedOllamaVersion)
}

func (h *Handler) manifest(w http.ResponseWriter, r *http.Request, id model.RepoID, tag string) {
	ctx := r.Context()
	rp, ok := h.load(w, r, id, false)
	if !ok {
		return
	}
	commit, err := h.o.Repos.Resolve(ctx, rp.ID, rp.DefaultBranch)
	if err != nil {
		registryError(w, r, http.StatusNotFound, "MANIFEST_UNKNOWN", "The Repo has no Commits yet.")
		return
	}
	m, missing, err := h.build(ctx, rp, commit, tag)
	var split *splitError
	switch {
	case err == nil:
	case errors.Is(err, errNoQuant):
		registryError(w, r, http.StatusNotFound, "MANIFEST_UNKNOWN", err.Error()+".")
		return
	case errors.As(err, &split):
		registryError(w, r, http.StatusBadRequest, "MANIFEST_INVALID", split.Error())
		return
	case errors.Is(err, errNotPresent):
		h.notPresent(w, r, rp, commit, tag, missing)
		return
	case errors.Is(err, errBadModel):
		registryError(w, r, http.StatusUnprocessableEntity, "MANIFEST_INVALID", err.Error())
		return
	default:
		h.internal(w, r, err)
		return
	}
	body, err := json.Marshal(m)
	if err != nil {
		h.internal(w, r, err)
		return
	}
	w.Header().Set("Content-Type", MediaTypeManifest)
	w.Header().Set("Docker-Content-Digest", digestOf(body))
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
	if h.o.Community != nil {
		if err := h.o.Community.RecordDownload(ctx, rp.UUID, time.Now()); err != nil {
			h.o.Log.WarnContext(ctx, "record download", "repo.id", rp.ID.String(), "error", err.Error())
		}
	}
}

// notPresent answers a request for a quant a Partial mirror does not hold:
// with Pull-through on it starts the Pull and asks Ollama to retry.
func (h *Handler) notPresent(w http.ResponseWriter, r *http.Request, rp *repo.Repo, commit model.CommitSHA, tag string, missing []string) {
	if h.o.PullThrough != nil {
		if err := h.o.PullThrough.PullFiles(r.Context(), rp, commit, missing); err == nil {
			w.Header().Set("Retry-After", "30")
			registryError(w, r, http.StatusServiceUnavailable, "UNAVAILABLE",
				fmt.Sprintf("Hangar is pulling %s:%s into the Arsenal. Run ollama pull again when the Pull finishes.", rp.ID.String(), tag))
			return
		} else {
			h.o.Log.WarnContext(r.Context(), "start pull-through for ollama", "repo.id", rp.ID.String(), "error", err.Error())
		}
	}
	registryError(w, r, http.StatusNotFound, "MANIFEST_UNKNOWN",
		fmt.Sprintf("%s:%s is not in the Arsenal. Pull it first: hangar pull %s:%s --server-only (missing: %s).",
			rp.ID.String(), tag, rp.ID.String(), tag, strings.Join(missing, ", ")))
}

func blobScope(id model.RepoID, digest string) string {
	return "ollama-blob:" + strings.ToLower(id.String()) + ":" + digest
}

// blob serves a layer. The first GET carries a Location header with a
// signed URL of the same blob, because Ollama 0.31 reads the direct URL
// for its parallel ranged downloads from Location and sends those without
// an Authorization header.
func (h *Handler) blob(w http.ResponseWriter, r *http.Request, id model.RepoID, digest string) {
	ctx := r.Context()
	if !digestRe.MatchString(digest) {
		registryError(w, r, http.StatusBadRequest, "DIGEST_INVALID", "The digest must be sha256:<64 hex digits>.")
		return
	}
	signed := false
	if r.URL.Query().Get(httpx.SigParam) != "" {
		scope, err := h.o.Signer.Verify(r.URL)
		if err != nil || scope != blobScope(id, digest) {
			registryError(w, r, http.StatusUnauthorized, "UNAUTHORIZED", "The download link is invalid or has expired. Run ollama pull again.")
			return
		}
		signed = true
	}
	rp, ok := h.load(w, r, id, signed)
	if !ok {
		return
	}
	var row db.OllamaBlob
	err := h.o.DB.NewSelect().Model(&row).Where("digest = ?", digest).Scan(ctx)
	switch {
	case err == nil:
		h.serve(w, r, rp, digest, row.Size, func(rng *blob.Range) (io.ReadCloser, error) {
			b := row.Data
			if rng != nil {
				start, end, err := rng.Resolve(int64(len(b)))
				if err != nil {
					return nil, err
				}
				b = b[start : end+1]
			}
			return io.NopCloser(bytes.NewReader(b)), nil
		})
		return
	case !errors.Is(err, sql.ErrNoRows):
		h.internal(w, r, err)
		return
	}
	oid := strings.TrimPrefix(digest, "sha256:")
	refs, err := h.o.Repos.LFSRefs(ctx, rp.ID, []string{oid})
	if err != nil {
		h.internal(w, r, err)
		return
	}
	ref, ok := refs[oid]
	if !ok {
		registryError(w, r, http.StatusNotFound, "BLOB_UNKNOWN", "The Repo has no layer with this digest.")
		return
	}
	blocked, err := h.o.Downloads.CheckObjects(ctx, rp.UUID, rp.ID, []string{oid})
	if err != nil {
		h.internal(w, r, err)
		return
	}
	if err := blocked[oid]; err != nil {
		registryError(w, r, http.StatusForbidden, "DENIED", err.Error())
		return
	}
	if !ref.Present {
		registryError(w, r, http.StatusNotFound, "BLOB_UNKNOWN", "This file is not in the Arsenal. Pull it into Hangar first.")
		return
	}
	obj, err := h.o.LFS.Stat(ctx, oid)
	if err != nil {
		registryError(w, r, http.StatusNotFound, "BLOB_UNKNOWN", "The layer's LFS object is missing.")
		return
	}
	h.serve(w, r, rp, digest, obj.Size, func(rng *blob.Range) (io.ReadCloser, error) {
		rc, _, err := h.o.LFS.Open(ctx, oid, rng)
		return rc, err
	})
}

// serve writes a blob with Content-Length, Docker-Content-Digest and
// single-range support, and the headers of every other download
// (blob.SetDownloadHeaders), so a browser never renders it.
func (h *Handler) serve(w http.ResponseWriter, r *http.Request, rp *repo.Repo, digest string, size int64, open func(*blob.Range) (io.ReadCloser, error)) {
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Accept-Ranges", "bytes")
	blob.SetDownloadHeaders(w.Header(), "")
	w.Header().Set("ETag", `"`+digest+`"`)
	if r.URL.Query().Get(httpx.SigParam) == "" {
		direct := h.base(r) + r.URL.EscapedPath()
		if signed, err := h.o.Signer.Sign(direct, blobScope(rp.ID, digest), blobURLTTL); err == nil {
			w.Header().Set("Location", signed)
		}
	}
	var rng *blob.Range
	status := http.StatusOK
	length := size
	if hdr := r.Header.Get("Range"); hdr != "" && r.Method == http.MethodGet {
		start, end, ok := parseRange(hdr, size)
		if !ok {
			w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(size, 10))
			registryError(w, r, http.StatusRequestedRangeNotSatisfiable, "RANGE_INVALID", "The requested range is not satisfiable.")
			return
		}
		rng = &blob.Range{Start: start, End: end}
		status, length = http.StatusPartialContent, end-start+1
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
	}
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	if r.Method == http.MethodHead {
		w.WriteHeader(status)
		return
	}
	rc, err := open(rng)
	if err != nil {
		h.internal(w, r, err)
		return
	}
	defer rc.Close()
	w.WriteHeader(status)
	n, _ := io.Copy(w, rc)
	if h.o.Instruments != nil && h.o.Instruments.DownloadBytes != nil {
		h.o.Instruments.DownloadBytes.Add(r.Context(), n, metric.WithAttributes(
			attribute.String(telemetry.MetricProtocol, string(telemetry.ProtocolOllama)),
			attribute.String(telemetry.MetricMode, "proxy"), attribute.String(telemetry.MetricRepoType, string(rp.ID.Type))))
	}
}

// parseRange parses a single `bytes=a-b`, `bytes=a-` or `bytes=-n` range.
func parseRange(h string, size int64) (int64, int64, bool) {
	spec, ok := strings.CutPrefix(h, "bytes=")
	if !ok || strings.Contains(spec, ",") || size == 0 {
		return 0, 0, false
	}
	a, b, ok := strings.Cut(strings.TrimSpace(spec), "-")
	if !ok {
		return 0, 0, false
	}
	if a == "" {
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		return max(0, size-n), size - 1, true
	}
	start, err := strconv.ParseInt(a, 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	end := size - 1
	if b != "" {
		e, err := strconv.ParseInt(b, 10, 64)
		if err != nil || e < start {
			return 0, 0, false
		}
		end = min(e, size-1)
	}
	return start, end, true
}

// TokenPath is the realm of Ollama's registry auth.
const TokenPath = "/v2/token"

type exemptToken struct{ next httpx.Authenticator }

// Authenticate implements httpx.Authenticator.
func (e exemptToken) Authenticate(r *http.Request) (model.Principal, error) {
	if r.URL.Path == TokenPath {
		return model.Principal{Kind: model.PrincipalAnonymous}, nil
	}
	return e.next.Authenticate(r)
}

// ExemptTokenEndpoint wraps the request Authenticator so it skips the
// token endpoint, whose Authorization header is an Ollama key signature
// (`<public key>:<signature>`) that only OllamaAuth can check. Without it,
// the Authenticator rejects the header as an unknown scheme.
func ExemptTokenEndpoint(a httpx.Authenticator) httpx.Authenticator { return exemptToken{next: a} }
