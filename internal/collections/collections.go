// Package collections manages Collections: named, ordered lists of Repos
// that a Namespace curates, compatible with the Hub collections API
// (BROWSE-5). collectionstest has an in-memory fake.
package collections

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/model"
)

// Errors.
var (
	// ErrNotFound means the Collection or item does not exist, or the
	// caller cannot see it.
	ErrNotFound  = errors.New("collection not found")
	ErrForbidden = errors.New("forbidden")
	ErrExists    = errors.New("collection already exists")
	ErrInvalid   = errors.New("invalid request")
)

// Item types, as the Hub names them. Hangar stores models, datasets and
// other Collections.
const (
	ItemModel      = "model"
	ItemDataset    = "dataset"
	ItemCollection = "collection"
)

// Item is one entry in a Collection.
type Item struct {
	ID uuid.UUID
	// Type is model, dataset or collection.
	Type string
	// ItemID is a Repo ID (`ns/name`) or a Collection slug.
	ItemID   string
	Position int
	Note     string
}

// Collection is a Collection with its items in order.
type Collection struct {
	ID          uuid.UUID
	Namespace   string
	Slug        string
	Title       string
	Description string
	Visibility  model.Visibility
	Theme       string
	Items       []Item
	UpdatedAt   time.Time
}

// ListOptions filters Collections.
type ListOptions struct {
	Owner string
	// Item lists Collections containing an item, such as `models/ns/name`.
	Item  string
	Query string
	Limit int
}

// CreateParams describes a new Collection. The slug is derived from the title.
type CreateParams struct {
	Namespace   string
	Title       string
	Description string
	Visibility  model.Visibility
	// ExistOK returns the existing Collection with the same slug instead of ErrExists.
	ExistOK bool
}

// Update changes a Collection. Nil fields are unchanged.
type Update struct {
	Title       *string
	Description *string
	Visibility  *model.Visibility
	Theme       *string
}

// ItemParams adds an item.
type ItemParams struct {
	Type    string
	ItemID  string
	Note    string
	ExistOK bool
}

// ItemUpdate changes an item.
type ItemUpdate struct {
	Note     *string
	Position *int
}

// Service manages Collections. Every method applies visibility for the
// viewer or actor.
type Service interface {
	List(ctx context.Context, viewer model.Principal, opts ListOptions) ([]Collection, error)
	Get(ctx context.Context, viewer model.Principal, namespace, slug string) (*Collection, error)
	Create(ctx context.Context, actor model.Principal, p CreateParams) (*Collection, error)
	Update(ctx context.Context, actor model.Principal, namespace, slug string, u Update) (*Collection, error)
	Delete(ctx context.Context, actor model.Principal, namespace, slug string) error
	AddItem(ctx context.Context, actor model.Principal, namespace, slug string, p ItemParams) (*Collection, error)
	UpdateItem(ctx context.Context, actor model.Principal, namespace, slug string, itemID uuid.UUID, u ItemUpdate) (*Collection, error)
	DeleteItem(ctx context.Context, actor model.Principal, namespace, slug string, itemID uuid.UUID) error
}
