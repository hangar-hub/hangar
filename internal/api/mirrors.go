package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jamesagarside/hangar/internal/api/gen"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/upstream"
)

func (h *Upstreams) mirrorResource(ctx context.Context, p model.Principal, rp *repo.Repo, withUpdate bool) (gen.Mirror, error) {
	m, up, err := h.Service.Mirror(ctx, rp)
	if err != nil {
		return gen.Mirror{}, err
	}
	out := gen.Mirror{Upstream: up.Name, UpstreamRepo: m.UpstreamRepo.String(), Selection: upSelectionResource(m.Selection), Status: m.Status,
		Track: m.Track, LastSyncedAt: m.LastSyncedAt, UpstreamUrl: ptr(up.BaseURL + "/" + m.UpstreamRepo.Path())}
	if m.TrackRef != "" {
		out.TrackRef = ptr(m.TrackRef)
	}
	if m.Schedule != "" {
		out.Schedule = ptr(m.Schedule)
	}
	if !m.LastSyncedCommit.IsZero() {
		out.LastSyncedCommit = ptr(string(m.LastSyncedCommit))
	}
	if m.LastError != "" {
		out.LastError = ptr(m.LastError)
	}
	if withUpdate && m.DetachedAt == nil {
		uctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if avail, err := h.Service.UpdateAvailable(uctx, p, rp); err == nil {
			out.UpdateAvailable = ptr(avail)
		}
	}
	return out, nil
}

// GetMirror implements gen.ServerInterface: Mirror settings and state, and
// whether the Upstream has moved on (it asks the Upstream, briefly).
func (h *Upstreams) GetMirror(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name) {
	rp, p, ok := h.upLoadRepo(w, r, pType, ns, name)
	if !ok {
		return
	}
	out, err := h.mirrorResource(r.Context(), p, rp, true)
	if err != nil {
		h.upFail(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

// UpdateMirror implements gen.ServerInterface: selection, tracking and schedule.
func (h *Upstreams) UpdateMirror(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name) {
	rp, p, ok := h.upLoadRepo(w, r, pType, ns, name)
	if !ok {
		return
	}
	var body gen.MirrorUpdate
	if !upDecode(w, r, &body) {
		return
	}
	u := upstream.MirrorUpdate{Track: body.Track, TrackRef: body.TrackRef, Schedule: body.Schedule}
	if body.Selection != nil {
		sel := upSelection(body.Selection)
		u.Selection = &sel
	}
	if _, err := h.Service.UpdateMirror(r.Context(), p, rp, u); err != nil {
		h.upFail(w, r, err)
		return
	}
	out, err := h.mirrorResource(r.Context(), p, rp, false)
	if err != nil {
		h.upFail(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

// SyncMirror implements gen.ServerInterface ("Sync now").
func (h *Upstreams) SyncMirror(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name) {
	rp, p, ok := h.upLoadRepo(w, r, pType, ns, name)
	if !ok {
		return
	}
	id, err := h.Service.SyncMirror(r.Context(), p, rp)
	if err != nil {
		h.upFail(w, r, err)
		return
	}
	h.writeJob(w, r, http.StatusAccepted, id)
}

// DetachMirror implements gen.ServerInterface: the Mirror becomes an
// ordinary Repo that keeps its history.
func (h *Upstreams) DetachMirror(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name) {
	rp, p, ok := h.upLoadRepo(w, r, pType, ns, name)
	if !ok {
		return
	}
	if err := h.Service.Detach(r.Context(), p, rp); err != nil {
		h.upFail(w, r, err)
		return
	}
	rp, err := h.Repos.Get(r.Context(), rp.ID)
	if err != nil {
		h.upFail(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, upRepoResource(rp))
}

// PullMoreIntoMirror implements gen.ServerInterface.
func (h *Upstreams) PullMoreIntoMirror(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name) {
	rp, p, ok := h.upLoadRepo(w, r, pType, ns, name)
	if !ok {
		return
	}
	var body gen.Selection
	if !upDecode(w, r, &body) {
		return
	}
	id, err := h.Service.PullMore(r.Context(), p, rp, upSelection(&body), false)
	if err != nil {
		h.upFail(w, r, err)
		return
	}
	h.writeJob(w, r, http.StatusAccepted, id)
}

// upRepoResource is the native Repo shape for the detach answer. W2's
// repos.go has the full conversion; the integration lead can switch to it.
func upRepoResource(rp *repo.Repo) gen.Repo {
	out := gen.Repo{Id: rp.ID.String(), Namespace: rp.ID.Namespace, Name: rp.ID.Name, Type: gen.RepoTypeSingular(rp.ID.Type),
		Visibility: gen.Visibility(rp.Visibility), DefaultBranch: rp.DefaultBranch, Mirror: rp.Mirror, CreatedAt: rp.CreatedAt,
		UpdatedAt: rp.UpdatedAt, SizeBytes: rp.SizeBytes, LfsBytes: rp.LFSBytes, Uuid: ptr(rp.UUID), Gated: gen.RepoGated(rp.Gating.Mode)}
	tags := append([]string{}, rp.Tags...)
	out.Tags = &tags
	if !rp.LastModified.IsZero() {
		out.LastModified = ptr(rp.LastModified)
	}
	if rp.Description != "" {
		out.Description = ptr(rp.Description)
	}
	if rp.Licence != "" {
		out.Licence = ptr(rp.Licence)
	}
	return out
}

// UpstreamMirrorSummary returns the `mirror_status` of a Repo for the
// native Repo responses in repos.go (W2), or nil when the Repo is not a
// Mirror. It reads only the database: it never asks the Upstream.
func UpstreamMirrorSummary(ctx context.Context, svc *upstream.Service, rp *repo.Repo) (*gen.MirrorSummary, error) {
	m, up, err := svc.Mirror(ctx, rp)
	if errors.Is(err, upstream.ErrNotMirror) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if m.DetachedAt != nil {
		return nil, nil
	}
	return &gen.MirrorSummary{Upstream: up.Name, UpstreamRepo: m.UpstreamRepo.String(), Status: m.Status, LastSyncedAt: m.LastSyncedAt,
		UpstreamUrl: ptr(up.BaseURL + "/" + m.UpstreamRepo.Path())}, nil
}
