package repotest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/blob/blobtest"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/gitstore"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/org"
	"github.com/jamesagarside/hangar/internal/repo"
)

// Real is the real Repo service (repo.Store, gitstore and repo.LFS) on a
// test database, for other packages that want to test against it as well
// as against Fake. Repos need their Namespace in the database, so create
// users and Organisations with AddUser and AddOrg.
type Real struct {
	*repo.Store
	LFS   *repo.LFS
	Git   *gitstore.Store
	DB    *db.DB
	Blobs blob.Store
	// Namespaces resolves Namespaces from the database, standing in for
	// org.Service.
	Namespaces *DBNamespaces
}

// RealOptions change NewReal's defaults.
type RealOptions struct {
	// Blobs is the Blob store; nil means a new blobtest.Fake.
	Blobs blob.Store
	// Tweak changes the repo.Options before the Store is built.
	Tweak func(*repo.Options)
}

// NewReal builds the real Repo service on d, usually a database from
// dbtest.SQLite or dbtest.ForEachDriver, with its pack cache in a test
// temporary directory.
func NewReal(t testing.TB, d *db.DB, opts ...RealOptions) *Real {
	t.Helper()
	var o RealOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	blobs := o.Blobs
	if blobs == nil {
		blobs = blobtest.NewFake()
	}
	gs, err := gitstore.New(gitstore.Options{DB: d, Blobs: blobs, CacheDir: t.TempDir(), Timeout: time.Minute})
	if err != nil {
		t.Fatalf("gitstore: %v", err)
	}
	lfs := repo.NewLFS(d, blobs)
	ns := &DBNamespaces{DB: d}
	ro := repo.Options{DB: d, Git: gs, LFS: lfs, Namespaces: ns}
	if o.Tweak != nil {
		o.Tweak(&ro)
	}
	svc, err := repo.NewStore(ro)
	if err != nil {
		t.Fatalf("repo: %v", err)
	}
	return &Real{Store: svc, LFS: lfs, Git: gs, DB: d, Blobs: blobs, Namespaces: ns}
}

// AddUser inserts a user and their personal Namespace and returns a
// Principal for them.
func (r *Real) AddUser(t testing.TB, username string) model.Principal {
	t.Helper()
	ctx := context.Background()
	ns := &db.Namespace{ID: db.NewID(), Name: username, Kind: org.KindUser, DisplayName: username}
	if _, err := r.DB.NewInsert().Model(ns).Exec(ctx); err != nil {
		t.Fatalf("add namespace %s: %v", username, err)
	}
	email := strings.ToLower(username) + "@example.com"
	u := &db.User{ID: db.NewID(), NamespaceID: ns.ID, Email: &email}
	if _, err := r.DB.NewInsert().Model(u).Exec(ctx); err != nil {
		t.Fatalf("add user %s: %v", username, err)
	}
	return model.Principal{Kind: model.PrincipalUser, UserID: u.ID, NamespaceID: ns.ID, Username: username,
		DisplayName: username, Email: email, Method: model.AuthSession}
}

// AddOrg inserts an Organisation with members at the given Roles.
func (r *Real) AddOrg(t testing.TB, name string, members map[uuid.UUID]model.Role) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	ns := &db.Namespace{ID: db.NewID(), Name: name, Kind: org.KindOrg, DisplayName: name}
	if _, err := r.DB.NewInsert().Model(ns).Exec(ctx); err != nil {
		t.Fatalf("add org %s: %v", name, err)
	}
	for userID, role := range members {
		m := &db.Membership{ID: db.NewID(), OrgNamespaceID: ns.ID, UserID: userID, Role: string(role), Source: org.SourceManual}
		if _, err := r.DB.NewInsert().Model(m).Exec(ctx); err != nil {
			t.Fatalf("add member: %v", err)
		}
	}
	return ns.ID
}

// DBNamespaces implements repo.Namespaces over the namespaces, users and
// memberships tables, standing in for org.Service in tests.
type DBNamespaces struct{ DB *db.DB }

var _ repo.Namespaces = (*DBNamespaces)(nil)

func (n *DBNamespaces) load(ctx context.Context, row *db.Namespace) *org.Namespace {
	ns := &org.Namespace{ID: row.ID, Name: row.Name, Kind: row.Kind, DisplayName: row.DisplayName, Description: row.Description,
		ReservedMirror: row.ReservedMirror, CreatedAt: row.CreatedAt}
	var u db.User
	if err := n.DB.NewSelect().Model(&u).Where("namespace_id = ?", row.ID).Scan(ctx); err == nil {
		ns.UserID = u.ID
	}
	return ns
}

// Get implements repo.Namespaces.
func (n *DBNamespaces) Get(ctx context.Context, name string) (*org.Namespace, error) {
	row := new(db.Namespace)
	if err := n.DB.NewSelect().Model(row).Where("lower(name) = lower(?)", name).Scan(ctx); err != nil {
		if db.IsNotFound(err) {
			return nil, org.ErrNotFound
		}
		return nil, err
	}
	return n.load(ctx, row), nil
}

// GetByID implements repo.Namespaces.
func (n *DBNamespaces) GetByID(ctx context.Context, id uuid.UUID) (*org.Namespace, error) {
	row := new(db.Namespace)
	if err := n.DB.NewSelect().Model(row).Where("id = ?", id).Scan(ctx); err != nil {
		if db.IsNotFound(err) {
			return nil, org.ErrNotFound
		}
		return nil, err
	}
	return n.load(ctx, row), nil
}

// MembershipsOf implements repo.Namespaces.
func (n *DBNamespaces) MembershipsOf(ctx context.Context, userID uuid.UUID) ([]org.Membership, error) {
	var ms []db.Membership
	if err := n.DB.NewSelect().Model(&ms).Where("user_id = ?", userID).Scan(ctx); err != nil {
		return nil, err
	}
	out := make([]org.Membership, 0, len(ms))
	for _, m := range ms {
		ns, err := n.GetByID(ctx, m.OrgNamespaceID)
		if err != nil {
			return nil, err
		}
		out = append(out, org.Membership{Org: *ns, Role: model.Role(m.Role)})
	}
	return out, nil
}
