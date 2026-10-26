package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/jobs"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
)

// adminEnv is a migrated SQLite database and filesystem Blob store, and
// the --set flags that point the admin commands at them.
type adminEnv struct {
	t   *testing.T
	set []string
}

func newAdminEnv(t *testing.T) *adminEnv {
	t.Helper()
	dir := t.TempDir()
	set := []string{
		"--set", "server.public_url=http://localhost:8080",
		"--set", "server.data_dir=" + dir,
		"--set", "database.dsn=" + filepath.Join(dir, "hangar.db"),
		"--set", "storage.fs.path=" + filepath.Join(dir, "blobs"),
		"--set", "git.cache_dir=" + filepath.Join(dir, "git-cache"),
		"--set", "telemetry.log_format=text",
	}
	e := &adminEnv{t: t, set: set}
	_, code := e.run("", "admin", "migrate")
	require.Equal(t, 0, code)
	return e
}

// run executes a command with stdin and returns its output and exit code.
func (e *adminEnv) run(stdin string, args ...string) (string, int) {
	e.t.Helper()
	root := newRootCommand()
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(append(args, e.set...))
	if err := root.Execute(); err != nil {
		return out.String() + errOut.String() + err.Error(), 1
	}
	return out.String(), 0
}

func (e *adminEnv) json(v any, stdin string, args ...string) {
	e.t.Helper()
	out, code := e.run(stdin, append(args, "--json")...)
	require.Equal(e.t, 0, code, out)
	require.NoError(e.t, json.Unmarshal([]byte(out), v), out)
}

// with opens the Instance the way the admin commands do, runs fn and
// closes it again, releasing the SQLite lock.
func (e *adminEnv) with(fn func(in *instance)) {
	e.t.Helper()
	var f configFlags
	for i := 1; i < len(e.set); i += 2 {
		f.set = append(f.set, e.set[i])
	}
	in, done, err := adminInstance(context.Background(), &f, io.Discard)
	require.NoError(e.t, err)
	defer done()
	fn(in)
}

func TestAdminCreateUserAndResetPassword(t *testing.T) {
	e := newAdminEnv(t)
	var created userResult
	e.json(&created, "", "admin", "create-user", "alice", "--admin", "--email", "alice@example.com")
	require.Equal(t, "alice", created.Username)
	require.True(t, created.IsAdmin)
	require.Len(t, created.Password, 24, "a generated password is printed once")

	out, code := e.run("", "admin", "create-user", "alice")
	require.Equal(t, 1, code)
	require.Contains(t, out, "taken")
	out, code = e.run("", "admin", "create-user", "bob", "--password-stdin")
	require.Equal(t, 1, code, "an empty password from standard input is refused")
	require.Contains(t, out, "empty password")
	out, code = e.run("short\n", "admin", "create-user", "bob", "--password-stdin")
	require.Equal(t, 1, code, "the password policy applies")
	require.Contains(t, out, "characters")
	out, code = e.run("a long enough password 7\n", "admin", "create-user", "bob", "--password-stdin")
	require.Equal(t, 0, code, out)
	require.Contains(t, out, "Created the user bob.")
	require.NotContains(t, out, "Password:", "a password from standard input is not echoed")

	var reset userResult
	e.json(&reset, "", "admin", "reset-password", "bob")
	require.Len(t, reset.Password, 24)
	out, code = e.run("", "admin", "reset-password", "nobody")
	require.Equal(t, 1, code)
	require.Contains(t, out, "nobody")
	out, code = e.run("another long password 8\n", "admin", "reset-password", "alice", "--password-stdin")
	require.Equal(t, 0, code, out)

	e.with(func(in *instance) {
		ctx := context.Background()
		for name, pw := range map[string]string{"alice": "another long password 8", "bob": reset.Password} {
			_, err := in.auth.Login.Password(ctx, auth.LoginAttempt{Username: name, Password: pw, IP: "127.0.0.1"})
			require.NoError(t, err, name)
		}
		_, err := in.auth.Login.Password(ctx, auth.LoginAttempt{Username: "alice", Password: created.Password, IP: "127.0.0.1"})
		require.Error(t, err, "the old password no longer works")
	})
}

// TestAdminUnlockSignIn covers hangar admin unlock-sign-in (security
// review finding 11): it clears the throttle of a username, an IP address
// or everything, so a locked-out admin signs in again without a new
// password.
func TestAdminUnlockSignIn(t *testing.T) {
	e := newAdminEnv(t)
	var created userResult
	e.json(&created, "", "admin", "create-user", "hangar-admin", "--admin")
	lock := func(ip string) {
		e.with(func(in *instance) {
			ctx := context.Background()
			for range 5 {
				_, err := in.auth.Login.Password(ctx, auth.LoginAttempt{Username: "hangar-admin", Password: "wrong password 1", IP: ip})
				require.ErrorIs(t, err, auth.ErrLoginFailed)
			}
			// The fifth failure locks for a second, less than a password
			// hash takes on a slow machine, so hold the locks for an hour.
			_, err := in.db.NewUpdate().Model((*db.LoginAttempt)(nil)).
				Set("locked_until = ?", time.Now().UTC().Add(time.Hour)).
				Where("locked_until IS NOT NULL").Exec(ctx)
			require.NoError(t, err)
		})
	}
	signIn := func(ip string) error {
		var err error
		e.with(func(in *instance) {
			_, err = in.auth.Login.Password(context.Background(), auth.LoginAttempt{Username: "hangar-admin", Password: created.Password, IP: ip})
		})
		return err
	}
	var te *auth.ThrottledError

	lock("203.0.113.9")
	require.ErrorAs(t, signIn("198.51.100.1"), &te)
	out, code := e.run("", "admin", "unlock-sign-in")
	require.Equal(t, 1, code)
	require.Contains(t, out, "name a username")
	out, code = e.run("", "admin", "unlock-sign-in", "hangar-admin", "--all")
	require.Equal(t, 1, code)
	require.Contains(t, out, "--all clears every counter")
	out, code = e.run("", "admin", "unlock-sign-in", "Hangar-Admin")
	require.Equal(t, 0, code, out)
	require.Contains(t, out, "Cleared 1 sign-in throttle counters.")
	require.NoError(t, signIn("198.51.100.1"))

	require.ErrorAs(t, signIn("203.0.113.9"), &te, "the address that sent the guesses is still locked")
	var res unlockResult
	e.json(&res, "", "admin", "unlock-sign-in", "--ip", "203.0.113.9")
	require.Equal(t, int64(1), res.Removed)
	require.NoError(t, signIn("203.0.113.9"))

	lock("203.0.113.10")
	e.json(&res, "", "admin", "unlock-sign-in", "--all")
	require.Equal(t, int64(2), res.Removed, "the username's and the address's counters")
	out, code = e.run("", "admin", "unlock-sign-in", "--all")
	require.Equal(t, 0, code, out)
	require.Contains(t, out, "Nothing to clear")
}

func TestAdminGC(t *testing.T) {
	e := newAdminEnv(t)
	var res gcResult
	e.json(&res, "", "admin", "gc")
	require.NotNil(t, res.Report)
	require.Equal(t, "all", res.Report.Scope)
	e.json(&res, "", "admin", "gc", "--git")
	require.Equal(t, "git", res.Report.Scope)
	out, code := e.run("", "admin", "gc", "--git", "--lfs")
	require.Equal(t, 1, code)
	require.Contains(t, out, "cannot be combined")
	out, code = e.run("", "admin", "gc", "--lfs")
	require.Equal(t, 0, code, out)
	require.Contains(t, out, "Garbage collection (lfs) finished.")

	var queued gcResult
	e.json(&queued, "", "admin", "gc", "--queue")
	require.NotEmpty(t, queued.JobID)
	require.Nil(t, queued.Report)
	e.with(func(in *instance) {
		js, _, err := in.queue.List(context.Background(), jobs.Filter{Kinds: []jobs.Kind{jobs.KindGC}})
		require.NoError(t, err)
		require.Len(t, js, 1)
		require.Equal(t, queued.JobID, js[0].ID.String())
	})
}

func TestAdminReindex(t *testing.T) {
	e := newAdminEnv(t)
	var created userResult
	e.json(&created, "", "admin", "create-user", "alice")
	ctx := context.Background()
	e.with(func(in *instance) {
		u, err := in.auth.Users.GetByUsername(ctx, "alice")
		require.NoError(t, err)
		p := u.Principal(model.AuthSession)
		for _, id := range []model.RepoID{{Type: model.RepoTypeModel, Namespace: "alice", Name: "m"}, {Type: model.RepoTypeDataset, Namespace: "alice", Name: "d"}} {
			_, err := in.repos.Create(ctx, repo.CreateParams{ID: id, Actor: p, InitialReadme: true, Visibility: model.VisibilityPublic})
			require.NoError(t, err)
		}
		// Creating the Repos queued their first index and scan Jobs; clear
		// them so the counts below are reindex's own.
		js, _, err := in.queue.List(ctx, jobs.Filter{})
		require.NoError(t, err)
		for _, j := range js {
			require.NoError(t, in.queue.Cancel(ctx, j.ID))
		}
	})

	var all reindexResult
	e.json(&all, "", "admin", "reindex")
	require.Equal(t, reindexResult{IndexJobs: 2, ScanJobs: 2}, all)
	var one reindexResult
	e.json(&one, "", "admin", "reindex", "datasets/alice/d")
	require.Equal(t, reindexResult{IndexJobs: 1, ScanJobs: 1}, one, "the dataset's Jobs join the pending ones")
	var stale reindexResult
	e.json(&stale, "", "admin", "reindex", "--stale")
	require.Equal(t, reindexResult{}, stale, "nothing was indexed by an older version")
	out, code := e.run("", "admin", "reindex", "alice/missing")
	require.Equal(t, 1, code)
	require.Contains(t, out, "alice/missing")
	out, code = e.run("", "admin", "reindex", "--stale", "alice/m")
	require.Equal(t, 1, code)
	require.Contains(t, out, "--stale")

	e.with(func(in *instance) {
		js, _, err := in.queue.List(ctx, jobs.Filter{Kinds: []jobs.Kind{jobs.KindIndex}, Statuses: []jobs.Status{jobs.StatusPending}})
		require.NoError(t, err)
		require.Len(t, js, 2)
		for _, j := range js {
			require.NotEqual(t, [16]byte{}, [16]byte(j.Repo), "reindex Jobs are scoped to their Repo")
		}
	})
}

func TestParseAdminRepoID(t *testing.T) {
	for in, want := range map[[2]string]model.RepoID{
		{"alice/m", ""}:               {Type: model.RepoTypeModel, Namespace: "alice", Name: "m"},
		{"models/alice/m", "dataset"}: {Type: model.RepoTypeModel, Namespace: "alice", Name: "m"},
		{"datasets/alice/d", ""}:      {Type: model.RepoTypeDataset, Namespace: "alice", Name: "d"},
		{"alice/d", "dataset"}:        {Type: model.RepoTypeDataset, Namespace: "alice", Name: "d"},
		{"alice/m", "model"}:          {Type: model.RepoTypeModel, Namespace: "alice", Name: "m"},
		{"datasets/alice/d", "model"}: {Type: model.RepoTypeDataset, Namespace: "alice", Name: "d"},
	} {
		got, err := parseAdminRepoID(in[0], in[1])
		require.NoError(t, err, in)
		require.Equal(t, want, got, in)
	}
	_, err := parseAdminRepoID("alice/m", "space")
	require.ErrorContains(t, err, "--type must be model or dataset")
	_, err = parseAdminRepoID("no-slash", "")
	require.Error(t, err)

	out, code := newAdminEnv(t).run("", "admin", "reindex", "--type", "space", "alice/m")
	require.Equal(t, 1, code)
	require.Contains(t, out, "--type must be model or dataset")
}

func TestAdminCommandsRefuseBadSetups(t *testing.T) {
	// A schema that is not current is refused before anything is built.
	dir := t.TempDir()
	set := []string{"--set", "server.data_dir=" + dir, "--set", "database.dsn=" + filepath.Join(dir, "hangar.db"),
		"--set", "storage.fs.path=" + filepath.Join(dir, "blobs"), "--set", "git.cache_dir=" + filepath.Join(dir, "git-cache"),
		"--set", "server.public_url=http://localhost:8080"}
	out, code := execute(t, append([]string{"admin", "gc"}, set...)...)
	require.Equal(t, 1, code)
	require.Contains(t, out, "run hangar admin migrate first")

	// So is a configuration that does not validate, as a usage error.
	out, code = execute(t, "admin", "gc", "--set", "storage.backend=tape", "--set", "server.data_dir="+t.TempDir())
	require.Equal(t, 1, code)
	require.Contains(t, out, "storage.backend")
	require.Equal(t, 2, run([]string{"admin", "gc", "--set", "storage.backend=tape", "--set", "server.data_dir=" + t.TempDir()}),
		"configuration problems exit with the usage code")

	// A database that cannot be opened fails the migration.
	blocker := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(blocker, nil, 0o600))
	out, code = execute(t, "admin", "migrate", "--set", "database.dsn="+filepath.Join(blocker, "hangar.db"))
	require.Equal(t, 1, code, out)
}

func TestTextProgress(t *testing.T) {
	var buf bytes.Buffer
	p := textProgress{w: &buf}
	require.NoError(t, p.Report(context.Background(), jobs.ProgressUpdate{Message: "Deleted 3 LFS objects."}))
	require.NoError(t, p.Report(context.Background(), jobs.ProgressUpdate{}))
	require.NoError(t, p.SetResult(context.Background(), map[string]int{"n": 1}))
	require.Equal(t, "Deleted 3 LFS objects.\n", buf.String())
}
