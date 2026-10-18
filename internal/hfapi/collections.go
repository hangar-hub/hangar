package hfapi

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/hangar-hub/hangar/internal/collections"
	"github.com/hangar-hub/hangar/internal/httpx"
	"github.com/hangar-hub/hangar/internal/model"
)

// collectionsListMax bounds a Collection listing.
const collectionsListMax = 100

// serveCollections routes the Hub collections API (BROWSE-5):
// /api/collections, /api/collections/{ns}/{slug} and its items.
func (h *Handler) serveCollections(w http.ResponseWriter, r *http.Request, p reqPath) {
	switch {
	case p.len() == 0:
		h.route(w, r, "/api/collections", true, methods{http.MethodGet: h.listCollections, http.MethodPost: h.createCollection})
	case p.len() == 2:
		ns, slug := p.at(0), p.at(1)
		h.route(w, r, "/api/collections/{ns}/{slug}", true, methods{
			http.MethodGet:    func(w http.ResponseWriter, r *http.Request) { h.getCollection(w, r, ns, slug) },
			http.MethodPatch:  func(w http.ResponseWriter, r *http.Request) { h.updateCollection(w, r, ns, slug) },
			http.MethodDelete: func(w http.ResponseWriter, r *http.Request) { h.deleteCollection(w, r, ns, slug) },
		})
	case p.len() == 3 && p.at(2) == "items":
		ns, slug := p.at(0), p.at(1)
		h.route(w, r, "/api/collections/{ns}/{slug}/items", true, methods{http.MethodPost: func(w http.ResponseWriter, r *http.Request) { h.addCollectionItem(w, r, ns, slug) }})
	case p.len() == 4 && p.at(2) == "items":
		ns, slug, item := p.at(0), p.at(1), p.at(3)
		h.route(w, r, "/api/collections/{ns}/{slug}/items/{id}", true, methods{
			http.MethodPatch:  func(w http.ResponseWriter, r *http.Request) { h.updateCollectionItem(w, r, ns, slug, item) },
			http.MethodDelete: func(w http.ResponseWriter, r *http.Request) { h.deleteCollectionItem(w, r, ns, slug, item) },
		})
	default:
		h.notFound(w, r)
	}
}

func (h *Handler) collectionError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, collections.ErrNotFound):
		httpx.WriteHubError(w, r, &httpx.HubError{Status: http.StatusNotFound, Message: "Collection not found."})
	case errors.Is(err, collections.ErrForbidden):
		h.writeForbidden(w, r, "You cannot change this Collection.")
	case errors.Is(err, collections.ErrExists):
		httpx.WriteHubError(w, r, &httpx.HubError{Status: http.StatusConflict, Message: errMessage(err)})
	case errors.Is(err, collections.ErrInvalid):
		h.writeBadRequest(w, r, errMessage(err))
	default:
		h.internalError(w, r, err)
	}
}

// collectionJSON renders a Collection in the shape huggingface_hub's
// Collection and CollectionItem read.
func collectionJSON(c *collections.Collection) map[string]any {
	items := make([]map[string]any, 0, len(c.Items))
	for _, it := range c.Items {
		item := map[string]any{"_id": it.ID.String(), "id": it.ItemID, "type": it.Type, "position": it.Position}
		if it.Note != "" {
			item["note"] = map[string]any{"text": it.Note, "html": it.Note}
		}
		if it.Type == collections.ItemCollection {
			item["slug"] = it.ItemID
		}
		items = append(items, item)
	}
	theme := c.Theme
	if theme == "" {
		theme = "default"
	}
	return map[string]any{
		"slug": c.Namespace + "/" + c.Slug, "title": c.Title, "description": c.Description,
		"owner": map[string]any{"name": c.Namespace}, "items": items, "lastUpdated": model.NewHubTime(c.UpdatedAt),
		"position": 0, "private": c.Visibility.Private(), "theme": theme, "upvotes": 0, "isUpvotedByUser": false,
	}
}

func (h *Handler) listCollections(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	list, err := h.d.Collections.List(r.Context(), httpx.Principal(r.Context()), collections.ListOptions{
		Owner: q.Get("owner"), Item: q.Get("item"), Query: q.Get("q"),
		Limit: httpx.QueryInt(q, "limit", collectionsListMax, 1, collectionsListMax),
	})
	if err != nil {
		h.collectionError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for i := range list {
		// The client sets `position` from the list order itself and fails
		// on a second value.
		c := collectionJSON(&list[i])
		delete(c, "position")
		out = append(out, c)
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

func (h *Handler) getCollection(w http.ResponseWriter, r *http.Request, ns, slug string) {
	c, err := h.d.Collections.Get(r.Context(), httpx.Principal(r.Context()), ns, slug)
	if err != nil {
		h.collectionError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, collectionJSON(c))
}

func (h *Handler) createCollection(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := httpx.Principal(ctx)
	var req struct {
		Title       string `json:"title"`
		Namespace   string `json:"namespace"`
		Description string `json:"description"`
		Private     bool   `json:"private"`
	}
	if !h.decodeJSON(w, r, &req) {
		return
	}
	if p.IsAnonymous() {
		h.writeForbidden(w, r, "")
		return
	}
	params := collections.CreateParams{Namespace: orDefault(req.Namespace, p.Username), Title: req.Title, Description: req.Description, Visibility: model.VisibilityPublic}
	if req.Private {
		params.Visibility = model.VisibilityPrivate
	}
	c, err := h.d.Collections.Create(ctx, p, params)
	if errors.Is(err, collections.ErrExists) {
		// exists_ok reads the slug from the 409 body.
		params.ExistOK = true
		if existing, gerr := h.d.Collections.Create(ctx, p, params); gerr == nil {
			httpx.WriteHubError(w, r, &httpx.HubError{Status: http.StatusConflict, Message: "A Collection with this title already exists.", Extra: map[string]any{"slug": existing.Namespace + "/" + existing.Slug}})
			return
		}
	}
	if err != nil {
		h.collectionError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, collectionJSON(c))
}

func (h *Handler) updateCollection(w http.ResponseWriter, r *http.Request, ns, slug string) {
	var req struct {
		Title       *string `json:"title"`
		Description *string `json:"description"`
		Private     *bool   `json:"private"`
		Theme       *string `json:"theme"`
		Position    *int    `json:"position"`
	}
	if !h.decodeJSON(w, r, &req) {
		return
	}
	u := collections.Update{Title: req.Title, Description: req.Description, Theme: req.Theme}
	if req.Private != nil {
		v := model.VisibilityPublic
		if *req.Private {
			v = model.VisibilityPrivate
		}
		u.Visibility = &v
	}
	c, err := h.d.Collections.Update(r.Context(), httpx.Principal(r.Context()), ns, slug, u)
	if err != nil {
		h.collectionError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, map[string]any{"success": true, "data": collectionJSON(c)})
}

func (h *Handler) deleteCollection(w http.ResponseWriter, r *http.Request, ns, slug string) {
	if err := h.d.Collections.Delete(r.Context(), httpx.Principal(r.Context()), ns, slug); err != nil {
		h.collectionError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, map[string]any{})
}

func (h *Handler) addCollectionItem(w http.ResponseWriter, r *http.Request, ns, slug string) {
	var req struct {
		Item struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"item"`
		Note string `json:"note"`
	}
	if !h.decodeJSON(w, r, &req) {
		return
	}
	switch req.Item.Type {
	case collections.ItemModel, collections.ItemDataset, collections.ItemCollection:
	default:
		h.writeBadRequest(w, r, "Hangar Collections hold models, datasets and other Collections; "+req.Item.Type+" items are not supported.")
		return
	}
	c, err := h.d.Collections.AddItem(r.Context(), httpx.Principal(r.Context()), ns, slug, collections.ItemParams{Type: req.Item.Type, ItemID: req.Item.ID, Note: req.Note})
	if err != nil {
		h.collectionError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, collectionJSON(c))
}

func (h *Handler) updateCollectionItem(w http.ResponseWriter, r *http.Request, ns, slug, item string) {
	id, err := uuid.Parse(item)
	if err != nil {
		h.collectionError(w, r, collections.ErrNotFound)
		return
	}
	var req struct {
		Note     *string `json:"note"`
		Position *int    `json:"position"`
	}
	if !h.decodeJSON(w, r, &req) {
		return
	}
	if _, err := h.d.Collections.UpdateItem(r.Context(), httpx.Principal(r.Context()), ns, slug, id, collections.ItemUpdate{Note: req.Note, Position: req.Position}); err != nil {
		h.collectionError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, map[string]any{})
}

func (h *Handler) deleteCollectionItem(w http.ResponseWriter, r *http.Request, ns, slug, item string) {
	id, err := uuid.Parse(item)
	if err != nil {
		h.collectionError(w, r, collections.ErrNotFound)
		return
	}
	if err := h.d.Collections.DeleteItem(r.Context(), httpx.Principal(r.Context()), ns, slug, id); err != nil {
		h.collectionError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, map[string]any{})
}
