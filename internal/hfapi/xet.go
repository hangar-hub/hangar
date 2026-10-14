package hfapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/xet"
)

// Xet token response headers (Xet research §1.3).
const (
	headerXetCASURL     = "X-Xet-Cas-Url"
	headerXetToken      = "X-Xet-Access-Token" //nolint:gosec // a header name, not a credential.
	headerXetExpiration = "X-Xet-Token-Expiration"
)

// xetWriteToken serves GET /api/{type}s/{repo}/xet-write-token/{rev}
// [?create_pr=1] through xet.Ingestor (03-hf-api.md "Xet"). The literal
// revision `None` means the default branch. There is no xet-read-token
// route: Hangar never offers Xet downloads.
func (h *Handler) xetWriteToken(w http.ResponseWriter, r *http.Request, ref repoRef, rev string) {
	createPR := httpx.QueryBool(r.URL.Query(), "create_pr", false)
	action := auth.ActionWrite
	if createPR {
		action = auth.ActionDiscuss
	}
	rp, ok := h.load(w, r, ref, action)
	if !ok {
		return
	}
	if h.d.Settings.XetReject {
		h.writeBadRequest(w, r, xet.RejectMessage)
		return
	}
	if rev == "None" || rev == "" {
		rev = rp.DefaultBranch
	}
	tok, err := h.d.Xet.IssueWriteToken(r.Context(), xet.TokenRequest{
		Principal: httpx.Principal(r.Context()), Repo: rp.ID, RepoUUID: rp.UUID, Revision: rev, CreatePR: createPR,
	})
	if err != nil {
		switch {
		case errors.Is(err, xet.ErrRejected):
			h.writeBadRequest(w, r, xet.RejectMessage)
		case errors.Is(err, xet.ErrScope):
			h.writeForbidden(w, r, "You cannot upload to "+rp.ID.Path()+".")
		default:
			h.internalError(w, r, err)
		}
		return
	}
	hdr := w.Header()
	hdr.Set("Cache-Control", "no-store")
	hdr.Set(headerXetCASURL, tok.CASURL)
	hdr.Set(headerXetToken, tok.AccessToken)
	hdr.Set(headerXetExpiration, strconv.FormatInt(tok.Exp, 10))
	httpx.WriteJSON(w, r, http.StatusOK, tok)
}
