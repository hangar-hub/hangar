package collectionstest_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/collections"
	"github.com/jamesagarside/hangar/internal/collections/collectionstest"
	"github.com/jamesagarside/hangar/internal/model"
)

func TestFakeCollections(t *testing.T) {
	ctx := context.Background()
	f := collectionstest.NewFake()
	alice := model.Principal{Kind: model.PrincipalUser, Username: "alice"}
	bob := model.Principal{Kind: model.PrincipalUser, Username: "bob"}

	c, err := f.Create(ctx, alice, collections.CreateParams{Namespace: "alice", Title: "Small GGUF models!"})
	require.NoError(t, err)
	require.Equal(t, "small-gguf-models", c.Slug)
	_, err = f.Create(ctx, alice, collections.CreateParams{Namespace: "alice", Title: "small gguf models"})
	require.ErrorIs(t, err, collections.ErrExists)
	again, err := f.Create(ctx, alice, collections.CreateParams{Namespace: "alice", Title: "small gguf models", ExistOK: true})
	require.NoError(t, err)
	require.Equal(t, c.ID, again.ID)
	_, err = f.Create(ctx, bob, collections.CreateParams{Namespace: "alice", Title: "x"})
	require.ErrorIs(t, err, collections.ErrForbidden)
	_, err = f.Create(ctx, alice, collections.CreateParams{Namespace: "alice", Title: "!!!"})
	require.ErrorIs(t, err, collections.ErrInvalid)

	c, err = f.AddItem(ctx, alice, "alice", c.Slug, collections.ItemParams{Type: collections.ItemModel, ItemID: "bartowski/a"})
	require.NoError(t, err)
	c, err = f.AddItem(ctx, alice, "alice", c.Slug, collections.ItemParams{Type: collections.ItemDataset, ItemID: "ns/d", Note: "eval set"})
	require.NoError(t, err)
	require.Len(t, c.Items, 2)
	_, err = f.AddItem(ctx, alice, "alice", c.Slug, collections.ItemParams{Type: collections.ItemModel, ItemID: "bartowski/a"})
	require.ErrorIs(t, err, collections.ErrExists)
	_, err = f.AddItem(ctx, alice, "alice", c.Slug, collections.ItemParams{Type: "space", ItemID: "x/y"})
	require.ErrorIs(t, err, collections.ErrInvalid)
	_, err = f.AddItem(ctx, bob, "alice", c.Slug, collections.ItemParams{Type: collections.ItemModel, ItemID: "x/y"})
	require.ErrorIs(t, err, collections.ErrForbidden)

	second := c.Items[1].ID
	zero := 0
	c, err = f.UpdateItem(ctx, alice, "alice", c.Slug, second, collections.ItemUpdate{Position: &zero})
	require.NoError(t, err)
	require.Equal(t, second, c.Items[0].ID, "items reorder")
	require.Equal(t, 1, c.Items[1].Position)
	_, err = f.UpdateItem(ctx, alice, "alice", c.Slug, uuid.New(), collections.ItemUpdate{})
	require.ErrorIs(t, err, collections.ErrNotFound)

	containing, err := f.List(ctx, bob, collections.ListOptions{Item: "models/bartowski/a"})
	require.NoError(t, err)
	require.Len(t, containing, 1)
	byQuery, err := f.List(ctx, bob, collections.ListOptions{Query: "GGUF", Owner: "alice", Limit: 5})
	require.NoError(t, err)
	require.Len(t, byQuery, 1)

	priv := model.VisibilityPrivate
	_, err = f.Update(ctx, alice, "alice", c.Slug, collections.Update{Visibility: &priv})
	require.NoError(t, err)
	_, err = f.Get(ctx, bob, "alice", c.Slug)
	require.ErrorIs(t, err, collections.ErrNotFound, "private Collections are hidden")
	got, err := f.Get(ctx, alice, "ALICE", c.Slug)
	require.NoError(t, err)
	require.Equal(t, priv, got.Visibility)
	bad := model.Visibility("secret")
	_, err = f.Update(ctx, alice, "alice", c.Slug, collections.Update{Visibility: &bad})
	require.ErrorIs(t, err, collections.ErrInvalid)

	require.NoError(t, f.DeleteItem(ctx, alice, "alice", c.Slug, second))
	require.ErrorIs(t, f.DeleteItem(ctx, alice, "alice", c.Slug, second), collections.ErrNotFound)
	require.NoError(t, f.Delete(ctx, alice, "alice", c.Slug))
	require.ErrorIs(t, f.Delete(ctx, alice, "alice", c.Slug), collections.ErrNotFound)
	require.Equal(t, "a-b", collectionstest.Slugify("  A & B  "))
}
