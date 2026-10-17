package repo_test

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
)

// failedQueries records the statements the database refused.
type failedQueries struct {
	mu   sync.Mutex
	errs []error
}

func (f *failedQueries) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context {
	return ctx
}

func (f *failedQueries) AfterQuery(_ context.Context, ev *bun.QueryEvent) {
	if ev.Err != nil && db.IsUniqueViolation(ev.Err) {
		f.mu.Lock()
		f.errs = append(f.errs, ev.Err)
		f.mu.Unlock()
	}
}

// TestCreateExistingRepoCausesNoDatabaseError is the regression test for
// bellatrix finding 6: huggingface_hub's create_repo(exist_ok=True) asks
// to create a Repo that exists on every upload, and each request made the
// insert fail with a unique violation, an ERROR line in the Postgres log.
func TestCreateExistingRepoCausesNoDatabaseError(t *testing.T) {
	t.Parallel()
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		hook := &failedQueries{}
		hooked := *d
		hooked.DB = d.WithQueryHook(hook)
		e := newEnv(t, &hooked)
		alice := e.addUser("alice")
		id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "again"}
		e.create(repo.CreateParams{ID: id, Actor: alice})
		for _, name := range []string{"again", "AGAIN"} {
			_, err := e.svc.Create(e.ctx, repo.CreateParams{ID: model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: name}, Actor: alice})
			require.ErrorIs(t, err, repo.ErrExists)
		}
		hook.mu.Lock()
		defer hook.mu.Unlock()
		require.Empty(t, hook.errs, "no statement failed")
	})
}
