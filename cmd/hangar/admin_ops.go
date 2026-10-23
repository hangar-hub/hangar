package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/config"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/gc"
	"github.com/hangar-hub/hangar/internal/index"
	"github.com/hangar-hub/hangar/internal/jobs"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/telemetry"
)

// adminInstance opens the database and Blob store from the server
// configuration and builds the Instance's services, for the admin commands
// that act on them directly. It refuses a database whose schema is not
// current.
func adminInstance(ctx context.Context, flags *configFlags, stderr io.Writer) (*instance, func(), error) {
	cfg, err := flags.load("")
	if err != nil {
		return nil, nil, usageError{err}
	}
	if err := cfg.ValidateBeforeSecrets(); err != nil {
		return nil, nil, usageError{err}
	}
	if _, err := config.EnsureSecrets(cfg); err != nil {
		return nil, nil, err
	}
	tel, err := telemetry.Setup(ctx, telemetry.Options{Config: cfg.Telemetry, Version: version, Role: "admin", Stdout: stderr,
		Getenv: func(string) string { return "" }})
	if err != nil {
		return nil, nil, fmt.Errorf("telemetry: %w", err)
	}
	d, err := openDatabase(ctx, cfg, false)
	if errors.Is(err, db.ErrLocked) {
		_ = tel.Shutdown(ctx)
		return nil, nil, fmt.Errorf("%w: stop Hangar before running this command on a SQLite database", err)
	}
	if err != nil {
		_ = tel.Shutdown(ctx)
		return nil, nil, err
	}
	closeAll := func() {
		_ = d.Close()
		_ = tel.Shutdown(context.WithoutCancel(ctx))
	}
	st, err := d.Schema(ctx)
	if err == nil && !st.UpToDate() {
		err = fmt.Errorf("the database schema is at %q and this binary expects %q; run hangar admin migrate", st.Current, st.Expected)
	}
	if err != nil {
		closeAll()
		return nil, nil, err
	}
	blobs, err := openBlobStore(ctx, cfg, tel)
	if err != nil {
		closeAll()
		return nil, nil, err
	}
	in, err := newInstance(ctx, cfg, tel, d, blobs)
	if err != nil {
		closeAll()
		return nil, nil, err
	}
	return in, func() { in.close(context.WithoutCancel(ctx)); closeAll() }, nil
}

// readPassword reads one line from r for --password-stdin, or generates a
// password when generate is set.
func readPassword(r io.Reader, fromStdin bool) (password string, generated bool, err error) {
	if !fromStdin {
		pw, err := config.RandomPassword(24)
		return pw, true, err
	}
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", false, err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return "", false, usageError{errors.New("--password-stdin read an empty password")}
	}
	return line, false, nil
}

// userResult is the --json output of create-user and reset-password. The
// password is included only when Hangar generated it.
type userResult struct {
	Username string `json:"username"`
	ID       string `json:"id"`
	IsAdmin  bool   `json:"is_admin"`
	Password string `json:"password,omitempty"`
}

func newCreateUserCommand() *cobra.Command {
	var flags configFlags
	var asJSON, admin, fromStdin bool
	var email, displayName string
	cmd := &cobra.Command{
		Use:   "create-user <username>",
		Short: "Create a local user",
		Long: `Creates a local user with a personal Namespace. The password is read from
standard input with --password-stdin, or generated and printed once.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			pw, generated, err := readPassword(cmd.InOrStdin(), fromStdin)
			if err != nil {
				return err
			}
			in, done, err := adminInstance(ctx, &flags, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			defer done()
			u, err := in.auth.Users.Create(ctx, auth.NewUser{Username: args[0], Email: email, DisplayName: displayName, Password: pw,
				IsAdmin: admin}, model.System())
			if err != nil {
				return err
			}
			res := userResult{Username: u.Username, ID: u.ID.String(), IsAdmin: u.IsAdmin}
			if generated {
				res.Password = pw
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), res)
			}
			out := cmd.OutOrStdout()
			kind := "user"
			if u.IsAdmin {
				kind = "Instance admin"
			}
			fmt.Fprintf(out, "Created the %s %s.\n", kind, u.Username)
			if generated {
				fmt.Fprintf(out, "Password: %s\nIt is shown once. Store it somewhere safe.\n", pw)
			}
			return nil
		},
	}
	flags.register(cmd)
	addJSONFlag(cmd, &asJSON)
	fl := cmd.Flags()
	fl.BoolVar(&admin, "admin", false, "make the user an Instance admin")
	fl.StringVar(&email, "email", "", "the user's email address")
	fl.StringVar(&displayName, "display-name", "", "the user's display name")
	fl.BoolVar(&fromStdin, "password-stdin", false, "read the password from standard input instead of generating one")
	return cmd
}

func newResetPasswordCommand() *cobra.Command {
	var flags configFlags
	var asJSON, fromStdin bool
	cmd := &cobra.Command{
		Use:   "reset-password <username>",
		Short: "Set a local user's password",
		Long: `Sets a new password for a user, signs them out everywhere and clears
sign-in throttling, for example when the only admin is locked out. The
password is read from standard input with --password-stdin, or generated
and printed once. To let a user choose their own, create a reset link in
the admin UI instead.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			pw, generated, err := readPassword(cmd.InOrStdin(), fromStdin)
			if err != nil {
				return err
			}
			in, done, err := adminInstance(ctx, &flags, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			defer done()
			u, err := in.auth.Users.GetByUsername(ctx, args[0])
			if err != nil {
				return fmt.Errorf("user %q: %w", args[0], err)
			}
			if u, err = in.auth.Users.SetPassword(ctx, u.ID, pw, model.System()); err != nil {
				return err
			}
			res := userResult{Username: u.Username, ID: u.ID.String(), IsAdmin: u.IsAdmin}
			if generated {
				res.Password = pw
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), res)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Set a new password for %s and signed them out everywhere.\n", u.Username)
			if generated {
				fmt.Fprintf(out, "Password: %s\nIt is shown once. Store it somewhere safe.\n", pw)
			}
			return nil
		},
	}
	flags.register(cmd)
	addJSONFlag(cmd, &asJSON)
	cmd.Flags().BoolVar(&fromStdin, "password-stdin", false, "read the password from standard input instead of generating one")
	return cmd
}

// gcResult is the --json output of hangar admin gc.
type gcResult struct {
	// JobID is set with --queue.
	JobID  string     `json:"job_id,omitempty"`
	Report *gc.Report `json:"report,omitempty"`
}

// textProgress reports a gc run's progress on standard error.
type textProgress struct{ w io.Writer }

func (p textProgress) Report(_ context.Context, u jobs.ProgressUpdate) error {
	if u.Message != "" {
		fmt.Fprintln(p.w, u.Message)
	}
	return nil
}

func (textProgress) SetResult(context.Context, any) error { return nil }

func newGCCommand() *cobra.Command {
	var flags configFlags
	var asJSON, gitOnly, lfsOnly, queue bool
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Collect garbage: unreferenced LFS objects and unused git data",
		Long: `Deletes LFS objects no Repo has referenced for gc.grace_period, orphaned
LFS objects in the Blob store, retired and orphaned packs, git data of
purged Repos, stale temporary files and abandoned uploads, then reports
what it freed. It runs in this process; with --queue it queues a gc Job
for the Instance's workers instead, which needs a running Instance on
Postgres.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if gitOnly && lfsOnly {
				return usageError{errors.New("--git and --lfs cannot be combined; leave both out to collect everything")}
			}
			scope := gc.ScopeAll
			switch {
			case gitOnly:
				scope = gc.ScopeGit
			case lfsOnly:
				scope = gc.ScopeLFS
			}
			ctx := cmd.Context()
			in, done, err := adminInstance(ctx, &flags, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			defer done()
			out := cmd.OutOrStdout()
			if queue {
				id, err := gc.Start(ctx, in.queue, scope, uuid.Nil)
				if err != nil {
					return err
				}
				if asJSON {
					return writeJSON(out, gcResult{JobID: id.String()})
				}
				_, err = fmt.Fprintf(out, "Queued gc Job %s (scope %s). Follow it with hangar jobs watch %s.\n", id, scope, id)
				return err
			}
			var progress jobs.Progress = textProgress{w: cmd.ErrOrStderr()}
			if asJSON {
				progress = textProgress{w: io.Discard}
			}
			rep, err := in.collector.Run(ctx, scope, progress)
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSON(out, gcResult{Report: &rep})
			}
			fmt.Fprintf(out, "Garbage collection (%s) finished.\n", rep.Scope)
			fmt.Fprintf(out, "  LFS objects deleted:       %d (%d bytes)\n", rep.LFSObjectsDeleted, rep.LFSBytesFreed)
			fmt.Fprintf(out, "  Orphaned LFS objects:      %d\n", rep.OrphanLFSBlobsDeleted)
			fmt.Fprintf(out, "  Packs deleted:             %d (%d bytes)\n", rep.PacksDeleted, rep.PackBytesFreed)
			fmt.Fprintf(out, "  Orphaned git data:         %d\n", rep.OrphanGitBlobsDeleted)
			fmt.Fprintf(out, "  Temporary objects deleted: %d\n", rep.StagingBlobsDeleted)
			fmt.Fprintf(out, "  Uploads aborted:           %d\n", rep.UploadsAborted)
			fmt.Fprintf(out, "  Stored now:                %d bytes of LFS objects, %d bytes of git data\n", rep.LFSBytes, rep.GitBytes)
			if rep.Errors > 0 {
				return fmt.Errorf("%d objects could not be collected; the log has the details, and the next run tries again", rep.Errors)
			}
			return nil
		},
	}
	flags.register(cmd)
	addJSONFlag(cmd, &asJSON)
	fl := cmd.Flags()
	fl.BoolVar(&gitOnly, "git", false, "collect git data only")
	fl.BoolVar(&lfsOnly, "lfs", false, "collect LFS objects only")
	fl.BoolVar(&queue, "queue", false, "queue a gc Job for the Instance's workers instead of running it here")
	return cmd
}

// reindexResult is the --json output of hangar admin reindex.
type reindexResult struct {
	IndexJobs int `json:"index_jobs"`
	ScanJobs  int `json:"scan_jobs"`
}

func newReindexCommand() *cobra.Command {
	var flags configFlags
	var asJSON, stale bool
	var repoType string
	cmd := &cobra.Command{
		Use:   "reindex [repo...]",
		Short: "Index and scan Repos again",
		Long: `Queues index and scan Jobs for the head of every branch of the named Repos,
or of every Repo when none is named. With --stale it queues them only for
results from an older indexer or scanner version, as workers do when they
start. The Instance's workers run the Jobs; on SQLite they run when Hangar
starts again.

Name a Repo as namespace/name, or datasets/namespace/name for a dataset.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if stale && len(args) > 0 {
				return usageError{errors.New("--stale applies to every Repo; leave out the Repo IDs")}
			}
			ids := make([]model.RepoID, 0, len(args))
			for _, a := range args {
				id, err := parseAdminRepoID(a, repoType)
				if err != nil {
					return usageError{err}
				}
				ids = append(ids, id)
			}
			ctx := cmd.Context()
			in, done, err := adminInstance(ctx, &flags, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			defer done()
			var res reindexResult
			if stale {
				if res.IndexJobs, err = in.indexer.ReindexStale(ctx, in.queue); err != nil {
					return err
				}
				if res.ScanJobs, err = in.scanner.RescanStale(ctx, in.queue); err != nil {
					return err
				}
			} else if res, err = in.reindex(ctx, ids); err != nil {
				return err
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), res)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Queued %d index Jobs and %d scan Jobs.\n", res.IndexJobs, res.ScanJobs)
			return err
		},
	}
	flags.register(cmd)
	addJSONFlag(cmd, &asJSON)
	cmd.Flags().BoolVar(&stale, "stale", false, "queue Jobs only for results from an older indexer or scanner version")
	cmd.Flags().StringVar(&repoType, "type", "", "the type of Repo IDs without a prefix: model (the default) or dataset")
	return cmd
}

// parseAdminRepoID parses `[models/|datasets/]namespace/name`.
func parseAdminRepoID(s, typeFlag string) (model.RepoID, error) {
	t := model.RepoTypeModel
	if typeFlag != "" {
		pt, err := model.ParseRepoType(typeFlag)
		if err != nil {
			return model.RepoID{}, fmt.Errorf("--type must be model or dataset, not %q", typeFlag)
		}
		t = pt
	}
	switch {
	case strings.HasPrefix(s, "datasets/"):
		t, s = model.RepoTypeDataset, strings.TrimPrefix(s, "datasets/")
	case strings.HasPrefix(s, "models/"):
		t, s = model.RepoTypeModel, strings.TrimPrefix(s, "models/")
	}
	return model.ParseRepoID(t, s)
}

// reindex queues index and scan Jobs for every branch head of the given
// Repos, or of every Repo that is not deleted when ids is empty.
func (in *instance) reindex(ctx context.Context, ids []model.RepoID) (reindexResult, error) {
	var res reindexResult
	var repos []*repo.Repo
	if len(ids) == 0 {
		var uuids []uuid.UUID
		if err := in.db.NewSelect().Model((*db.Repo)(nil)).Column("id").Where("deleted_at IS NULL").Order("id").Scan(ctx, &uuids); err != nil {
			return res, err
		}
		for _, u := range uuids {
			r, err := in.repos.GetByUUID(ctx, u)
			if errors.Is(err, repo.ErrNotFound) {
				continue
			}
			if err != nil {
				return res, err
			}
			repos = append(repos, r)
		}
	}
	for _, id := range ids {
		r, err := in.repos.Get(ctx, id)
		if err != nil {
			return res, fmt.Errorf("%s: %w", id.Path(), err)
		}
		repos = append(repos, r)
	}
	for _, r := range repos {
		if r.DeletedAt != nil {
			continue
		}
		refs, err := in.repos.Refs(ctx, r.ID)
		if err != nil {
			return res, fmt.Errorf("%s: %w", r.ID.Path(), err)
		}
		seen := map[model.CommitSHA]bool{}
		for _, b := range refs.Branches {
			if seen[b.Target] {
				continue
			}
			seen[b.Target] = true
			job := index.CommitJob{RepoUUID: r.UUID, Repo: r.ID.String(), Type: r.ID.Type, Commit: b.Target}
			for _, k := range []jobs.Kind{jobs.KindIndex, jobs.KindScan} {
				if _, err := in.queue.Enqueue(ctx, jobs.NewJob{Kind: k, DedupeKey: job.DedupeKey(), Payload: job, Repo: r.UUID}); err != nil {
					return res, err
				}
			}
			res.IndexJobs++
			res.ScanJobs++
		}
	}
	return res, nil
}
