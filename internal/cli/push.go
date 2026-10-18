package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/hangar-hub/hangar/internal/api/gen"
	"github.com/hangar-hub/hangar/internal/model"
)

// pushOptions are the flags of `hangar push`.
type pushOptions struct {
	Branch      string
	Message     string
	Description string
	Create      bool
	Private     bool
	Type        string
	Include     []string
	Exclude     []string
	CreatePR    bool
}

// pushResult is what `hangar push --json` prints.
type pushResult struct {
	Repo        string `json:"repo"`
	Commit      string `json:"commit"`
	Ref         string `json:"ref"`
	Files       int    `json:"files"`
	Bytes       int64  `json:"bytes"`
	NoOp        bool   `json:"no_op"`
	PullRequest int    `json:"pull_request,omitempty"`
}

func (a *app) pushCommand() *cobra.Command {
	var o pushOptions
	cmd := &cobra.Command{
		Use:   "push <dir> <repo>",
		Short: "Upload a folder to a Repo as one Commit",
		Long: `Uploads every file under a folder as one Commit. Large files go up in parts,
straight to the Blob store where the Instance allows it, and an interrupted
upload resumes from the parts already stored when you run the command again.
A part that fails for a reason that may pass, such as a dropped connection
or a 503 from a proxy, is sent again up to 6 times with a growing wait, and
part URLs that expired are renewed. When finishing an upload fails for a
reason that may pass, the Instance answers 503 with Retry-After, a proxy
cuts the request off, or no answer comes within 5 minutes (40 seconds per
GiB for a file larger than 7.5 GiB), and the command waits and tries again,
up to 10 times.
A file that still fails does not stop the others: the command uploads the
rest, names the files that failed and makes no Commit, so running it again
sends only those files.
Files the Instance already has anywhere are not sent again.

With --branch, a branch that does not exist yet is created from the default
branch before the upload starts.

The .git folder and huggingface_hub's .cache/huggingface folder are skipped.`,
		Example: `  hangar push ./my-finetune acme/my-finetune --create --private
  hangar push ./data datasets/acme/reviews --message "Add the March export"
  hangar push ./out acme/model --branch experiment --include '*.safetensors'`,
		Args: args(cobra.ExactArgs(2)),
		RunE: func(cmd *cobra.Command, argv []string) error {
			a := a.with(cmd)
			ref, err := parseRepoRef(argv[1], o.Type, false)
			if err != nil {
				return err
			}
			res, err := a.push(cmd.Context(), argv[0], ref.ID, o)
			if err != nil {
				return err
			}
			if a.json {
				return a.printJSON(res)
			}
			switch {
			case res.NoOp:
				a.printf("Nothing to commit: %s already has these files at %s.\n", display(ref.ID), shortSHA(res.Commit))
			case res.PullRequest > 0:
				a.printf("Opened pull request #%d on %s with Commit %s (%d files, %s).\n", res.PullRequest, display(ref.ID), shortSHA(res.Commit), res.Files, formatBytes(res.Bytes))
			default:
				a.printf("Committed %d files (%s) to %s at %s.\n", res.Files, formatBytes(res.Bytes), display(ref.ID), shortSHA(res.Commit))
			}
			return nil
		},
	}
	a.clientFlags(cmd)
	f := cmd.Flags()
	f.StringVar(&o.Branch, "branch", "", "branch to commit to, created from the default branch if it does not exist (default: the default branch)")
	f.StringVarP(&o.Message, "message", "m", "Upload with hangar push", "Commit message")
	f.StringVar(&o.Description, "description", "", "longer Commit description")
	f.BoolVar(&o.Create, "create", false, "create the Repo if it does not exist")
	f.BoolVar(&o.Private, "private", false, "with --create, make the new Repo private")
	f.StringVar(&o.Type, "type", "", "Repo type for IDs without a prefix: model or dataset")
	f.StringArrayVar(&o.Include, "include", nil, "only upload files matching this pattern (repeatable)")
	f.StringArrayVar(&o.Exclude, "exclude", nil, "skip files matching this pattern (repeatable)")
	f.BoolVar(&o.CreatePR, "create-pr", false, "open a pull request instead of committing to the branch")
	return cmd
}

// push runs `hangar push`.
func (a *app) push(ctx context.Context, dir string, id model.RepoID, o pushOptions) (*pushResult, error) {
	files, outside, err := walkFolder(dir, o.Include, o.Exclude)
	if err != nil {
		return nil, err
	}
	a.warnOutside(dir, outside)
	if len(files) == 0 {
		return nil, usagef("%s has no files to upload", dir)
	}
	c, err := a.client()
	if err != nil {
		return nil, err
	}
	if err := a.ensureRepo(ctx, c, id, o.Create, o.Private); err != nil {
		return nil, err
	}
	// A branch that does not exist yet is created from the default branch
	// before anything is uploaded, so the Commit has somewhere to go.
	if o.Branch != "" {
		created, err := c.ensureBranch(ctx, id, o.Branch)
		if err != nil {
			return nil, fmt.Errorf("create branch %s: %w", o.Branch, err)
		}
		if created {
			a.logf("Created branch %s from the default branch.\n", o.Branch)
		}
	}
	return a.commitFiles(ctx, c, id, files, o)
}

// ensureRepo checks the Repo exists, creating it if asked to.
func (a *app) ensureRepo(ctx context.Context, c *client, id model.RepoID, create, private bool) error {
	_, err := c.getRepo(ctx, id)
	if err == nil {
		return nil
	}
	if !IsNotFound(err) {
		return err
	}
	if !create {
		return notFoundf("%s does not exist; pass --create to create it", display(id))
	}
	body := gen.RepoCreate{Type: singular(id), Namespace: id.Namespace, Name: id.Name}
	if private {
		body.Visibility = ptr(gen.Private)
	}
	if _, err := c.createRepo(ctx, body); err != nil {
		return err
	}
	a.logf("Created %s.\n", display(id))
	return nil
}

// commitFiles hashes, uploads and commits files.
func (a *app) commitFiles(ctx context.Context, c *client, id model.RepoID, files []localFile, o pushOptions) (*pushResult, error) {
	var total int64
	for _, f := range files {
		total += f.Size
	}
	hp := newProgress(a.stderr, "Hashing", total, a.json)
	if err := hashFiles(ctx, files, hp); err != nil {
		return nil, err
	}
	hp.finish()
	up := &uploader{c: c, id: id, revision: o.Branch, prog: newProgress(a.stderr, "Uploading", total, a.json), waiting: a.waitStatus, note: a.logf}
	ops, err := up.upload(ctx, files)
	up.prog.finish()
	if err != nil {
		return nil, err
	}
	body := gen.CommitRequest{Summary: o.Message, Operations: ops}
	if o.Branch != "" {
		body.Branch = &o.Branch
	}
	if o.Description != "" {
		body.Description = &o.Description
	}
	if o.CreatePR {
		body.CreatePr = ptr(true)
	}
	res, err := c.createCommit(ctx, id, body)
	if err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return &pushResult{Repo: display(id), Commit: res.Sha, Ref: res.Ref, Files: len(files), Bytes: total,
		NoOp: deref(res.NoOp), PullRequest: deref(res.PullRequest)}, nil
}
