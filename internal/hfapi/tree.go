package hfapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"

	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/httpx"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/scan"
)

// treePageSize matches the Hub's page size for tree listings.
const treePageSize = 1000

// maxPathsInfo bounds one paths-info request; the client sends batches of 500.
const maxPathsInfo = 1000

// tree serves GET /api/{type}s/{repo}/tree/{rev}[/{path}] (contract §3.1),
// paginated with an absolute Link header on server.public_url.
func (h *Handler) tree(w http.ResponseWriter, r *http.Request, ref repoRef, rev, dir string) {
	rp, ok := h.load(w, r, ref, auth.ActionRead)
	if !ok {
		return
	}
	sha, ok := h.resolveRevision(w, r, rp, rev)
	if !ok {
		return
	}
	clean, err := model.CleanRepoPath(dir)
	if err != nil {
		httpx.HubEntryNotFound(w, r, dir, rev, "")
		return
	}
	q := r.URL.Query()
	recursive := httpx.QueryBool(q, "recursive", false)
	expand := httpx.QueryBool(q, "expand", false)
	limit := httpx.QueryInt(q, "limit", treePageSize, 1, treePageSize)
	after := ""
	if c := q.Get("cursor"); c != "" {
		b, err := base64.RawURLEncoding.DecodeString(c)
		if err != nil {
			h.writeBadRequest(w, r, "The cursor is not valid.")
			return
		}
		after = string(b)
	}
	entries, err := h.d.Repos.Tree(r.Context(), rp.ID, sha, clean, repo.TreeOpts{Recursive: recursive, Expand: expand, After: after, Limit: limit + 1})
	if err != nil {
		if errors.Is(err, repo.ErrEntryNotFound) {
			httpx.HubEntryNotFound(w, r, dir, rev, "")
			return
		}
		h.serviceError(w, r, rp.ID, err)
		return
	}
	if len(entries) > limit {
		entries = entries[:limit]
		next := url.Values{}
		next.Set("recursive", boolString(recursive))
		next.Set("expand", boolString(expand))
		next.Set("limit", strconv.Itoa(limit))
		next.Set("cursor", base64.RawURLEncoding.EncodeToString([]byte(entries[len(entries)-1].Path)))
		httpx.SetLinkNext(w, httpx.AbsoluteURL(h.d.Settings.PublicURL, r.URL.EscapedPath(), next))
	}
	sec := h.fileSecurity(r.Context(), rp, sha, expand)
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, treeEntry(e, expand, sec))
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

// pathsInfo serves POST /api/{type}s/{repo}/paths-info/{rev}
// (contract §3.2): entries for the paths that exist, missing ones left out.
func (h *Handler) pathsInfo(w http.ResponseWriter, r *http.Request, ref repoRef, rev string) {
	rp, ok := h.load(w, r, ref, auth.ActionRead)
	if !ok {
		return
	}
	sha, ok := h.resolveRevision(w, r, rp, rev)
	if !ok {
		return
	}
	paths, expand, err := h.readPathsInfo(r)
	if err != nil {
		h.writeBadRequest(w, r, err.Error())
		return
	}
	if len(paths) > maxPathsInfo {
		h.writeBadRequest(w, r, "Send at most 1,000 paths per request.")
		return
	}
	clean := make([]string, 0, len(paths))
	for _, p := range paths {
		c, err := model.CleanRepoPath(p)
		if err != nil || c == "" {
			continue
		}
		clean = append(clean, c)
	}
	out := []map[string]any{}
	if len(clean) > 0 {
		entries, err := h.d.Repos.Entries(r.Context(), rp.ID, sha, clean, expand)
		if err != nil {
			h.serviceError(w, r, rp.ID, err)
			return
		}
		sec := h.fileSecurity(r.Context(), rp, sha, expand)
		for _, e := range entries {
			out = append(out, treeEntry(e, expand, sec))
		}
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

// readPathsInfo accepts the form body huggingface_hub sends (repeated
// `paths`, string `expand`) and a JSON body `{"paths": [...], "expand": bool}`.
func (h *Handler) readPathsInfo(r *http.Request) ([]string, bool, error) {
	body := http.MaxBytesReader(nil, r.Body, h.d.Settings.BodyMax)
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct == "application/json" {
		var req struct {
			Paths  []string `json:"paths"`
			Expand any      `json:"expand"`
		}
		if err := json.NewDecoder(body).Decode(&req); err != nil {
			return nil, false, errors.New("the request body is not valid JSON")
		}
		return req.Paths, anyBool(req.Expand), nil
	}
	b, err := io.ReadAll(body)
	if err != nil {
		return nil, false, errors.New("the request body could not be read")
	}
	form, err := url.ParseQuery(string(b))
	if err != nil {
		return nil, false, errors.New("the request body is not a valid form")
	}
	paths := append(form["paths"], form["paths[]"]...)
	return paths, httpx.QueryBool(form, "expand", false), nil
}

// anyBool reads a JSON boolean or a Hub-style boolean string.
func anyBool(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		b, _ := httpx.ParseBool(x)
		return b
	case float64:
		return x != 0
	}
	return false
}

// treeEntry renders one entry in the Hub's tree shape. It never has
// xetHash (contract §8.2).
func treeEntry(e model.Entry, expand bool, sec map[string]map[string]any) map[string]any {
	out := map[string]any{"type": string(e.Type), "oid": e.OID, "size": e.Size, "path": e.Path}
	if e.Type == model.EntryDirectory {
		out["size"] = 0
	}
	if e.LFS != nil {
		out["lfs"] = map[string]any{"oid": e.LFS.OID, "size": e.LFS.Size, "pointerSize": e.LFS.PointerSize}
	}
	if expand {
		if e.LastCommit != nil {
			out["lastCommit"] = map[string]any{"id": string(e.LastCommit.SHA), "title": e.LastCommit.Title, "date": model.NewHubTime(e.LastCommit.Date)}
		}
		if e.Type == model.EntryFile && sec != nil {
			if s, ok := sec[e.Path]; ok {
				out["securityFileStatus"] = s
			} else {
				out["securityFileStatus"] = securityFileStatus(scan.StatusSafe, nil)
			}
		}
	}
	return out
}

// fileSecurity returns each scanned file's securityFileStatus, or nil when
// the Commit is not scanned (the field is then left out, never half-filled).
func (h *Handler) fileSecurity(ctx context.Context, rp *repo.Repo, sha model.CommitSHA, expand bool) map[string]map[string]any {
	if !expand {
		return nil
	}
	res, err := h.d.Scan.Results(ctx, rp.UUID, sha)
	if err != nil {
		if !errors.Is(err, scan.ErrNotScanned) {
			h.log.WarnContext(ctx, "could not read scan results", "error", err)
		}
		return nil
	}
	out := make(map[string]map[string]any, len(res.Files))
	for _, f := range res.Files {
		out[f.Path] = securityFileStatus(f.Status, f.Findings)
	}
	return out
}

// securityFileStatus renders the Hub's per-file security object with every
// key the client reads: status, avScan and pickleImportScan. Hangar does no
// antivirus scanning, so avScan says so.
func securityFileStatus(s scan.Status, findings []scan.Finding) map[string]any {
	imports := []map[string]any{}
	for _, f := range findings {
		if f.Module == "" && f.Name == "" {
			continue
		}
		imports = append(imports, map[string]any{"module": f.Module, "name": f.Name, "safety": importSafety(f.Status)})
	}
	return map[string]any{
		"status":           hubScanStatus(s),
		"avScan":           map[string]any{"status": "unscanned"},
		"pickleImportScan": map[string]any{"status": hubScanStatus(s), "pickleImports": imports},
	}
}

func hubScanStatus(s scan.Status) string {
	switch s {
	case scan.StatusUnsafe:
		return "unsafe"
	case scan.StatusSuspicious:
		return "caution"
	case scan.StatusError:
		return "error"
	default:
		return "safe"
	}
}

func importSafety(s scan.Status) string {
	switch s {
	case scan.StatusUnsafe:
		return "dangerous"
	case scan.StatusSuspicious:
		return "suspicious"
	default:
		return "innocuous"
	}
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
