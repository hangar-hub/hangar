package repo_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/audit"
	"github.com/jamesagarside/hangar/internal/audit/audittest"
	"github.com/jamesagarside/hangar/internal/blob/blobtest"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/gitstore"
	"github.com/jamesagarside/hangar/internal/jobs"
	"github.com/jamesagarside/hangar/internal/jobs/jobstest"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/org"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/webhook/webhooktest"
)

// dbNamespaces is a repo.Namespaces over the namespaces, users and
// memberships tables, for tests. In Hangar, org.Service fills this role.
type dbNamespaces struct{ d *db.DB }

func (n dbNamespaces) get(ctx context.Context, q func(*db.Namespace) error) (*org.Namespace, error) {
	row := new(db.Namespace)
	if err := q(row); err != nil {
		if db.IsNotFound(err) {
			return nil, org.ErrNotFound
		}
		return nil, err
	}
	ns := &org.Namespace{ID: row.ID, Name: row.Name, Kind: row.Kind, DisplayName: row.DisplayName, CreatedAt: row.CreatedAt}
	var u db.User
	if err := n.d.NewSelect().Model(&u).Where("namespace_id = ?", row.ID).Scan(ctx); err == nil {
		ns.UserID = u.ID
	}
	return ns, nil
}

func (n dbNamespaces) Get(ctx context.Context, name string) (*org.Namespace, error) {
	return n.get(ctx, func(row *db.Namespace) error {
		return n.d.NewSelect().Model(row).Where("lower(name) = lower(?)", name).Scan(ctx)
	})
}

func (n dbNamespaces) GetByID(ctx context.Context, id uuid.UUID) (*org.Namespace, error) {
	return n.get(ctx, func(row *db.Namespace) error { return n.d.NewSelect().Model(row).Where("id = ?", id).Scan(ctx) })
}

func (n dbNamespaces) MembershipsOf(ctx context.Context, userID uuid.UUID) ([]org.Membership, error) {
	var ms []db.Membership
	if err := n.d.NewSelect().Model(&ms).Where("user_id = ?", userID).Scan(ctx); err != nil {
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

// env is a Repo service on a fresh database with fakes around it.
type env struct {
	t       *testing.T
	ctx     context.Context
	d       *db.DB
	blobs   *blobtest.Fake
	git     *gitstore.Store
	lfs     *repo.LFS
	svc     *repo.Store
	jobs    *jobstest.Fake
	reg     *jobs.Registry
	audit   *audittest.Fake
	hooks   *webhooktest.Fake
	clock   *clock
	opts    repo.Options
	nsNames dbNamespaces
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

var start = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

func newEnv(t *testing.T, d *db.DB, tweak ...func(*repo.Options)) *env {
	t.Helper()
	blobs := blobtest.NewFake()
	gs, err := gitstore.New(gitstore.Options{DB: d, Blobs: blobs, CacheDir: t.TempDir(), Timeout: time.Minute})
	require.NoError(t, err)
	lfs := repo.NewLFS(d, blobs)
	reg := jobs.NewRegistry()
	q := jobstest.NewFake(reg)
	c := &clock{now: start}
	e := &env{t: t, ctx: context.Background(), d: d, blobs: blobs, git: gs, lfs: lfs, jobs: q, reg: reg,
		audit: audittest.NewFake(), hooks: &webhooktest.Fake{}, clock: c, nsNames: dbNamespaces{d}}
	e.opts = repo.Options{DB: d, Git: gs, LFS: lfs, Namespaces: e.nsNames, Jobs: q, Webhooks: e.hooks, Audit: e.audit, Now: c.Now}
	for _, f := range tweak {
		f(&e.opts)
	}
	e.svc, err = repo.NewStore(e.opts)
	require.NoError(t, err)
	return e
}

// forEach runs fn against SQLite and, when enabled, Postgres.
func forEach(t *testing.T, fn func(t *testing.T, e *env), tweak ...func(*repo.Options)) {
	t.Parallel()
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) { fn(t, newEnv(t, d, tweak...)) })
}

// addUser creates a user with a personal Namespace and returns them.
func (e *env) addUser(name string) model.Principal {
	e.t.Helper()
	ns := &db.Namespace{ID: db.NewID(), Name: name, Kind: org.KindUser, DisplayName: strings.ToUpper(name[:1]) + name[1:]}
	_, err := e.d.NewInsert().Model(ns).Exec(e.ctx)
	require.NoError(e.t, err)
	email := name + "@example.com"
	u := &db.User{ID: db.NewID(), NamespaceID: ns.ID, Email: &email}
	_, err = e.d.NewInsert().Model(u).Exec(e.ctx)
	require.NoError(e.t, err)
	return model.Principal{Kind: model.PrincipalUser, UserID: u.ID, NamespaceID: ns.ID, Username: name, DisplayName: ns.DisplayName, Email: email, Method: model.AuthSession}
}

// auditEvents returns the recorded audit events with one action, in order.
func (e *env) auditEvents(action audit.Action) []audit.Event {
	var out []audit.Event
	for _, ev := range e.audit.Events() {
		if ev.Action == action {
			out = append(out, ev)
		}
	}
	return out
}

func (e *env) create(p repo.CreateParams) *repo.Repo {
	e.t.Helper()
	r, err := e.svc.Create(e.ctx, p)
	require.NoError(e.t, err)
	return r
}

func (e *env) commit(id model.RepoID, actor model.Principal, ops ...repo.Operation) *repo.CommitResult {
	e.t.Helper()
	res, err := e.svc.Commit(e.ctx, repo.CommitParams{Repo: id, Summary: "update", Operations: ops, Actor: actor})
	require.NoError(e.t, err)
	return res
}

func add(path, content string) repo.Operation {
	return repo.Operation{Kind: repo.OpAdd, Path: path, Content: strings.NewReader(content)}
}

// putLFS stores content as an LFS object uploaded by uploaders, whose
// Commits may then add pointers to it.
func (e *env) putLFS(content []byte, uploaders ...model.Principal) string {
	e.t.Helper()
	sum := sha256.Sum256(content)
	oid := hex.EncodeToString(sum[:])
	_, err := e.lfs.Ingest(e.ctx, oid, int64(len(content)), bytes.NewReader(content))
	require.NoError(e.t, err)
	for _, u := range uploaders {
		require.NoError(e.t, e.lfs.RecordUploader(e.ctx, oid, u.UserID))
	}
	return oid
}

func (e *env) read(id model.RepoID, rev, path string) string {
	e.t.Helper()
	sha, err := e.svc.Resolve(e.ctx, id, rev)
	require.NoError(e.t, err)
	rc, _, err := e.svc.OpenFile(e.ctx, id, sha, path, nil)
	require.NoError(e.t, err)
	defer rc.Close()
	b, err := io.ReadAll(rc)
	require.NoError(e.t, err)
	return string(b)
}

// export clones a Repo's view into a plain bare repository on disk, the
// "exported copy" that git fsck and git cat-file check.
func (e *env) export(id model.RepoID) string {
	e.t.Helper()
	v, err := e.svc.OpenView(e.ctx, id)
	require.NoError(e.t, err)
	defer v.Close()
	dst := filepath.Join(e.t.TempDir(), "export.git")
	gitCmd(e.t, "", "clone", "--quiet", "--mirror", v.Dir, dst)
	return dst
}

func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitRun(dir, nil, args...)
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(out)
}

func gitRun(dir string, stdin io.Reader, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdin = stdin
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Tester", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=Tester", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
