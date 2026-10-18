package hfapi

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/go-git/go-git/v6/plumbing/format/gitattributes"
	"github.com/go-git/go-git/v6/plumbing/format/gitignore"

	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/blob"
	"github.com/hangar-hub/hangar/internal/httpx"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
)

// maxPreuploadFiles bounds one preupload request; the client sends 256.
const maxPreuploadFiles = 1000

// maxRuleFile bounds the .gitattributes and .gitignore files read.
const maxRuleFile = 1 << 20

// Upload modes.
const (
	modeRegular = "regular"
	modeLFS     = "lfs"
)

// preupload serves POST /api/{type}s/{repo}/preupload/{rev} (contract §7.5
// and 03-hf-api.md rule 12).
func (h *Handler) preupload(w http.ResponseWriter, r *http.Request, ref repoRef, rev string) {
	createPR := httpx.QueryBool(r.URL.Query(), "create_pr", false)
	action := auth.ActionWrite
	if createPR {
		action = auth.ActionDiscuss
	}
	rp, ok := h.load(w, r, ref, action)
	if !ok {
		return
	}
	var req struct {
		Files []struct {
			Path   string `json:"path"`
			Sample string `json:"sample"`
			Size   int64  `json:"size"`
		} `json:"files"`
		GitIgnore *string `json:"gitIgnore"`
	}
	if !h.decodeJSON(w, r, &req) {
		return
	}
	if len(req.Files) > maxPreuploadFiles {
		h.writeBadRequest(w, r, "Send at most 1000 files per preupload request.")
		return
	}
	sha, ok := h.resolveRevision(w, r, rp, rev)
	if !ok {
		return
	}
	ctx := r.Context()
	rules, err := h.loadRules(ctx, rp.ID, sha, req.GitIgnore)
	if err != nil {
		h.serviceError(w, r, rp.ID, err)
		return
	}
	clean := make([]string, len(req.Files))
	var existing []string
	for i, f := range req.Files {
		c, err := model.CleanRepoPath(f.Path)
		if err != nil || c == "" {
			h.writeBadRequest(w, r, "Invalid path in repo: "+f.Path+".")
			return
		}
		clean[i] = c
		existing = append(existing, c)
	}
	entries := map[string]model.Entry{}
	if len(existing) > 0 {
		es, err := h.d.Repos.Entries(ctx, rp.ID, sha, existing, false)
		if err != nil {
			h.serviceError(w, r, rp.ID, err)
			return
		}
		for _, e := range es {
			entries[e.Path] = e
		}
	}
	out := make([]map[string]any, 0, len(req.Files))
	for i, f := range req.Files {
		mode := h.uploadMode(f.Size, rules.isLFS(clean[i]))
		var oid any
		if e, ok := entries[clean[i]]; ok && e.Type == model.EntryFile {
			switch {
			case mode == modeLFS && e.LFS != nil:
				oid = e.LFS.OID
			case mode == modeRegular && e.LFS == nil:
				oid = e.OID
			}
		}
		out = append(out, map[string]any{"path": f.Path, "uploadMode": mode, "shouldIgnore": rules.ignored(clean[i]), "oid": oid})
	}
	httpx.WriteJSON(w, r, http.StatusOK, map[string]any{"files": out, "commitOid": string(sha)})
}

// uploadMode applies the upload mode policy: regular below
// hfapi.regular_upload_max unless .gitattributes stores the path in LFS,
// lfs otherwise. With hfapi.xet set to reject, files up to the limit are
// regular even when .gitattributes says LFS, so small uploads work with
// no client setup (03-hf-api.md "Modes").
func (h *Handler) uploadMode(size int64, lfsRule bool) string {
	limit := h.d.Settings.RegularUploadMax
	if h.d.Settings.XetReject {
		if size <= limit {
			return modeRegular
		}
		return modeLFS
	}
	if lfsRule || size >= limit {
		return modeLFS
	}
	return modeRegular
}

// pathRules are a Commit's LFS rules from .gitattributes and its ignore
// rules from .gitignore (plus the .gitignore being uploaded).
type pathRules struct {
	attrs  gitattributes.Matcher
	ignore gitignore.Matcher
}

func (p pathRules) isLFS(path string) bool {
	if p.attrs == nil {
		return false
	}
	res, _ := p.attrs.Match(strings.Split(path, "/"), []string{"filter"})
	a, ok := res["filter"]
	return ok && a.IsValueSet() && a.Value() == "lfs"
}

func (p pathRules) ignored(path string) bool {
	return p.ignore != nil && p.ignore.Match(strings.Split(path, "/"), false)
}

func (h *Handler) loadRules(ctx context.Context, id model.RepoID, sha model.CommitSHA, uploadedIgnore *string) (pathRules, error) {
	var out pathRules
	attrs, err := h.readRepoFile(ctx, id, sha, ".gitattributes")
	if err != nil {
		return out, err
	}
	if attrs != "" {
		stack, err := gitattributes.ReadAttributes(strings.NewReader(attrs), nil, true)
		if err == nil {
			out.attrs = gitattributes.NewMatcher(stack)
		}
	}
	ignore := ""
	if uploadedIgnore != nil {
		ignore = *uploadedIgnore
	} else if ignore, err = h.readRepoFile(ctx, id, sha, ".gitignore"); err != nil {
		return out, err
	}
	if ignore != "" {
		var patterns []gitignore.Pattern
		sc := bufio.NewScanner(strings.NewReader(ignore))
		for sc.Scan() {
			line := strings.TrimRight(sc.Text(), "\r")
			if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
				continue
			}
			patterns = append(patterns, gitignore.ParsePattern(line, nil))
		}
		out.ignore = gitignore.NewMatcher(patterns)
	}
	return out, nil
}

// readRepoFile reads a small file at a Commit, or "" if it does not exist.
func (h *Handler) readRepoFile(ctx context.Context, id model.RepoID, sha model.CommitSHA, p string) (string, error) {
	rc, e, err := h.d.Repos.OpenFile(ctx, id, sha, p, nil)
	if err != nil {
		if errors.Is(err, repo.ErrEntryNotFound) || errors.Is(err, repo.ErrNotPresent) {
			return "", nil
		}
		return "", err
	}
	defer func() { _ = rc.Close() }()
	if e.Size > maxRuleFile {
		return "", nil
	}
	b, err := io.ReadAll(io.LimitReader(rc, maxRuleFile))
	if err != nil && !errors.Is(err, blob.ErrInvalidRange) {
		return "", err
	}
	return string(b), nil
}
