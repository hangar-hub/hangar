package datasetstest

import (
	"context"
	"testing"

	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/repo"
)

// InsertRepoRow records a Repo of a repotest.Fake in the database, with a
// Namespace, so rows that reference repos(id) can be written in tests
// that do not use the real Repo service.
func InsertRepoRow(t testing.TB, d *db.DB, r *repo.Repo) {
	t.Helper()
	ctx := context.Background()
	ns := &db.Namespace{ID: db.NewID(), Name: r.ID.Namespace + "-" + r.UUID.String(), Kind: "org"}
	if _, err := d.NewInsert().Model(ns).Exec(ctx); err != nil {
		t.Fatalf("insert namespace: %v", err)
	}
	row := &db.Repo{ID: r.UUID, NamespaceID: ns.ID, Name: r.ID.Name, Type: string(r.ID.Type), Visibility: string(r.Visibility),
		DefaultBranch: r.DefaultBranch, HeadRef: r.HeadRef, Tags: []string{}, Settings: map[string]any{}, Gating: map[string]any{}}
	if _, err := d.NewInsert().Model(row).Exec(ctx); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
}
