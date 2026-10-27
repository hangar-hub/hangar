package cli

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/hangar-hub/hangar/internal/api/gen"
	"github.com/hangar-hub/hangar/internal/config"
	"github.com/hangar-hub/hangar/internal/jobs"
	"github.com/hangar-hub/hangar/internal/model"
)

// Shell completion (`hangar completion <shell>`). Completions that need the
// Instance, such as Repo IDs and Job IDs, ask it with the saved login, or
// --url and --token when they are on the command line. A completion that
// cannot reach the Instance offers nothing, so the shell never shows an
// error.

// shellCompletionTimeout bounds the requests one completion sends, so a slow or
// unreachable Instance does not hold up the shell.
const shellCompletionTimeout = 3 * time.Second

// shellCompletionLimit is how many Repos or Jobs a completion asks for.
const shellCompletionLimit = 100

// noFiles is the directive for values that are not paths.
const noFiles = cobra.ShellCompDirectiveNoFileComp

// values completes a flag from a fixed list.
func values(choices ...string) cobra.CompletionFunc {
	return cobra.FixedCompletions(choices, noFiles)
}

// dirs completes a command's only argument, a folder.
func dirs(_ *cobra.Command, argv []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
	if len(argv) > 0 {
		return nil, noFiles
	}
	return nil, cobra.ShellCompDirectiveFilterDirs
}

// repoTypes completes --type for Repo IDs without a prefix.
var repoTypes = values(string(model.RepoTypeModel), string(model.RepoTypeDataset))

// completionClient returns a client and a context bounded by
// shellCompletionTimeout, or false when there is no Instance to ask.
func (a *app) completionClient(cmd *cobra.Command) (*client, context.Context, context.CancelFunc, bool) {
	c, err := a.client()
	if err != nil {
		cobra.CompDebugln("no Instance: "+err.Error(), false)
		return nil, nil, nil, false
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), shellCompletionTimeout)
	return c, ctx, cancel, true
}

// hasPrefixFold reports whether s starts with prefix, ignoring case.
func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// completeRepo completes a Repo ID with the Repos the user can see in the
// Arsenal. typ is the command's --type. With quants, after a colon it
// completes the Repo's quants instead, as in
// bartowski/Llama-3.2-3B-Instruct-GGUF:Q4_K_M. format, when set, offers only
// Repos with files in that format.
func (a *app) completeRepo(cmd *cobra.Command, toComplete, typ, format string, quants bool) ([]cobra.Completion, cobra.ShellCompDirective) {
	if i := strings.LastIndexByte(toComplete, ':'); i >= 0 {
		if !quants {
			return nil, noFiles
		}
		return a.completeQuant(cmd, toComplete[:i], toComplete[i+1:], typ)
	}
	t := model.RepoTypeModel
	if pt, err := model.ParseRepoType(typ); err == nil {
		t = pt
	}
	// prefix keeps a datasets/ or models/ prefix the user typed.
	prefix, q := "", toComplete
	for _, p := range []struct {
		prefix string
		typ    model.RepoType
	}{{"datasets/", model.RepoTypeDataset}, {"models/", model.RepoTypeModel}} {
		if strings.HasPrefix(q, p.prefix) {
			prefix, q, t = p.prefix, strings.TrimPrefix(q, p.prefix), p.typ
		}
	}
	c, ctx, cancel, ok := a.completionClient(cmd)
	if !ok {
		return nil, noFiles
	}
	defer cancel()
	// The Arsenal's q matches a substring of namespace/name, ignoring case.
	params := &gen.ListReposParams{Type: ptr(gen.RepoTypePlural(t.Plural())), Limit: ptr(shellCompletionLimit), Sort: ptr(gen.ListReposParamsSortName)}
	if q != "" {
		params.Q = &q
	}
	if format != "" {
		params.Format = &[]string{format}
	}
	r, err := c.api.ListReposWithResponse(ctx, params)
	if err == nil {
		err = check(r.HTTPResponse, r.Body, http.StatusOK)
	}
	if err != nil {
		cobra.CompDebugln("list Repos: "+err.Error(), false)
		return nil, noFiles
	}
	var out []cobra.Completion
	for _, repo := range r.JSON200.Items {
		if hasPrefixFold(repo.Id, q) {
			out = append(out, prefix+repo.Id)
		}
	}
	return out, noFiles
}

// completeQuant completes the quant after `repo:` with the quants the
// Instance found in the Repo, with each one's size.
func (a *app) completeQuant(cmd *cobra.Command, repoArg, toComplete, typ string) ([]cobra.Completion, cobra.ShellCompDirective) {
	ref, err := parseRepoRef(repoArg, typ, false)
	if err != nil || ref.ID.Type != model.RepoTypeModel {
		return nil, noFiles
	}
	c, ctx, cancel, ok := a.completionClient(cmd)
	if !ok {
		return nil, noFiles
	}
	defer cancel()
	list, err := c.quants(ctx, ref.ID, "")
	if err != nil {
		cobra.CompDebugln("list quants: "+err.Error(), false)
		return nil, noFiles
	}
	var out []cobra.Completion
	for _, g := range list.Items {
		if !hasPrefixFold(g.Name, toComplete) {
			continue
		}
		desc := formatBytes(g.Size)
		if g.Present {
			desc += ", in the Arsenal"
		}
		out = append(out, cobra.CompletionWithDesc(repoArg+":"+g.Name, desc))
	}
	return out, noFiles
}

// completeRepoArg completes a command's only argument, a Repo ID.
func (a *app) completeRepoArg(typ *string, format string, quants bool) cobra.CompletionFunc {
	return func(cmd *cobra.Command, argv []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
		if len(argv) > 0 {
			return nil, noFiles
		}
		return a.completeRepo(cmd, toComplete, *typ, format, quants)
	}
}

// completeRepoFlag completes a flag whose value is a Repo ID.
func (a *app) completeRepoFlag(typ *string) cobra.CompletionFunc {
	return func(cmd *cobra.Command, _ []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
		return a.completeRepo(cmd, toComplete, *typ, "", false)
	}
}

// completeDatasetConfig completes --config, or with splits --split, from
// the Configs and Splits of the dataset Repo in the first argument.
func (a *app) completeDatasetConfig(typ, revision *string, splits bool) cobra.CompletionFunc {
	return func(cmd *cobra.Command, argv []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
		if len(argv) == 0 {
			return nil, noFiles
		}
		ref, err := parseRepoRef(argv[0], *typ, true)
		if err != nil || ref.ID.Type != model.RepoTypeDataset {
			return nil, noFiles
		}
		c, ctx, cancel, ok := a.completionClient(cmd)
		if !ok {
			return nil, noFiles
		}
		defer cancel()
		cfgs, err := c.datasetConfigs(ctx, ref.ID, *revision)
		if err != nil {
			cobra.CompDebugln("list Configs: "+err.Error(), false)
			return nil, noFiles
		}
		var out []cobra.Completion
		for _, cfg := range cfgs.Configs {
			if !splits {
				out = append(out, cfg.Config)
				continue
			}
			for _, s := range cfg.Splits {
				if !slices.Contains(out, s.Split) {
					out = append(out, s.Split)
				}
			}
		}
		return out, noFiles
	}
}

// completeJob completes a Job ID with the user's Jobs that have not
// finished, described by kind, target and status.
func (a *app) completeJob(cmd *cobra.Command, argv []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
	if len(argv) > 0 {
		return nil, noFiles
	}
	c, ctx, cancel, ok := a.completionClient(cmd)
	if !ok {
		return nil, noFiles
	}
	defer cancel()
	r, err := c.api.ListJobsWithResponse(ctx, &gen.ListJobsParams{Mine: ptr(true), Limit: ptr(shellCompletionLimit)})
	if err == nil {
		err = check(r.HTTPResponse, r.Body, http.StatusOK)
	}
	if err != nil {
		cobra.CompDebugln("list Jobs: "+err.Error(), false)
		return nil, noFiles
	}
	var out []cobra.Completion
	for _, j := range r.JSON200.Items {
		id := j.Id.String()
		if terminal(j.Status) || !strings.HasPrefix(id, toComplete) {
			continue
		}
		desc := j.Kind
		if t := jobTarget(j); t != "-" {
			desc += " " + t
		}
		out = append(out, cobra.CompletionWithDesc(id, desc+", "+string(j.Status)))
	}
	return out, noFiles
}

// jobStatuses completes jobs list --status.
var jobStatuses = values(string(gen.JobStatusPending), string(gen.JobStatusRunning),
	string(gen.JobStatusSucceeded), string(gen.JobStatusFailed), string(gen.JobStatusCancelled))

// completeJobKind completes jobs list --kind.
func completeJobKind(*cobra.Command, []string, string) ([]cobra.Completion, cobra.ShellCompDirective) {
	var out []cobra.Completion
	for _, k := range jobs.AllKinds() {
		out = append(out, string(k))
	}
	return out, noFiles
}

// completeLMInstall completes lmstudio remove with the Repos installed from
// Hangar, from the local list of installs.
func completeLMInstall(_ *cobra.Command, argv []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
	if len(argv) > 0 {
		return nil, noFiles
	}
	st, _, err := loadLMState()
	if err != nil {
		return nil, noFiles
	}
	var out []cobra.Completion
	for _, inst := range st.Installs {
		id := inst.Repo
		if inst.Type == string(model.RepoTypeDataset) {
			id = "datasets/" + id
		}
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out, noFiles
}

// repoFilterKeys are the keys --filter takes for the Arsenal, in the order
// search's help lists them; applyRepoFilter accepts each one.
var repoFilterKeys = []string{"format", "task", "library", "quant", "licence", "ns", "params_min", "params_max",
	"context_min", "size_max", "mirror", "modality", "language", "size_category"}

// completeSearchFilter completes search --filter with the Arsenal's filter
// keys, as key=. An Upstream's filters are its tags, which are not listed.
func completeSearchFilter(cmd *cobra.Command, _ []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
	if cmd.Flags().Changed("upstream") || strings.Contains(toComplete, "=") {
		return nil, noFiles
	}
	out := make([]cobra.Completion, 0, len(repoFilterKeys))
	for _, k := range repoFilterKeys {
		out = append(out, k+"=")
	}
	return out, noFiles | cobra.ShellCompDirectiveNoSpace
}

// completeSearchSort completes search --sort with the Arsenal's orders, or
// an Upstream's with --upstream.
func completeSearchSort(cmd *cobra.Command, _ []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
	if cmd.Flags().Changed("upstream") {
		return []cobra.Completion{string(gen.DiscoverParamsSortTrending), string(gen.DiscoverParamsSortDownloads),
			string(gen.DiscoverParamsSortLikes), string(gen.DiscoverParamsSortLastModified)}, noFiles
	}
	return []cobra.Completion{string(gen.ListReposParamsSortUpdated), string(gen.ListReposParamsSortName),
		string(gen.ListReposParamsSortSize), string(gen.ListReposParamsSortParameters),
		string(gen.ListReposParamsSortDownloads), string(gen.ListReposParamsSortLikes)}, noFiles
}

// CompleteConfigFlags sets up completion for the --config and --set flags
// that commands reading the server configuration take: YAML files for
// --config, and the configuration keys, as key=, for --set.
func CompleteConfigFlags(cmd *cobra.Command) {
	_ = cmd.MarkFlagFilename("config", "yaml", "yml")
	_ = cmd.RegisterFlagCompletionFunc("set", completeConfigKey)
}

// completeConfigKey completes --set with every configuration key that
// --set can name, described by the first sentence of its description. Keys
// of map entries and list items, which need an ID or an index, are left out.
func completeConfigKey(_ *cobra.Command, _ []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
	if strings.Contains(toComplete, "=") {
		return nil, noFiles
	}
	var out []cobra.Completion
	for _, f := range config.Fields() {
		if strings.ContainsAny(f.Key, "<[") {
			continue
		}
		desc, _, _ := strings.Cut(strings.Join(strings.Fields(f.Description), " "), ". ")
		out = append(out, cobra.CompletionWithDesc(f.Key+"=", strings.TrimSuffix(desc, ".")))
	}
	return out, noFiles | cobra.ShellCompDirectiveNoSpace
}

// SetDefaultCompletions turns off file name completion for every argument
// and flag value under root that has no completion of its own, so the shell
// offers files only where a command takes a path. Call it once every command
// is added.
func SetDefaultCompletions(root *cobra.Command) {
	for _, c := range root.Commands() {
		setDefaultCompletions(c)
	}
}

func setDefaultCompletions(c *cobra.Command) {
	if c.ValidArgsFunction == nil && len(c.ValidArgs) == 0 && !c.HasSubCommands() {
		c.ValidArgsFunction = cobra.NoFileCompletions
	}
	c.LocalFlags().VisitAll(func(f *pflag.Flag) {
		if f.Value.Type() == "bool" {
			return
		}
		if _, ok := f.Annotations[cobra.BashCompFilenameExt]; ok {
			return
		}
		if _, ok := f.Annotations[cobra.BashCompSubdirsInDir]; ok {
			return
		}
		if _, ok := c.GetFlagCompletionFunc(f.Name); ok {
			return
		}
		_ = c.RegisterFlagCompletionFunc(f.Name, cobra.NoFileCompletions)
	})
	for _, s := range c.Commands() {
		setDefaultCompletions(s)
	}
}
