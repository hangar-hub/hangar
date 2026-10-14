package cli

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jamesagarside/hangar/internal/api/gen"
)

func (a *app) searchCommand() *cobra.Command {
	var upstream, typ, sort string
	var filters []string
	var limit int
	cmd := &cobra.Command{
		Use:   "search [query]",
		Short: "Search the Arsenal, or an Upstream with --upstream",
		Long: `Searches the Repos you can see in the Arsenal. With --upstream, searches an
Upstream instead (Discover) and marks each result as in the Arsenal, partly in
the Arsenal, or not.

Filters for the Arsenal are key=value pairs: format, task, library, quant,
licence, ns, params_min, params_max, context_min, size_max, mirror, modality,
language and size_category. Filters for an Upstream are its tags, such as
gguf, mlx or text-generation.`,
		Example: `  hangar search llama --filter format=gguf --filter quant=Q4_K_M
  hangar search --type dataset --filter task=text-classification
  hangar search qwen --upstream --filter gguf --sort downloads`,
		Args: args(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, argv []string) error {
			a := a.with(cmd)
			q := ""
			if len(argv) == 1 {
				q = argv[0]
			}
			if cmd.Flags().Changed("upstream") {
				return a.discover(cmd.Context(), q, upstream, typ, sort, filters, limit)
			}
			return a.searchArsenal(cmd.Context(), q, typ, sort, filters, limit)
		},
	}
	a.clientFlags(cmd)
	f := cmd.Flags()
	f.StringVar(&upstream, "upstream", "", "search this Upstream instead of the Arsenal (default huggingface)")
	f.Lookup("upstream").NoOptDefVal = "huggingface"
	f.StringVar(&typ, "type", "model", "Repo type: model or dataset")
	f.StringArrayVar(&filters, "filter", nil, "filter results (repeatable); see above")
	f.StringVar(&sort, "sort", "", "sort order: for the Arsenal updated, name, size, parameters, downloads or likes; for an Upstream trending, downloads, likes or lastModified")
	f.IntVar(&limit, "limit", 50, "maximum number of results, up to 500")
	return cmd
}

func repoTypeArg(typ string) (gen.RepoTypePlural, error) {
	switch typ {
	case "model", "models", "":
		return gen.Models, nil
	case "dataset", "datasets":
		return gen.Datasets, nil
	}
	return "", usagef("--type must be model or dataset, not %q", typ)
}

func (a *app) searchArsenal(ctx context.Context, q, typ, sort string, filters []string, limit int) error {
	t, err := repoTypeArg(typ)
	if err != nil {
		return err
	}
	if limit < 1 || limit > 500 {
		return usagef("--limit must be between 1 and 500")
	}
	params := &gen.ListReposParams{Type: &t, Limit: &limit}
	if q != "" {
		params.Q = &q
	}
	if sort != "" {
		s := gen.ListReposParamsSort(sort)
		params.Sort = &s
	}
	for _, f := range filters {
		if err := applyRepoFilter(params, f); err != nil {
			return err
		}
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	r, err := c.api.ListReposWithResponse(ctx, params)
	if err != nil {
		return c.reach(err)
	}
	if err := check(r.HTTPResponse, r.Body, http.StatusOK); err != nil {
		return err
	}
	if a.json {
		return a.printJSON(r.JSON200)
	}
	if len(r.JSON200.Items) == 0 {
		a.printf("No Repos match.\n")
		return nil
	}
	rows := make([][]string, 0, len(r.JSON200.Items))
	for _, repo := range r.JSON200.Items {
		formats, quants := "-", "-"
		if repo.Summary != nil {
			if repo.Summary.Formats != nil {
				formats = orDash(strings.Join(*repo.Summary.Formats, ","))
			}
			if repo.Summary.Quants != nil {
				quants = orDash(strings.Join(*repo.Summary.Quants, ","))
			}
		}
		source := "local"
		if repo.Mirror {
			source = "mirror"
		}
		id := repo.Id
		if repo.Type == gen.RepoTypeSingularDataset {
			id = "datasets/" + id
		}
		rows = append(rows, []string{id, formats, quants, formatBytes(repo.SizeBytes + repo.LfsBytes), source, repo.UpdatedAt.Local().Format("2006-01-02")})
	}
	return a.table([]string{"REPO", "FORMATS", "QUANTS", "SIZE", "SOURCE", "UPDATED"}, rows)
}

// applyRepoFilter sets one key=value Arsenal filter.
func applyRepoFilter(p *gen.ListReposParams, f string) error {
	k, v, ok := strings.Cut(f, "=")
	if !ok || v == "" {
		return usagef("--filter %q: use key=value, such as format=gguf", f)
	}
	str := func(dst **string) error { *dst = &v; return nil }
	num := func(dst **int64) error {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return usagef("--filter %s: %q is not a whole number", k, v)
		}
		*dst = &n
		return nil
	}
	switch k {
	case "format":
		list := deref(p.Format)
		p.Format = ptr(append(list, strings.Split(v, ",")...))
		return nil
	case "task":
		return str(&p.Task)
	case "library":
		return str(&p.Library)
	case "quant":
		return str(&p.Quant)
	case "licence", "license":
		return str(&p.Licence)
	case "ns", "namespace":
		return str(&p.Ns)
	case "modality":
		return str(&p.Modality)
	case "language":
		return str(&p.Language)
	case "size_category":
		return str(&p.SizeCategory)
	case "params_min":
		return num(&p.ParamsMin)
	case "params_max":
		return num(&p.ParamsMax)
	case "context_min":
		return num(&p.ContextMin)
	case "size_max":
		return num(&p.SizeMax)
	case "mirror":
		b, err := strconv.ParseBool(v)
		if err != nil {
			return usagef("--filter mirror must be true or false")
		}
		p.Mirror = &b
		return nil
	}
	return usagef("--filter %q: unknown key %q", f, k)
}

func (a *app) discover(ctx context.Context, q, upstream, typ, sort string, filters []string, limit int) error {
	t, err := repoTypeArg(typ)
	if err != nil {
		return err
	}
	if limit < 1 || limit > 500 {
		return usagef("--limit must be between 1 and 500")
	}
	params := &gen.DiscoverParams{Upstream: &upstream, Limit: &limit}
	if q != "" {
		params.Search = &q
	}
	if sort != "" {
		s := gen.DiscoverParamsSort(sort)
		params.Sort = &s
	}
	if len(filters) > 0 {
		params.Filter = &filters
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	r, err := c.api.DiscoverWithResponse(ctx, t, params)
	if err != nil {
		return c.reach(err)
	}
	if err := check(r.HTTPResponse, r.Body, http.StatusOK); err != nil {
		return err
	}
	if a.json {
		return a.printJSON(r.JSON200)
	}
	if len(r.JSON200.Items) == 0 {
		a.printf("No Repos on %s match.\n", upstream)
		return nil
	}
	rows := make([][]string, 0, len(r.JSON200.Items))
	for _, it := range r.JSON200.Items {
		id := it.Id
		if it.Type == gen.RepoTypeSingularDataset {
			id = "datasets/" + id
		}
		rows = append(rows, []string{id, orDash(deref(it.PipelineTag)), strconv.FormatInt(deref(it.Downloads), 10), arsenalLabel(it.Arsenal, it.LocalSha != nil)})
	}
	return a.table([]string{"REPO", "TASK", "DOWNLOADS", "ARSENAL"}, rows)
}

// arsenalLabel describes a Repo's state in the Arsenal.
func arsenalLabel(s gen.ArsenalState, updateAvailable bool) string {
	l := "-"
	switch s {
	case gen.ArsenalStateFull:
		l = "In arsenal"
	case gen.ArsenalStatePartial:
		l = "Partly in arsenal"
	}
	if updateAvailable && s != gen.ArsenalStateNone {
		l += " (update available)"
	}
	return l
}
