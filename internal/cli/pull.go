package cli

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jamesagarside/hangar/internal/api/gen"
	"github.com/jamesagarside/hangar/internal/model"
)

// parquetRevision is the branch that holds a dataset's Parquet export.
const parquetRevision = "refs/convert/parquet"

// pullOptions are the flags of `hangar pull`.
type pullOptions struct {
	Revision    string
	Include     []string
	Exclude     []string
	To          string
	ServerOnly  bool
	Upstream    string
	Type        string
	Configs     []string
	Splits      []string
	ParquetOnly bool
	Track       bool
	Now         bool
	CacheDir    string
}

// pullResult is what `hangar pull --json` prints.
type pullResult struct {
	Repo     string       `json:"repo"`
	Revision string       `json:"revision"`
	Commit   string       `json:"commit"`
	JobID    string       `json:"job_id,omitempty"`
	Files    []downloaded `json:"files"`
}

func (a *app) pullCommand() *cobra.Command {
	var o pullOptions
	cmd := &cobra.Command{
		Use:   "pull <repo>[:<quant>]",
		Short: "Pull a Repo into the Arsenal and download it to the local cache",
		Long: `Makes sure the Repo, or the files you choose, are in the Arsenal: if they are
not, asks the Instance to Pull them from an Upstream and shows the progress.
Then downloads them into the local Hugging Face cache, where Python libraries
pointed at the same Instance find them, or into a folder with --to.

Choose files with a GGUF quant after a colon, --include and --exclude
patterns, or, for datasets, --config, --split and --parquet-only. With
--server-only, nothing is downloaded to this machine.`,
		Example: `  hangar pull bartowski/Llama-3.2-3B-Instruct-GGUF:Q4_K_M
  hangar pull hf-internal-testing/tiny-random-bert --include '*.json' --include '*.safetensors'
  hangar pull datasets/lhoestq/demo1 --config default --split train --parquet-only --to ./data
  hangar pull Qwen/Qwen3-0.6B --server-only`,
		Args: args(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, argv []string) error {
			a := a.with(cmd)
			ref, err := parseRepoRef(argv[0], o.Type, true)
			if err != nil {
				return err
			}
			res, err := a.pull(cmd.Context(), ref, o)
			if err != nil {
				return err
			}
			if a.json {
				return a.printJSON(res)
			}
			if o.ServerOnly {
				a.printf("%s is in the Arsenal at %s.\n", display(ref.ID), shortSHA(res.Commit))
				return nil
			}
			where := "the Hugging Face cache"
			if o.To != "" {
				where = o.To
			}
			a.printf("Downloaded %d files of %s at %s into %s.\n", len(res.Files), display(ref.ID), shortSHA(res.Commit), where)
			return nil
		},
	}
	a.clientFlags(cmd)
	f := cmd.Flags()
	f.StringVar(&o.Revision, "revision", "", "branch, tag or Commit SHA (default: the default branch)")
	f.StringArrayVar(&o.Include, "include", nil, "only files matching this pattern, such as '*.safetensors' (repeatable)")
	f.StringArrayVar(&o.Exclude, "exclude", nil, "skip files matching this pattern (repeatable)")
	f.StringVar(&o.To, "to", "", "download into this folder instead of the Hugging Face cache")
	f.BoolVar(&o.ServerOnly, "server-only", false, "only Pull into the Arsenal; download nothing")
	f.StringVar(&o.Upstream, "upstream", "huggingface", "Upstream to Pull from when the Repo is not in the Arsenal")
	f.StringVar(&o.Type, "type", "", "Repo type for IDs without a prefix: model or dataset")
	f.StringArrayVar(&o.Configs, "config", nil, "dataset Config to Pull (repeatable)")
	f.StringArrayVar(&o.Splits, "split", nil, "dataset Split to Pull (repeatable)")
	f.BoolVar(&o.ParquetOnly, "parquet-only", false, "Pull and download only the dataset's Parquet export")
	f.BoolVar(&o.Track, "track", false, "make a new Mirror follow its upstream branch")
	f.BoolVar(&o.Now, "now", false, "start the Pull now, outside the Instance's pull windows, if allowed")
	f.StringVar(&o.CacheDir, "cache-dir", "", "Hugging Face cache folder (default: $HF_HUB_CACHE or ~/.cache/huggingface/hub)")
	return cmd
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// selection is the server-side file selection for a Pull.
func (o pullOptions) selection(ref repoRef) gen.Selection {
	var s gen.Selection
	if len(o.Include) > 0 {
		s.Include = &o.Include
	}
	if len(o.Exclude) > 0 {
		s.Exclude = &o.Exclude
	}
	if ref.Quant != "" {
		s.Quants = &[]string{ref.Quant}
	}
	if len(o.Configs) > 0 {
		s.Configs = &o.Configs
	}
	if len(o.Splits) > 0 {
		s.Splits = &o.Splits
	}
	if o.ParquetOnly {
		s.ParquetOnly = ptr(true)
	}
	return s
}

// pull runs `hangar pull`: Pull what is missing, then download.
func (a *app) pull(ctx context.Context, ref repoRef, o pullOptions) (*pullResult, error) {
	if ref.ID.Type == model.RepoTypeModel && (len(o.Configs) > 0 || len(o.Splits) > 0 || o.ParquetOnly) {
		return nil, usagef("--config, --split and --parquet-only are for datasets; use a datasets/ prefix")
	}
	if ref.ID.Type == model.RepoTypeDataset && ref.Quant != "" {
		return nil, usagef("quants are for GGUF models, not datasets")
	}
	c, err := a.client()
	if err != nil {
		return nil, err
	}
	res := &pullResult{Repo: display(ref.ID)}
	repo, err := c.getRepo(ctx, ref.ID)
	switch {
	case IsNotFound(err):
		body := gen.PullCreate{Upstream: o.Upstream, Repo: ref.ID.String(), Type: singular(ref.ID), Selection: ptr(o.selection(ref))}
		if o.Revision != "" {
			body.Revision = &o.Revision
		}
		if o.Track {
			body.Track = ptr(true)
		}
		if o.Now {
			body.Now = ptr(true)
		}
		a.logf("%s is not in the Arsenal. Pulling it from %s.\n", display(ref.ID), o.Upstream)
		job, err := c.startPull(ctx, body)
		if err != nil {
			return nil, err
		}
		res.JobID = job.Id.String()
		if _, err := a.watchJob(ctx, c, job.Id, "Pull"); err != nil {
			return nil, err
		}
		if repo, err = c.getRepo(ctx, ref.ID); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	}
	rev := o.Revision
	if rev == "" {
		rev = repo.DefaultBranch
	}
	if o.ParquetOnly {
		rev = parquetRevision
	}
	files, commit, err := a.selectFiles(ctx, c, ref, o, rev)
	if err != nil && (!IsNotFound(err) || !repo.Mirror) {
		return nil, err
	}
	missing := 0
	for _, f := range files {
		if !f.Present {
			missing++
		}
	}
	if (err != nil || missing > 0) && repo.Mirror && res.JobID == "" {
		if err != nil {
			a.logf("The Arsenal does not have %s at %s yet. Pulling it.\n", display(ref.ID), rev)
		} else {
			a.logf("%d of the selected files are not in the Arsenal. Pulling them.\n", missing)
		}
		job, err := c.pullIntoMirror(ctx, ref.ID, o.selection(ref))
		if err != nil {
			return nil, err
		}
		res.JobID = job.Id.String()
		if _, err := a.watchJob(ctx, c, job.Id, "Pull"); err != nil {
			return nil, err
		}
		if files, commit, err = a.selectFiles(ctx, c, ref, o, rev); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	res.Revision, res.Commit = rev, commit
	var want []wantFile
	var notPresent []string
	for _, f := range files {
		if !f.Present {
			notPresent = append(notPresent, f.Path)
			continue
		}
		want = append(want, fileOf(f))
	}
	if len(notPresent) > 0 {
		return nil, fmt.Errorf("these files are still not in the Arsenal: %s", strings.Join(notPresent, ", "))
	}
	if len(want) == 0 {
		return nil, notFoundf("no files in %s at %s match the selection", display(ref.ID), rev)
	}
	if o.ServerOnly {
		res.Files = []downloaded{}
		return res, nil
	}
	var total int64
	for _, f := range want {
		total += f.Size
	}
	cacheDir := o.CacheDir
	if cacheDir == "" {
		cacheDir = defaultHFCacheDir()
	}
	d := &downloader{c: c, id: ref.ID, commit: commit, cache: hfCache{root: cacheDir}, dir: o.To,
		prog: newProgress(a.stderr, "Downloading", total, a.json)}
	got, err := d.fetchAll(ctx, want)
	d.prog.finish()
	if err != nil {
		return nil, err
	}
	if o.To == "" {
		if err := d.cache.writeRef(ref.ID, rev, commit); err != nil {
			return nil, err
		}
	}
	res.Files = got
	return res, nil
}

// selectFiles lists the files at rev that the options select.
func (a *app) selectFiles(ctx context.Context, c *client, ref repoRef, o pullOptions, rev string) ([]gen.TreeEntry, string, error) {
	commit, entries, err := c.tree(ctx, ref.ID, rev)
	if err != nil {
		return nil, "", err
	}
	include := append([]string{}, o.Include...)
	if o.ParquetOnly && len(o.Configs) > 0 {
		for _, cfg := range o.Configs {
			if len(o.Splits) == 0 {
				include = append(include, cfg+"/")
			}
			for _, sp := range o.Splits {
				include = append(include, cfg+"/"+sp+"/")
			}
		}
	} else if o.ParquetOnly && len(o.Splits) > 0 {
		for _, sp := range o.Splits {
			include = append(include, "*/"+sp+"/*")
		}
	}
	inc, err := newGlobMatcher(include)
	if err != nil {
		return nil, "", err
	}
	exc, err := newGlobMatcher(o.Exclude)
	if err != nil {
		return nil, "", err
	}
	var quantFiles map[string]bool
	if ref.Quant != "" {
		q, err := c.quants(ctx, ref.ID, rev)
		if err != nil {
			return nil, "", err
		}
		quantFiles, err = pickQuant(q.Items, ref.Quant)
		if err != nil {
			return nil, "", err
		}
	}
	var out []gen.TreeEntry
	for _, e := range entries {
		if quantFiles != nil && !quantFiles[e.Path] && !keepWithQuant(e) {
			continue
		}
		if !inc.empty() && !inc.match(e.Path) {
			continue
		}
		if exc.match(e.Path) {
			continue
		}
		out = append(out, e)
	}
	return out, commit, nil
}

// keepWithQuant reports whether a file outside the chosen quant group still
// comes with a quant: the small files (README.md, config.json and the like)
// that come with the history anyway. Other GGUF files never do, whether they
// are LFS objects or regular git blobs: an Instance stores a GGUF file under
// hfapi.regular_upload_max as a regular blob, because the default
// .gitattributes has no *.gguf rule. Other LFS objects, such as safetensors
// weights, are left out too.
func keepWithQuant(e gen.TreeEntry) bool {
	return e.Lfs == nil && !strings.EqualFold(path.Ext(e.Path), ".gguf")
}

// pickQuant returns the files of a quant group (case-insensitive) and its
// projectors.
func pickQuant(groups []gen.QuantGroup, quant string) (map[string]bool, error) {
	names := make([]string, 0, len(groups))
	for _, g := range groups {
		names = append(names, g.Name)
		if !strings.EqualFold(g.Name, quant) {
			continue
		}
		files := map[string]bool{}
		for _, f := range g.Files {
			files[f] = true
		}
		for _, p := range deref(g.Projectors) {
			files[p] = true
		}
		return files, nil
	}
	if len(names) == 0 {
		return nil, notFoundf("the Repo has no GGUF quants")
	}
	return nil, notFoundf("no quant %q; the Repo has %s", quant, strings.Join(names, ", "))
}
