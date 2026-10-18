package api

import (
	"net/http"

	"github.com/hangar-hub/hangar/internal/api/gen"
	"github.com/hangar-hub/hangar/internal/httpx"
)

// AdminListTokens implements GET /admin/tokens: every live Token, newest
// first. Secrets are never stored, so they are never shown.
func (h *Identity) AdminListTokens(w http.ResponseWriter, r *http.Request, params gen.AdminListTokensParams) {
	if _, ok := h.instanceAdmin(w, r); !ok {
		return
	}
	cursor := ""
	if params.Cursor != nil {
		cursor = *params.Cursor
	}
	list, next, err := h.auth.TokenStore.ListPage(r.Context(), cursor, idLimit(params.Limit))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := gen.TokenList{Items: make([]gen.Token, 0, len(list)), NextCursor: idNextCursor(next)}
	for _, t := range list {
		out.Items = append(out.Items, idTokenOut(t))
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

// AdminRevokeToken implements DELETE /admin/tokens/{id}.
func (h *Identity) AdminRevokeToken(w http.ResponseWriter, r *http.Request, id gen.ID) {
	p, ok := h.instanceAdmin(w, r)
	if !ok {
		return
	}
	if err := h.auth.TokenStore.Revoke(r.Context(), id, p); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
