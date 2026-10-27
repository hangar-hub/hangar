package cli

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/api/gen"
	"github.com/hangar-hub/hangar/internal/cli/clitest"
	"github.com/hangar-hub/hangar/internal/model"
)

// completion is what `hangar __complete` printed: the choices, without
// their descriptions, and the directive.
type completion struct {
	choices   []string
	descs     []string
	directive cobra.ShellCompDirective
}

// complete runs cobra's hidden __complete command, as a shell does on Tab.
func (e *env) complete(args ...string) completion {
	e.t.Helper()
	r := e.ok(append([]string{cobra.ShellCompRequestCmd}, args...)...)
	lines := strings.Split(strings.TrimRight(r.stdout, "\n"), "\n")
	last := lines[len(lines)-1]
	require.True(e.t, strings.HasPrefix(last, ":"), "no directive in %q", r.stdout)
	d, err := strconv.Atoi(last[1:])
	require.NoError(e.t, err)
	c := completion{directive: cobra.ShellCompDirective(d)}
	for _, l := range lines[:len(lines)-1] {
		choice, desc, _ := strings.Cut(l, "\t")
		c.choices = append(c.choices, choice)
		c.descs = append(c.descs, desc)
	}
	return c
}

func TestCompleteRepoIDs(t *testing.T) {
	e := newEnv(t)
	e.login()
	gguf := mustID(t, model.RepoTypeModel, "bartowski/Tiny-GGUF")
	e.inst.AddRepo(gguf,
		clitest.File{Path: "Tiny-Q4_K_M.gguf", Content: blobOf("q4", 2000)},
		clitest.File{Path: "Tiny-Q8_0.gguf", Content: blobOf("q8", 3000)})
	e.inst.AddRepo(mustID(t, model.RepoTypeModel, "acme/second"), clitest.File{Path: "README.md", Content: []byte("# Second\n")})
	e.inst.AddRepo(mustID(t, model.RepoTypeDataset, "acme/reviews"), clitest.File{Path: "data.csv", Content: []byte("a,b\n")})

	c := e.complete("info", "")
	require.ElementsMatch(t, []string{"acme/second", "bartowski/Tiny-GGUF"}, c.choices, "models only, without a prefix or --type")
	require.Equal(t, cobra.ShellCompDirectiveNoFileComp, c.directive)
	require.Equal(t, []string{"bartowski/Tiny-GGUF"}, e.complete("info", "BART").choices, "case is ignored")
	require.Empty(t, e.complete("info", "tiny").choices, "a substring that is not a prefix does not match")
	require.Equal(t, []string{"datasets/acme/reviews"}, e.complete("info", "datasets/").choices)
	require.Equal(t, []string{"acme/reviews"}, e.complete("info", "--type", "dataset", "acme/").choices)
	require.Equal(t, []string{"models/acme/second"}, e.complete("pull", "models/acme/").choices)
	require.Empty(t, e.complete("info", "acme/second", "").choices, "info takes one Repo")

	// After a colon, the quants, for the commands that take one.
	c = e.complete("pull", "bartowski/Tiny-GGUF:")
	require.Equal(t, []string{"bartowski/Tiny-GGUF:Q4_K_M", "bartowski/Tiny-GGUF:Q8_0"}, c.choices)
	require.Equal(t, []string{"2.0 KiB, in the Arsenal", "2.9 KiB, in the Arsenal"}, c.descs)
	require.Equal(t, []string{"bartowski/Tiny-GGUF:Q8_0"}, e.complete("ollama", "install", "bartowski/Tiny-GGUF:q8").choices)
	require.Len(t, e.complete("lmstudio", "install", "bartowski/Tiny-GGUF:").choices, 2)
	require.Empty(t, e.complete("info", "bartowski/Tiny-GGUF:").choices, "info takes no quant")
	require.Empty(t, e.complete("pull", "acme/missing:").choices)

	// push takes a folder, then a Repo.
	require.Equal(t, cobra.ShellCompDirectiveFilterDirs, e.complete("push", "").directive)
	require.Equal(t, []string{"acme/second"}, e.complete("push", "./out", "acme/s").choices)
	require.Equal(t, []string{"acme/second"}, e.complete("import", "--from-dir", "./out", "--repo", "acme/s").choices)
}

func TestCompleteWithoutAnInstance(t *testing.T) {
	e := newEnv(t)
	// Not signed in: nothing to offer, and no error for the shell to show.
	c := e.complete("pull", "")
	require.Empty(t, c.choices)
	require.Equal(t, cobra.ShellCompDirectiveNoFileComp, c.directive)
	require.Empty(t, e.complete("jobs", "cancel", "").choices)

	// An Instance that cannot be reached.
	c = e.complete("info", "--url", "http://127.0.0.1:1", "")
	require.Empty(t, c.choices)
	require.Equal(t, cobra.ShellCompDirectiveNoFileComp, c.directive)
}

func TestCompleteJobIDs(t *testing.T) {
	e := newEnv(t)
	e.login()
	id, _ := ggufRepo(e)
	e.ok("pull", id.String()+":Q8_0", "--server-only")

	// A Pull that has not started yet: the fake Instance moves a Job on
	// only when it is polled.
	base, err := url.Parse(e.inst.URL)
	require.NoError(t, err)
	c, err := newClient(base, clitest.Token, "test", newHTTPClient())
	require.NoError(t, err)
	j, err := c.startPull(context.Background(), gen.PullCreate{Upstream: "huggingface", Repo: id.String(), Type: gen.RepoTypeSingularModel})
	require.NoError(t, err)

	for _, sub := range []string{"watch", "cancel"} {
		got := e.complete("jobs", sub, "")
		require.Equal(t, []string{j.Id.String()}, got.choices, "only the Job that has not finished")
		require.Equal(t, []string{"pull bartowski/Tiny-GGUF, pending"}, got.descs)
	}
	require.Empty(t, e.complete("jobs", "watch", "ffffffff").choices)
	require.Empty(t, e.complete("jobs", "watch", j.Id.String(), "").choices)
}

func TestCompleteDatasetConfigsAndSplits(t *testing.T) {
	e := newEnv(t)
	e.login()
	r := e.inst.AddRepo(mustID(t, model.RepoTypeDataset, "acme/reviews"), clitest.File{Path: "data.csv", Content: []byte("a,b\n")})
	r.Configs = []gen.DatasetConfig{
		{Config: "default", Default: true, Splits: []gen.DatasetSplit{{Split: "train"}, {Split: "test"}}},
		{Config: "extra", Splits: []gen.DatasetSplit{{Split: "train"}}},
	}

	require.Equal(t, []string{"default", "extra"}, e.complete("pull", "datasets/acme/reviews", "--config", "").choices)
	require.Equal(t, []string{"train", "test"}, e.complete("pull", "acme/reviews", "--type", "dataset", "--split", "").choices)
	require.Empty(t, e.complete("pull", "acme/reviews", "--config", "").choices, "a model ID has no Configs")
	require.Empty(t, e.complete("pull", "--config", "").choices, "no Repo yet")
}

func TestCompleteLMStudioInstalls(t *testing.T) {
	e := newEnv(t)
	p, err := lmStatePath()
	require.NoError(t, err)
	st := &lmState{Installs: []lmInstall{
		{Repo: "bartowski/Tiny-GGUF", Type: "model", Quant: "Q4_K_M"},
		{Repo: "bartowski/Tiny-GGUF", Type: "model", Quant: "Q8_0"},
		{Repo: "acme/reviews", Type: "dataset"},
	}}
	require.NoError(t, st.save(p))

	c := e.complete("lmstudio", "remove", "")
	require.Equal(t, []string{"bartowski/Tiny-GGUF", "datasets/acme/reviews"}, c.choices)
	require.Equal(t, cobra.ShellCompDirectiveNoFileComp, c.directive)
}

func TestCompleteFlagValues(t *testing.T) {
	e := newEnv(t)
	require.Equal(t, []string{"model", "dataset"}, e.complete("search", "--type", "").choices)
	require.Equal(t, []string{"symlink", "hardlink", "copy"}, e.complete("lmstudio", "install", "x", "--link-mode", "").choices)
	require.Equal(t, []string{"bash", "zsh", "fish", "powershell"}, e.complete("env", "--shell", "").choices)
	require.Contains(t, e.complete("jobs", "list", "--kind", "").choices, "pull")
	require.Equal(t, []string{"pending", "running", "succeeded", "failed", "cancelled"}, e.complete("jobs", "list", "--status", "").choices)
	require.Contains(t, e.complete("search", "--sort", "").choices, "parameters")
	require.Contains(t, e.complete("search", "--upstream", "--sort", "").choices, "trending")

	c := e.complete("search", "--filter", "")
	require.Equal(t, cobra.ShellCompDirectiveNoFileComp|cobra.ShellCompDirectiveNoSpace, c.directive)
	require.Contains(t, c.choices, "quant=")
	require.Empty(t, e.complete("search", "--filter", "quant=").choices)
	require.Empty(t, e.complete("search", "--upstream", "--filter", "").choices, "an Upstream's filters are its tags")

	c = e.complete("admin", "backup", "--set", "")
	require.Equal(t, cobra.ShellCompDirectiveNoFileComp|cobra.ShellCompDirectiveNoSpace, c.directive)
	require.Contains(t, c.choices, "database.dsn=")
	for _, k := range c.choices {
		require.NotContains(t, k, "<", "map entries need an ID")
		require.NotContains(t, k, "[", "list items need an index")
	}
	require.Equal(t, cobra.ShellCompDirectiveFilterFileExt, e.complete("admin", "backup", "--config", "").directive)
}

func TestCompleteFoldersAndDefaults(t *testing.T) {
	e := newEnv(t)
	for _, args := range [][]string{
		{"pull", "x", "--to", ""},
		{"pull", "x", "--cache-dir", ""},
		{"lmstudio", "install", "x", "--lmstudio-dir", ""},
		{"import", "--from-dir", ""},
		{"import", "--from-hf-cache", ""},
		{"admin", "backup", "--output", ""},
		{"admin", "restore", ""},
	} {
		require.Equal(t, cobra.ShellCompDirectiveFilterDirs, e.complete(args...).directive, args)
	}
	// Everything else that is not a path offers no file names.
	for _, args := range [][]string{
		{"whoami", ""},
		{"search", ""},
		{"pull", "x", "--revision", ""},
		{"pull", "x", "--include", ""},
		{"push", "./out", "acme/x", "--message", ""},
		{"login", "--url", ""},
		{"admin", "restore", "/backups/b", ""},
	} {
		require.Equal(t, cobra.ShellCompDirectiveNoFileComp, e.complete(args...).directive, args)
	}
}

func TestRepoFilterKeysAreAccepted(t *testing.T) {
	for _, k := range repoFilterKeys {
		require.NoError(t, applyRepoFilter(&gen.ListReposParams{}, k+"=1"), k)
	}
}
