package viewer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/jamesagarside/hangar/internal/datasets"
	"github.com/jamesagarside/hangar/internal/datasets/features"
	"github.com/jamesagarside/hangar/internal/datasets/media"
	"github.com/jamesagarside/hangar/internal/datasets/pipeline"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/model"
)

// assetScope is the signed URL scope of media cells.
const assetScope = "viewer-asset"

func kindName(k features.Kind) string {
	switch k {
	case features.KindImage:
		return "image"
	case features.KindAudio:
		return "audio"
	case features.KindVideo:
		return "video"
	}
	return "document"
}

// assetURL returns a function that signs media cell URLs of a Split.
// URLs have the Hub's shape, {dataset}/--/{revision}/--/{config}/{split}/
// {row}/{column}/{file}, under /datasets-server/assets, and authorise
// themselves for AssetTTL, as the Hub's signed URLs do.
func (s *Service) assetURL(st *state, config, split string) func(m mediaRef, ext string) string {
	return func(m mediaRef, ext string) string {
		segs := []string{"assets", st.repo.ID.Namespace, st.repo.ID.Name, "--", string(st.commit), "--", config, split,
			strconv.FormatInt(m.rowIdx, 10), m.column, appendHashSuffix(kindName(m.kind), m.path) + ext}
		for i, x := range segs {
			segs[i] = url.PathEscape(x)
		}
		rel := "/" + strings.Join(segs, "/")
		if len(m.path) > 0 {
			b, _ := json.Marshal(m.path)
			rel += "?jp=" + url.QueryEscape(string(b))
		}
		signed, err := s.d.Signer.Sign(rel, assetScope, s.o.AssetTTL)
		if err != nil {
			return ""
		}
		u, err := url.Parse(signed)
		if err != nil {
			return ""
		}
		return strings.TrimSuffix(s.o.PublicURL, "/") + httpx.ViewerPrefix + u.EscapedPath() + "?" + u.RawQuery
	}
}

// serveAsset serves a media cell from a signed URL. path is the request
// path relative to the Viewer root, starting with /assets/.
func (s *Service) serveAsset(w http.ResponseWriter, r *http.Request, escapedPath string) {
	u := &url.URL{Path: mustUnescape(escapedPath), RawPath: escapedPath, RawQuery: r.URL.RawQuery}
	scope, err := s.d.Signer.Verify(u)
	if err != nil || scope != assetScope {
		writeError(w, r, http.StatusForbidden, "ExternalAuthenticatedError", "The asset URL is invalid or has expired.")
		return
	}
	segs, err := httpx.SplitEscapedPath(strings.TrimPrefix(escapedPath, "/assets/"))
	if err != nil || len(segs) != 10 || segs[2] != "--" || segs[4] != "--" {
		writeError(w, r, http.StatusNotFound, "ResponseNotFound", "Not found.")
		return
	}
	id := model.RepoID{Type: model.RepoTypeDataset, Namespace: segs[0], Name: segs[1]}
	commit := model.CommitSHA(segs[3])
	config, split, column := segs[5], segs[6], segs[8]
	row, err := strconv.ParseInt(segs[7], 10, 64)
	if err != nil || row < 0 {
		writeError(w, r, http.StatusNotFound, "ResponseNotFound", "Not found.")
		return
	}
	var path []any
	if jp := r.URL.Query().Get("jp"); jp != "" {
		if err := json.Unmarshal([]byte(jp), &path); err != nil {
			writeError(w, r, http.StatusNotFound, "ResponseNotFound", "Not found.")
			return
		}
	}
	b, name, err := s.mediaBytes(r.Context(), id, commit, config, split, row, column, path)
	if err != nil {
		if errors.Is(err, datasets.ErrNotFound) {
			writeError(w, r, http.StatusNotFound, "ResponseNotFound", "Not found.")
			return
		}
		s.d.Log.ErrorContext(r.Context(), "dataset asset failed", "error", err.Error())
		writeError(w, r, http.StatusInternalServerError, "UnexpectedApiError", "Unexpected error.")
		return
	}
	mime := "application/octet-stream"
	if _, _, m, err := media.ImageSize(b); err == nil {
		mime = m
	} else if m, _ := media.AudioMIME(name, b); m != "application/octet-stream" {
		mime = m
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeContent(w, r, "", s.d.Now(), bytes.NewReader(b))
}

func mustUnescape(p string) string {
	u, err := url.PathUnescape(p)
	if err != nil {
		return p
	}
	return u
}

// mediaBytes reads a media cell of a converted Split.
func (s *Service) mediaBytes(ctx context.Context, id model.RepoID, commit model.CommitSHA, config, split string, row int64, column string, path []any) ([]byte, string, error) {
	r, err := s.d.Repos.Get(ctx, id)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %w", datasets.ErrNotFound, err)
	}
	info, err := s.d.Store.Info(ctx, r.UUID, commit, config)
	if errors.Is(err, pipeline.ErrNoInfo) {
		return nil, "", datasets.ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	st := &state{repo: r, commit: commit}
	t, err := s.table(st, info, split)
	if err != nil {
		return nil, "", err
	}
	if row >= t.numRows() || t.features.Index(column) < 0 {
		return nil, "", datasets.ErrNotFound
	}
	rows, err := t.read(ctx, []string{column}, row, 1)
	if err != nil || len(rows) != 1 {
		return nil, "", fmt.Errorf("%w: row %d", datasets.ErrNotFound, row)
	}
	v := rows[0][0]
	st0 := t.storage.Fields[t.features.Index(column)].Type
	for _, p := range path {
		switch k := p.(type) {
		case float64:
			l, ok := v.([]any)
			if !ok || int(k) < 0 || int(k) >= len(l) {
				return nil, "", datasets.ErrNotFound
			}
			v, st0 = l[int(k)], st0.Elem
		case string:
			l, ok := v.([]any)
			if !ok || st0.ID != features.Struct {
				return nil, "", datasets.ErrNotFound
			}
			i := st0.Field(k)
			if i < 0 || i >= len(l) {
				return nil, "", datasets.ErrNotFound
			}
			v, st0 = l[i], st0.Fields[i].Type
		default:
			return nil, "", datasets.ErrNotFound
		}
		if st0 == nil {
			return nil, "", datasets.ErrNotFound
		}
	}
	m, ok := v.([]any)
	if !ok || len(m) < 2 {
		return nil, "", datasets.ErrNotFound
	}
	b, ok := m[0].([]byte)
	if !ok {
		return nil, "", datasets.ErrNotFound
	}
	name, _ := m[1].(string)
	return b, name, nil
}
