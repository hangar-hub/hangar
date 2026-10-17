package viewer

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jamesagarside/hangar/internal/datasets"
	"github.com/jamesagarside/hangar/internal/datasets/pq"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/model"
)

// Messages of the Hub's authentication errors.
const (
	msgUnauthenticated = "The dataset does not exist, or is not accessible without authentication (private or gated). Please check the spelling of the dataset name or retry with authentication."
	msgAuthenticated   = "The dataset does not exist, or is not accessible with the current credentials (private or gated). Please check the spelling of the dataset name or retry with other authentication credentials."
	msgNotReady        = "Preparing preview. Hangar is converting this dataset to Parquet."
)

func writeError(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	w.Header().Set("X-Error-Code", code)
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, r, status, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, r *http.Request, st *state, v any) {
	if st != nil {
		w.Header().Set("X-Revision", st.Revision())
	}
	w.Header().Set("Cache-Control", "no-store")
	b, ok := v.(json.RawMessage)
	if !ok {
		var err error
		if b, err = json.Marshal(v); err != nil {
			httpx.WriteJSON(w, r, http.StatusInternalServerError, map[string]string{"error": "Unexpected error."})
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(unescapeHTML(b))
	}
}

// unescapeHTML undoes encoding/json's escaping of <, >, & and the line
// and paragraph separators inside strings, so responses match the Hub's
// orjson output byte for byte. The response is JSON with nosniff, so the
// characters are safe unescaped.
func unescapeHTML(b []byte) []byte {
	if !bytes.Contains(b, []byte(`\u00`)) && !bytes.Contains(b, []byte(`\u202`)) {
		return b
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		if b[i] != '\\' || i+1 >= len(b) {
			out = append(out, b[i])
			continue
		}
		if b[i+1] == 'u' && i+6 <= len(b) {
			switch string(b[i+2 : i+6]) {
			case "003c":
				out, i = append(out, '<'), i+5
				continue
			case "003e":
				out, i = append(out, '>'), i+5
				continue
			case "0026":
				out, i = append(out, '&'), i+5
				continue
			case "2028":
				out, i = append(out, "\u2028"...), i+5
				continue
			case "2029":
				out, i = append(out, "\u2029"...), i+5
				continue
			}
		}
		// Any other escape, including an escaped backslash, is copied whole.
		out = append(out, b[i], b[i+1])
		i++
	}
	return out
}

// Handler returns the Dataset Viewer HTTP API. It expects paths relative
// to the Viewer root (/splits, /rows, ...), as httpx.Dispatch passes them.
func (s *Service) Handler() http.Handler {
	return http.HandlerFunc(s.serve)
}

type params struct {
	r *http.Request
}

func (p params) get(name string) string { return p.r.URL.Query().Get(name) }

func (s *Service) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, r, http.StatusMethodNotAllowed, "MethodNotAllowed", "Only GET and HEAD are allowed.")
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Expose-Headers", "X-Error-Code, X-Revision")
	escaped := r.URL.EscapedPath()
	if strings.HasPrefix(escaped, "/assets/") {
		httpx.SetRoute(r.Context(), "/datasets-server/assets/...")
		s.serveAsset(w, r, escaped)
		return
	}
	endpoint := strings.TrimSuffix(escaped, "/")
	known := map[string]bool{"/is-valid": true, "/splits": true, "/first-rows": true, "/rows": true, "/search": true, "/filter": true,
		"/parquet": true, "/info": true, "/size": true, "/statistics": true, "/opt-in-out-urls": true, "/presidio-entities": true}
	// The route template names only known endpoints, so the access log,
	// span names and metrics never carry a raw path.
	if known[endpoint] || endpoint == "/healthcheck" {
		httpx.SetRoute(r.Context(), "/datasets-server"+endpoint)
	} else {
		httpx.SetRoute(r.Context(), "/datasets-server/{endpoint}")
	}
	if endpoint == "/healthcheck" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok"))
		return
	}
	if !known[endpoint] {
		writeError(w, r, http.StatusNotFound, "ResponseNotFound", "Not found.")
		return
	}
	p := params{r}
	dataset := p.get("dataset")
	if strings.TrimSpace(dataset) == "" {
		writeError(w, r, http.StatusUnprocessableEntity, "MissingRequiredParameter", "Parameter 'dataset' is required")
		return
	}
	id, err := model.ParseRepoID(model.RepoTypeDataset, dataset)
	if err != nil {
		s.notFound(w, r)
		return
	}
	httpx.SetRepo(r.Context(), id)
	principal := httpx.Principal(r.Context())
	config, split := p.get("config"), p.get("split")
	need := func(names ...string) bool {
		for _, n := range names {
			if strings.TrimSpace(p.get(n)) == "" {
				writeError(w, r, http.StatusUnprocessableEntity, "MissingRequiredParameter", "Parameter '"+n+"' is required")
				return false
			}
		}
		return true
	}
	ctx := r.Context()
	var st *state
	var out any
	switch endpoint {
	case "/is-valid":
		if split != "" && !need("config") {
			return
		}
		out, st, err = s.IsValidFor(ctx, principal, id, config, split)
	case "/splits":
		var resp *datasets.SplitsResponse
		resp, st, err = s.SplitsFor(ctx, principal, id, config)
		if err == nil && config != "" {
			out = map[string]any{"splits": resp.Splits}
		} else {
			out = resp
		}
	case "/first-rows":
		if !need("config", "split") {
			return
		}
		out, st, err = s.FirstRowsFor(ctx, principal, id, config, split)
	case "/rows", "/search", "/filter":
		if !need("config", "split") {
			return
		}
		offset, length, perr := pageParams(p)
		if perr != nil {
			writeError(w, r, http.StatusUnprocessableEntity, "InvalidParameter", perr.Error())
			return
		}
		base := datasets.RowsRequest{Repo: id, Config: config, Split: split, Offset: offset, Length: length}
		switch endpoint {
		case "/rows":
			out, st, err = s.RowsFor(ctx, principal, base)
		case "/search":
			if !need("query") {
				return
			}
			out, st, err = s.SearchFor(ctx, principal, datasets.SearchRequest{RowsRequest: base, Query: p.get("query")})
		default:
			out, st, err = s.FilterFor(ctx, principal, datasets.FilterRequest{RowsRequest: base, Where: p.get("where"), OrderBy: p.get("orderby")})
		}
	case "/parquet":
		out, st, err = s.ParquetFor(ctx, principal, id, config)
	case "/info":
		out, st, err = s.InfoFor(ctx, principal, id, config)
	case "/size":
		out, st, err = s.SizeFor(ctx, principal, id, config)
	case "/statistics":
		if !need("config", "split") {
			return
		}
		out, st, err = s.StatisticsFor(ctx, principal, id, config, split)
	case "/opt-in-out-urls":
		out, st, err = s.optInOut(ctx, principal, id, config, split)
	case "/presidio-entities":
		out, st, err = s.presidio(ctx, principal, id, config, split)
	}
	if err != nil {
		s.writeErr(w, r, st, err)
		return
	}
	writeJSON(w, r, st, out)
}

// paramError is a parameter error with the Hub's message, which starts
// with a capital letter.
type paramError string

func (e paramError) Error() string { return string(e) }

func pageParams(p params) (int64, int, error) {
	offset, length := int64(0), datasets.MaxRowsPerPage
	if v := p.get("offset"); v != "" {
		o, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0, 0, paramError("Parameter 'offset' must be integer")
		}
		offset = o
	}
	if v := p.get("length"); v != "" {
		l, err := strconv.Atoi(v)
		if err != nil {
			return 0, 0, paramError("Parameter 'length' must be integer")
		}
		length = l
	}
	if err := checkPage(offset, length); err != nil {
		return 0, 0, errors.New(strings.TrimPrefix(err.Error(), ErrInvalidParameter.Error()+": "))
	}
	return offset, length, nil
}

func (s *Service) notFound(w http.ResponseWriter, r *http.Request) {
	if httpx.Principal(r.Context()).IsAnonymous() {
		writeError(w, r, http.StatusNotFound, "ExternalUnauthenticatedError", msgUnauthenticated)
		return
	}
	writeError(w, r, http.StatusNotFound, "ExternalAuthenticatedError", msgAuthenticated)
}

// writeErr maps an error to the Hub's viewer error responses.
func (s *Service) writeErr(w http.ResponseWriter, r *http.Request, st *state, err error) {
	if st != nil {
		w.Header().Set("X-Revision", st.Revision())
	}
	msg := err.Error()
	var he *hubError
	var tb *pq.TooBigError
	switch {
	case errors.As(err, &he):
		writeError(w, r, he.status, he.code, he.message)
	case errors.As(err, &tb):
		he = tooBigError(tb, http.StatusInternalServerError)
		writeError(w, r, he.status, he.code, he.message)
	case errors.Is(err, datasets.ErrNotFound) && st == nil:
		s.notFound(w, r)
	case errors.Is(err, datasets.ErrNotFound):
		writeError(w, r, http.StatusNotFound, "ResponseNotFound", "Not found.")
	case errors.Is(err, datasets.ErrNotReady):
		w.Header().Set("Retry-After", "30")
		writeError(w, r, http.StatusInternalServerError, "ResponseNotReady", msgNotReady)
	case errors.Is(err, ErrInvalidParameter):
		writeError(w, r, http.StatusUnprocessableEntity, "InvalidParameter", strings.TrimPrefix(msg, ErrInvalidParameter.Error()+": "))
	case errors.Is(err, datasets.ErrInvalidQuery):
		writeError(w, r, http.StatusUnprocessableEntity, "InvalidParameter", strings.TrimPrefix(msg, datasets.ErrInvalidQuery.Error()+": "))
	default:
		s.internalError(w, r, "dataset viewer request failed", err)
	}
}

// internalError logs err as logMsg and answers 500 without details. A
// request the client closed, such as a Viewer tab shut mid-query, is
// answered 499 and logged at debug level instead (bellatrix finding 9).
func (s *Service) internalError(w http.ResponseWriter, r *http.Request, logMsg string, err error) {
	if httpx.ClientGone(r, err) {
		s.d.Log.DebugContext(r.Context(), "client closed the request", "error", err.Error())
		writeError(w, r, httpx.StatusClientClosedRequest, "ClientClosedRequest", httpx.ClientClosedMessage)
		return
	}
	s.d.Log.ErrorContext(r.Context(), logMsg, "error", err.Error())
	writeError(w, r, http.StatusInternalServerError, "UnexpectedApiError", "Unexpected error.")
}
