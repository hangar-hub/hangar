package hfapi

import (
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/datasets"
	"github.com/hangar-hub/hangar/internal/httpx"
)

// parquet serves GET /api/datasets/{repo}/parquet[/{config}[/{split}[/{n}.parquet]]]
// (14-datasets.md "Hub API additions"): the Parquet export URLs grouped by
// Config and Split, or a redirect to one shard.
func (h *Handler) parquet(w http.ResponseWriter, r *http.Request, ref repoRef, args []string) {
	rp, ok := h.load(w, r, ref, auth.ActionReadFiles)
	if !ok {
		return
	}
	config := ""
	if len(args) > 0 {
		config = args[0]
	}
	res, err := h.d.Viewer.Parquet(r.Context(), httpx.Principal(r.Context()), rp.ID, config)
	if err != nil {
		h.viewerError(w, r, err)
		return
	}
	// Shards per Config and Split, in file name order so {n} is stable.
	groups := map[string]map[string][]datasets.ParquetFile{}
	for _, f := range res.ParquetFiles {
		if groups[f.Config] == nil {
			groups[f.Config] = map[string][]datasets.ParquetFile{}
		}
		groups[f.Config][f.Split] = append(groups[f.Config][f.Split], f)
	}
	for _, splits := range groups {
		for _, files := range splits {
			sort.Slice(files, func(i, j int) bool { return files[i].Filename < files[j].Filename })
		}
	}
	shardURL := func(c, s string, n int) string {
		return h.d.Settings.PublicURL + "/api/datasets/" + rp.ID.String() + "/parquet/" + url.PathEscape(c) + "/" + url.PathEscape(s) + "/" + strconv.Itoa(n) + ".parquet"
	}
	urls := func(c, s string) []string {
		out := []string{}
		for i := range groups[c][s] {
			out = append(out, shardURL(c, s, i))
		}
		return out
	}
	notFound := func() {
		httpx.WriteHubError(w, r, &httpx.HubError{Status: http.StatusNotFound, Message: "No Parquet export for this Config and Split. If the dataset is new, Hangar may still be converting it to Parquet."})
	}
	switch len(args) {
	case 0:
		out := map[string]map[string][]string{}
		for c, splits := range groups {
			out[c] = map[string][]string{}
			for s := range splits {
				out[c][s] = urls(c, s)
			}
		}
		httpx.WriteJSON(w, r, http.StatusOK, out)
	case 1:
		if groups[config] == nil {
			notFound()
			return
		}
		out := map[string][]string{}
		for s := range groups[config] {
			out[s] = urls(config, s)
		}
		httpx.WriteJSON(w, r, http.StatusOK, out)
	case 2:
		if len(groups[config][args[1]]) == 0 {
			notFound()
			return
		}
		httpx.WriteJSON(w, r, http.StatusOK, urls(config, args[1]))
	default:
		n, err := strconv.Atoi(strings.TrimSuffix(args[2], ".parquet"))
		files := groups[config][args[1]]
		if err != nil || !strings.HasSuffix(args[2], ".parquet") || n < 0 || n >= len(files) {
			notFound()
			return
		}
		w.Header().Set("Location", files[n].URL)
		w.WriteHeader(http.StatusFound)
	}
}

// croissant serves GET /api/datasets/{repo}/croissant (DATA-10).
func (h *Handler) croissant(w http.ResponseWriter, r *http.Request, ref repoRef) {
	rp, ok := h.load(w, r, ref, auth.ActionRead)
	if !ok {
		return
	}
	doc, err := h.d.Viewer.Croissant(r.Context(), httpx.Principal(r.Context()), rp.ID)
	if err != nil {
		h.viewerError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(doc)
	}
}

func (h *Handler) viewerError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, datasets.ErrNotFound):
		httpx.WriteHubError(w, r, &httpx.HubError{Status: http.StatusNotFound, Message: "The dataset, Config or Split was not found."})
	case errors.Is(err, datasets.ErrNotReady):
		w.Header().Set("Retry-After", "60")
		httpx.WriteHubError(w, r, &httpx.HubError{Status: http.StatusNotFound, Message: "Preparing preview. Hangar is converting this dataset to Parquet."})
	default:
		h.internalError(w, r, err)
	}
}
