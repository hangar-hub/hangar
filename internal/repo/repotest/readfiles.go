package repotest

import (
	"context"
	"errors"

	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
)

// CanReadFilesWith answers repo.Options.CanReadFiles and
// Fake.CanReadFiles with az's read_files permission, as the server wires
// it, so tests can use authtest.Authz.
func CanReadFilesWith(az auth.Authz) func(context.Context, model.Principal, *repo.Repo) (bool, error) {
	return func(ctx context.Context, p model.Principal, r *repo.Repo) (bool, error) {
		err := az.Can(ctx, p, auth.ActionReadFiles, auth.Target{Repo: &auth.RepoTarget{ID: r.ID, UUID: r.UUID,
			NamespaceID: r.NamespaceID, Visibility: r.Visibility, Gated: r.Gating.Enabled(), Mirror: r.Mirror}})
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, auth.ErrNotFound), errors.Is(err, auth.ErrForbidden), errors.Is(err, auth.ErrGated):
			return false, nil
		}
		return false, err
	}
}
