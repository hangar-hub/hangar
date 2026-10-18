package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hangar-hub/hangar/internal/api/gen"
	"github.com/hangar-hub/hangar/internal/model"
)

// repoInfo is what `hangar info --json` prints for a Repo in the Arsenal.
type repoInfo struct {
	Repo         *gen.Repo           `json:"repo"`
	Commit       string              `json:"commit"`
	Arsenal      gen.ArsenalState    `json:"arsenal"`
	Files        []gen.TreeEntry     `json:"files"`
	Quants       []gen.QuantGroup    `json:"quants,omitempty"`
	Dependencies *gen.Dependencies   `json:"dependencies,omitempty"`
	Configs      *gen.DatasetConfigs `json:"configs,omitempty"`
}

func (a *app) infoCommand() *cobra.Command {
	var revision, upstream, typ string
	var local bool
	cmd := &cobra.Command{
		Use:   "info <repo>",
		Short: "Show a Repo's metadata, files, quants, Dependencies and Arsenal state",
		Long: `Shows a Repo in the Arsenal: its metadata, files (and whether each one is in
the Arsenal), GGUF quants, Dependencies and, for datasets, Configs and Splits.

If the Repo is not in the Arsenal, shows what an Upstream has instead, unless
--local is set. Repo IDs for datasets take a datasets/ prefix.`,
		Example: `  hangar info bartowski/Llama-3.2-3B-Instruct-GGUF
  hangar info datasets/lhoestq/demo1 --json`,
		Args: args(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, argv []string) error {
			a := a.with(cmd)
			ref, err := parseRepoRef(argv[0], typ, false)
			if err != nil {
				return err
			}
			return a.info(cmd.Context(), ref.ID, revision, upstream, local)
		},
	}
	a.clientFlags(cmd)
	f := cmd.Flags()
	f.StringVar(&revision, "revision", "", "branch, tag or Commit SHA (default: the default branch)")
	f.StringVar(&upstream, "upstream", "huggingface", "Upstream to ask when the Repo is not in the Arsenal")
	f.StringVar(&typ, "type", "", "Repo type for IDs without a prefix: model or dataset")
	f.BoolVar(&local, "local", false, "only look in the Arsenal")
	return cmd
}

func (a *app) info(ctx context.Context, id model.RepoID, revision, upstream string, local bool) error {
	c, err := a.client()
	if err != nil {
		return err
	}
	repo, err := c.getRepo(ctx, id)
	if IsNotFound(err) && !local {
		return a.upstreamInfo(ctx, c, id, upstream, revision)
	}
	if err != nil {
		return err
	}
	rev := revision
	if rev == "" {
		rev = repo.DefaultBranch
	}
	info := repoInfo{Repo: repo}
	if info.Commit, info.Files, err = c.tree(ctx, id, rev); err != nil {
		return err
	}
	info.Arsenal = arsenalOf(info.Files)
	if id.Type == model.RepoTypeModel {
		if q, err := c.quants(ctx, id, rev); err == nil {
			info.Quants = q.Items
		} else if !IsNotFound(err) {
			return err
		}
	} else {
		if cfgs, err := c.datasetConfigs(ctx, id, rev); err == nil {
			info.Configs = cfgs
		} else if !IsNotFound(err) {
			return err
		}
	}
	if deps, err := c.dependencies(ctx, id); err == nil {
		info.Dependencies = deps
	} else if !IsNotFound(err) {
		return err
	}
	if a.json {
		return a.printJSON(info)
	}
	return a.printRepoInfo(info)
}

// arsenalOf says whether every LFS file is present.
func arsenalOf(files []gen.TreeEntry) gen.ArsenalState {
	present, missing := 0, 0
	for _, f := range files {
		if f.Present {
			present++
		} else {
			missing++
		}
	}
	switch {
	case missing == 0:
		return gen.ArsenalStateFull
	case present == 0:
		return gen.ArsenalStateNone
	}
	return gen.ArsenalStatePartial
}

func (a *app) printRepoInfo(info repoInfo) error {
	r := info.Repo
	kind := "Local Repo"
	if r.Mirror {
		kind = "Mirror"
		if r.MirrorStatus != nil {
			kind = fmt.Sprintf("Mirror of %s on %s", r.MirrorStatus.UpstreamRepo, r.MirrorStatus.Upstream)
		}
	}
	a.printf("%s\n", display(model.RepoID{Type: model.RepoType(r.Type), Namespace: r.Namespace, Name: r.Name}))
	if d := deref(r.Description); d != "" {
		a.printf("%s\n", d)
	}
	a.printf("\n")
	rows := [][]string{
		{"Type", string(r.Type)},
		{"Source", kind},
		{"Visibility", string(r.Visibility)},
		{"Default branch", r.DefaultBranch},
		{"Commit", info.Commit},
		{"Arsenal", arsenalLabel(info.Arsenal, false)},
		{"Size", formatBytes(r.SizeBytes + r.LfsBytes)},
	}
	if l := deref(r.Licence); l != "" {
		rows = append(rows, []string{"Licence", l})
	}
	if s := r.Summary; s != nil {
		if s.Architecture != nil {
			rows = append(rows, []string{"Architecture", *s.Architecture})
		}
		if s.Parameters != nil {
			rows = append(rows, []string{"Parameters", strconv.FormatInt(*s.Parameters, 10)})
		}
		if s.ContextLength != nil {
			rows = append(rows, []string{"Context length", strconv.FormatInt(*s.ContextLength, 10)})
		}
		if s.PipelineTag != nil {
			rows = append(rows, []string{"Task", *s.PipelineTag})
		}
		if s.LibraryName != nil {
			rows = append(rows, []string{"Library", *s.LibraryName})
		}
	}
	for _, row := range rows {
		a.printf("%-16s %s\n", row[0]+":", row[1])
	}
	if len(info.Quants) > 0 {
		a.printf("\nQuants:\n")
		qrows := make([][]string, 0, len(info.Quants))
		for _, q := range info.Quants {
			qrows = append(qrows, []string{q.Name, strconv.Itoa(len(q.Files)), formatBytes(q.Size), presentLabel(q.Present)})
		}
		if err := a.table([]string{"QUANT", "FILES", "SIZE", "ARSENAL"}, qrows); err != nil {
			return err
		}
	}
	if info.Configs != nil && len(info.Configs.Configs) > 0 {
		a.printf("\nConfigs and Splits:\n")
		var crows [][]string
		for _, cfg := range info.Configs.Configs {
			for _, sp := range cfg.Splits {
				crows = append(crows, []string{cfg.Config, sp.Split, string(sp.Status),
					optInt(sp.NumRows), optBytes(sp.NumBytesSource), optBytes(sp.NumBytesParquet)})
			}
		}
		if err := a.table([]string{"CONFIG", "SPLIT", "STATUS", "ROWS", "SOURCE SIZE", "PARQUET SIZE"}, crows); err != nil {
			return err
		}
	}
	if d := info.Dependencies; d != nil && (len(d.Needs) > 0 || len(d.NeededBy) > 0) {
		a.printf("\nDependencies:\n")
		var drows [][]string
		for _, dep := range d.Needs {
			drows = append(drows, []string{"needs", dep.Repo, string(dep.Kind), strconv.FormatBool(dep.Required), arsenalLabel(dep.Arsenal, false)})
		}
		for _, dep := range d.NeededBy {
			drows = append(drows, []string{"needed by", dep.Repo, string(dep.Kind), strconv.FormatBool(dep.Required), arsenalLabel(dep.Arsenal, false)})
		}
		if err := a.table([]string{"LINK", "REPO", "KIND", "REQUIRED", "ARSENAL"}, drows); err != nil {
			return err
		}
	}
	a.printf("\nFiles:\n")
	frows := make([][]string, 0, len(info.Files))
	for _, f := range info.Files {
		size := f.Size
		if f.Lfs != nil {
			size = f.Lfs.Size
		}
		frows = append(frows, []string{f.Path, formatBytes(size), presentLabel(f.Present)})
	}
	return a.table([]string{"PATH", "SIZE", "ARSENAL"}, frows)
}

func presentLabel(present bool) string {
	if present {
		return "In arsenal"
	}
	return "Not in arsenal"
}

func optInt(p *int64) string {
	if p == nil {
		return "-"
	}
	return strconv.FormatInt(*p, 10)
}

func optBytes(p *int64) string {
	if p == nil {
		return "-"
	}
	return formatBytes(*p)
}

func (a *app) upstreamInfo(ctx context.Context, c *client, id model.RepoID, upstream, revision string) error {
	d, err := c.discoverDetail(ctx, id, upstream, revision)
	if err != nil {
		if IsNotFound(err) {
			return notFoundf("%s is not in the Arsenal or on %s", display(id), upstream)
		}
		return err
	}
	if a.json {
		return a.printJSON(map[string]any{"upstream": upstream, "detail": d})
	}
	a.printf("%s on %s (%s)\n\n", display(id), upstream, arsenalLabel(d.Arsenal, false))
	a.printf("%-16s %s\n", "Commit:", orDash(deref(d.Sha)))
	a.printf("%-16s %s\n", "Total size:", formatBytes(d.TotalSize))
	if d.PipelineTag != nil {
		a.printf("%-16s %s\n", "Task:", *d.PipelineTag)
	}
	if d.Quants != nil && len(*d.Quants) > 0 {
		a.printf("\nQuants:\n")
		var rows [][]string
		for _, q := range *d.Quants {
			rows = append(rows, []string{q.Name, strconv.Itoa(len(q.Files)), formatBytes(q.Size), presentLabel(q.Present)})
		}
		if err := a.table([]string{"QUANT", "FILES", "SIZE", "ARSENAL"}, rows); err != nil {
			return err
		}
	}
	if len(d.Dependencies) > 0 {
		a.printf("\nDependencies a Pull would add:\n")
		var rows [][]string
		for _, dep := range d.Dependencies {
			rows = append(rows, []string{dep.Repo, string(dep.Kind), strconv.FormatBool(dep.Required)})
		}
		if err := a.table([]string{"REPO", "KIND", "REQUIRED"}, rows); err != nil {
			return err
		}
	}
	a.printf("\nFiles:\n")
	rows := make([][]string, 0, len(d.Files))
	for _, f := range d.Files {
		rows = append(rows, []string{f.Path, formatBytes(f.Size), presentLabel(f.Present)})
	}
	if err := a.table([]string{"PATH", "SIZE", "ARSENAL"}, rows); err != nil {
		return err
	}
	a.printf("\nRun hangar pull %s to pull it into the Arsenal.\n", strings.TrimSpace(display(id)))
	return nil
}
