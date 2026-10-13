package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/jamesagarside/hangar/internal/api/gen"
	"github.com/jamesagarside/hangar/internal/audit"
	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/org"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/webhook"
)

// WebhooksAPI serves webhook registration and the delivery log (EVT-1).
// Namespace webhooks need the admin Role in the Namespace; Repo webhooks
// need admin on the Repo. Webhooks a caller cannot manage are not found.
type WebhooksAPI struct{ d MetaDeps }

// NewWebhooksAPI returns the webhook handlers. Webhooks is required.
func NewWebhooksAPI(d MetaDeps) (*WebhooksAPI, error) {
	if err := d.init(); err != nil {
		return nil, err
	}
	if d.Webhooks == nil {
		return nil, errors.New("api: Webhooks is required")
	}
	return &WebhooksAPI{d: d}, nil
}

// hookRepo parses `type/ns/name`, where type is models or datasets (or
// the singular).
func hookRepo(s string) (model.RepoID, error) {
	t, rest, ok := strings.Cut(s, "/")
	if !ok {
		return model.RepoID{}, fmt.Errorf("%w: repo must look like models/namespace/name", model.ErrInvalidRepoID)
	}
	rt, err := model.ParseRepoType(t)
	if err != nil || t == "" {
		return model.RepoID{}, fmt.Errorf("%w: repo must start with models/ or datasets/", model.ErrInvalidRepoID)
	}
	return model.ParseRepoID(rt, rest)
}

func (a *WebhooksAPI) manageNamespace(ctx context.Context, p model.Principal, ns *org.Namespace) error {
	err := a.d.Authz.Can(ctx, p, auth.ActionManageNamespace, auth.Target{Namespace: &auth.NamespaceTarget{ID: ns.ID, Name: ns.Name, Kind: ns.Kind}})
	if err != nil {
		return fmt.Errorf("%w: %v", auth.ErrForbidden, err)
	}
	return nil
}

func (a *WebhooksAPI) manageRepo(ctx context.Context, p model.Principal, rp *repo.Repo) error {
	if rp.DeletedAt != nil {
		return repo.ErrNotFound
	}
	if err := a.d.Authz.Can(ctx, p, auth.ActionRead, metaTarget(rp)); err != nil {
		return err
	}
	return a.d.Authz.Can(ctx, p, auth.ActionAdmin, metaTarget(rp))
}

// hookLoad returns a webhook the caller may manage, or a not-found error.
func (a *WebhooksAPI) hookLoad(r *http.Request, id uuid.UUID) (*webhook.Webhook, error) {
	ctx := r.Context()
	p := httpx.Principal(ctx)
	h, err := a.d.Webhooks.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	var check error
	switch {
	case h.RepoUUID != nil:
		rp, err := a.d.Repos.GetByUUID(ctx, *h.RepoUUID)
		if err != nil {
			return nil, webhook.ErrNotFound
		}
		check = a.manageRepo(ctx, p, rp)
	case h.NamespaceID != nil:
		ns, err := a.d.Orgs.GetByID(ctx, *h.NamespaceID)
		if err != nil {
			return nil, webhook.ErrNotFound
		}
		check = a.manageNamespace(ctx, p, ns)
	}
	if check != nil {
		return nil, fmt.Errorf("%w: %s", webhook.ErrNotFound, id)
	}
	return h, nil
}

func hookDelivery(d *webhook.Delivery, withPayload bool) gen.WebhookDelivery {
	out := gen.WebhookDelivery{Id: d.ID, Event: string(d.Event), Attempt: d.Attempt, CreatedAt: d.CreatedAt,
		StatusCode: d.StatusCode, DurationMs: d.DurationMS, NextAttemptAt: d.NextAttemptAt, DeliveredAt: d.DeliveredAt}
	if d.Error != "" {
		e := d.Error
		out.Error = &e
	}
	if withPayload {
		p := d.Payload
		out.Payload = &p
	}
	return out
}

func (a *WebhooksAPI) hookOut(ctx context.Context, h *webhook.Webhook) gen.Webhook {
	out := gen.Webhook{Id: h.ID, Url: h.URL, Enabled: h.Enabled, CreatedAt: h.CreatedAt, Events: make([]string, 0, len(h.Events))}
	for _, e := range h.Events {
		out.Events = append(out.Events, string(e))
	}
	if h.NamespaceID != nil {
		if ns, err := a.d.Orgs.GetByID(ctx, *h.NamespaceID); err == nil {
			out.Namespace = &ns.Name
		}
	}
	if h.RepoUUID != nil {
		if rp, err := a.d.Repos.GetByUUID(ctx, *h.RepoUUID); err == nil {
			s := rp.ID.Type.Plural() + "/" + rp.ID.String()
			out.Repo = &s
		}
	}
	if d, err := a.d.Webhooks.LastDelivery(ctx, h.ID); err == nil && d != nil {
		last := hookDelivery(d, false)
		out.LastDelivery = &last
	}
	return out
}

func (a *WebhooksAPI) record(r *http.Request, h *webhook.Webhook, change string) {
	if a.d.Audit == nil {
		return
	}
	data := map[string]any{"change": change, "url_host": hostOf(h.URL)}
	err := a.d.Audit.Record(r.Context(), audit.Event{Action: audit.WebhookChanged, Actor: httpx.Principal(r.Context()),
		TargetType: "webhook", TargetID: h.ID.String(), IP: httpx.ClientIP(r), UserAgent: r.UserAgent(), Data: data})
	if err != nil {
		a.d.Log.WarnContext(r.Context(), "record webhook audit event", "error", err.Error())
	}
}

// hostOf returns a URL's host, so audit data never holds a URL's query.
func hostOf(raw string) string {
	rest := raw
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

// ListWebhooks implements gen.ServerInterface. With ns or repo it lists
// that scope; with neither, the webhooks of every Namespace the caller
// administers and of the Repos in them.
func (a *WebhooksAPI) ListWebhooks(w http.ResponseWriter, r *http.Request, params gen.ListWebhooksParams) {
	ctx := r.Context()
	p := httpx.Principal(ctx)
	if p.IsAnonymous() {
		httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusUnauthorized, Detail: "Sign in to manage webhooks."})
		return
	}
	var scope webhook.ListOptions
	switch {
	case params.Repo != nil && *params.Repo != "":
		id, err := hookRepo(*params.Repo)
		if err != nil {
			metaBadRequest(w, r, "repo", err.Error())
			return
		}
		rp, err := a.d.Repos.Get(ctx, id)
		if err == nil {
			err = a.manageRepo(ctx, p, rp)
		}
		if err != nil {
			a.d.metaProblem(w, r, err)
			return
		}
		scope.RepoUUIDs = []uuid.UUID{rp.UUID}
	case params.Ns != nil && *params.Ns != "":
		ns, err := a.d.Orgs.Get(ctx, *params.Ns)
		if err == nil {
			err = a.manageNamespace(ctx, p, ns)
		}
		if err != nil {
			a.d.metaProblem(w, r, err)
			return
		}
		scope.NamespaceIDs = []uuid.UUID{ns.ID}
	default:
		names := []string{}
		if p.NamespaceID != uuid.Nil {
			scope.NamespaceIDs = append(scope.NamespaceIDs, p.NamespaceID)
			names = append(names, p.Username)
		}
		ms, err := a.d.Orgs.MembershipsOf(ctx, p.UserID)
		if err != nil {
			a.d.metaProblem(w, r, err)
			return
		}
		for _, m := range ms {
			if m.Role.Includes(model.RoleAdmin) {
				scope.NamespaceIDs = append(scope.NamespaceIDs, m.Org.ID)
				names = append(names, m.Org.Name)
			}
		}
		if len(names) > 0 {
			res, err := a.d.Repos.List(ctx, repo.ListOptions{Viewer: p, Namespaces: names, Limit: 500})
			if err != nil {
				a.d.metaProblem(w, r, err)
				return
			}
			for _, rp := range res.Repos {
				scope.RepoUUIDs = append(scope.RepoUUIDs, rp.UUID)
			}
		}
	}
	list, err := a.d.Webhooks.List(ctx, scope)
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	out := gen.WebhookList{Items: make([]gen.Webhook, 0, len(list))}
	for i := range list {
		out.Items = append(out.Items, a.hookOut(ctx, &list[i]))
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

// CreateWebhook implements gen.ServerInterface.
func (a *WebhooksAPI) CreateWebhook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := httpx.Principal(ctx)
	if p.IsAnonymous() {
		httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusUnauthorized, Detail: "Sign in to manage webhooks."})
		return
	}
	var body gen.WebhookCreate
	if !metaDecode(w, r, &body) {
		return
	}
	hasNS, hasRepo := body.Namespace != nil && *body.Namespace != "", body.Repo != nil && *body.Repo != ""
	if hasNS == hasRepo {
		metaBadRequest(w, r, "", "Give either namespace or repo.")
		return
	}
	cp := webhook.CreateParams{URL: body.Url, Secret: body.Secret, Events: body.Events, Enabled: true, CreatedBy: p.UserID}
	if body.Enabled != nil {
		cp.Enabled = *body.Enabled
	}
	if hasRepo {
		id, err := hookRepo(*body.Repo)
		if err != nil {
			metaBadRequest(w, r, "repo", err.Error())
			return
		}
		rp, err := a.d.Repos.Get(ctx, id)
		if err == nil {
			err = a.manageRepo(ctx, p, rp)
		}
		if err != nil {
			a.d.metaProblem(w, r, err)
			return
		}
		cp.RepoUUID = &rp.UUID
	} else {
		ns, err := a.d.Orgs.Get(ctx, *body.Namespace)
		if err == nil {
			err = a.manageNamespace(ctx, p, ns)
		}
		if err != nil {
			a.d.metaProblem(w, r, err)
			return
		}
		cp.NamespaceID = &ns.ID
	}
	h, err := a.d.Webhooks.Create(ctx, cp)
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	a.record(r, h, "created")
	httpx.WriteJSON(w, r, http.StatusCreated, a.hookOut(ctx, h))
}

// GetWebhook implements gen.ServerInterface.
func (a *WebhooksAPI) GetWebhook(w http.ResponseWriter, r *http.Request, id gen.ID) {
	h, err := a.hookLoad(r, id)
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, a.hookOut(r.Context(), h))
}

// UpdateWebhook implements gen.ServerInterface.
func (a *WebhooksAPI) UpdateWebhook(w http.ResponseWriter, r *http.Request, id gen.ID) {
	if _, err := a.hookLoad(r, id); err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	var body gen.WebhookUpdate
	if !metaDecode(w, r, &body) {
		return
	}
	h, err := a.d.Webhooks.Update(r.Context(), id, webhook.Update{URL: body.Url, Secret: body.Secret, Events: body.Events, Enabled: body.Enabled})
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	a.record(r, h, "updated")
	httpx.WriteJSON(w, r, http.StatusOK, a.hookOut(r.Context(), h))
}

// DeleteWebhook implements gen.ServerInterface.
func (a *WebhooksAPI) DeleteWebhook(w http.ResponseWriter, r *http.Request, id gen.ID) {
	h, err := a.hookLoad(r, id)
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	if err := a.d.Webhooks.Delete(r.Context(), id); err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	a.record(r, h, "deleted")
	w.WriteHeader(http.StatusNoContent)
}

// ListWebhookDeliveries implements gen.ServerInterface: the delivery log,
// newest first, with each payload.
func (a *WebhooksAPI) ListWebhookDeliveries(w http.ResponseWriter, r *http.Request, id gen.ID, params gen.ListWebhookDeliveriesParams) {
	limit, ok := metaLimit(params.Limit, 50)
	if !ok {
		metaBadRequest(w, r, "limit", "limit must be between 1 and 500.")
		return
	}
	if _, err := a.hookLoad(r, id); err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	cursor := ""
	if params.Cursor != nil {
		cursor = *params.Cursor
	}
	list, next, err := a.d.Webhooks.Deliveries(r.Context(), id, cursor, limit)
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	out := gen.WebhookDeliveryList{Items: make([]gen.WebhookDelivery, 0, len(list))}
	for i := range list {
		out.Items = append(out.Items, hookDelivery(&list[i], true))
	}
	if next != "" {
		out.NextCursor = &next
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

// RedeliverWebhook implements gen.ServerInterface.
func (a *WebhooksAPI) RedeliverWebhook(w http.ResponseWriter, r *http.Request, id gen.ID, deliveryID openapi_types.UUID) {
	if _, err := a.hookLoad(r, id); err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	d, err := a.d.Webhooks.Redeliver(r.Context(), id, deliveryID)
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusAccepted, hookDelivery(d, true))
}

// TestWebhook implements gen.ServerInterface: it queues a ping event.
func (a *WebhooksAPI) TestWebhook(w http.ResponseWriter, r *http.Request, id gen.ID) {
	if _, err := a.hookLoad(r, id); err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	d, err := a.d.Webhooks.Test(r.Context(), id, httpx.Principal(r.Context()))
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusAccepted, hookDelivery(d, true))
}
