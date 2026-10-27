package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/hangar-hub/hangar/internal/api/gen"
	"github.com/hangar-hub/hangar/internal/model"
)

// Link modes for LM Studio installs.
const (
	linkSymlink  = "symlink"
	linkHardlink = "hardlink"
	linkCopy     = "copy"
)

// defaultQuantOrder is the preference order when no quant is given, the
// same as the Ollama endpoint's `latest` (docs/spec/08-local-tools.md).
var defaultQuantOrder = []string{"Q4_K_M", "Q4_K_S", "Q5_K_M", "Q4_0", "Q5_K_S", "Q6_K", "Q8_0", "IQ4_XS", "Q3_K_M", "BF16", "F16"}

// lmInstall records one `hangar lmstudio install`.
type lmInstall struct {
	Repo        string    `json:"repo"`
	Type        string    `json:"type"`
	URL         string    `json:"url"`
	Revision    string    `json:"revision"`
	Commit      string    `json:"commit"`
	Quant       string    `json:"quant,omitempty"`
	Dir         string    `json:"dir"`
	Files       []string  `json:"files"`
	LinkMode    string    `json:"link_mode"`
	CacheDir    string    `json:"cache_dir"`
	InstalledAt time.Time `json:"installed_at"`
}

// lmState is ~/.config/hangar/lmstudio.json, the list of installs.
type lmState struct {
	Installs []lmInstall `json:"installs"`
}

func lmStatePath() (string, error) {
	d, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "lmstudio.json"), nil
}

func loadLMState() (*lmState, string, error) {
	p, err := lmStatePath()
	if err != nil {
		return nil, "", err
	}
	s := &lmState{}
	b, err := os.ReadFile(p) // #nosec G304 -- the user's own state file.
	if errors.Is(err, os.ErrNotExist) {
		return s, p, nil
	}
	if err != nil {
		return nil, p, err
	}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, p, fmt.Errorf("read %s: %w", p, err)
	}
	return s, p, nil
}

func (s *lmState) save(p string) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

// lmStudioHome finds LM Studio's home folder the way LM Studio does: the
// path in ~/.lmstudio-home-pointer, else ~/.cache/lm-studio if it exists,
// else ~/.lmstudio.
func lmStudioHome() string {
	home := userHome()
	if b, err := os.ReadFile(filepath.Join(home, ".lmstudio-home-pointer")); err == nil { // #nosec G304 -- fixed name in the home folder.
		if p := strings.TrimSpace(string(b)); p != "" {
			return p
		}
	}
	if st, err := os.Stat(filepath.Join(home, ".cache", "lm-studio")); err == nil && st.IsDir() {
		return filepath.Join(home, ".cache", "lm-studio")
	}
	return filepath.Join(home, ".lmstudio")
}

// lmStudioModelsDir returns the models folder: the flag, else the
// downloadsFolder in LM Studio's settings.json, else <home>/models.
func lmStudioModelsDir(flag string) string {
	if flag != "" {
		return flag
	}
	home := lmStudioHome()
	if b, err := os.ReadFile(filepath.Join(home, "settings.json")); err == nil { // #nosec G304 -- LM Studio's settings file.
		var s struct {
			DownloadsFolder string `json:"downloadsFolder"`
		}
		if json.Unmarshal(b, &s) == nil && s.DownloadsFolder != "" {
			return s.DownloadsFolder
		}
	}
	return filepath.Join(home, "models")
}

func defaultLinkMode() string {
	if runtime.GOOS == "windows" {
		return linkHardlink
	}
	return linkSymlink
}

func (a *app) lmstudioCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "lmstudio",
		Short: "Install Hangar models into LM Studio",
		Long: `Installs models from the Instance into LM Studio's models folder, laid out as
<namespace>/<name>/<files>. Files are downloaded once into the local Hugging
Face cache and linked into LM Studio's folder, so the disk is used once even
when Python libraries use the same model.

The models folder is --lmstudio-dir if given; otherwise the downloadsFolder in
LM Studio's settings.json, in LM Studio's home folder (the path in
~/.lmstudio-home-pointer, else ~/.cache/lm-studio, else ~/.lmstudio);
otherwise the models folder in that home folder.`,
		Args: args(cobra.NoArgs),
	}
	a.clientFlags(cmd)
	var lmDir string
	cmd.PersistentFlags().StringVar(&lmDir, "lmstudio-dir", "", "LM Studio's models folder")
	_ = cmd.MarkPersistentFlagDirname("lmstudio-dir")

	var linkMode, revision, cacheDir string
	install := &cobra.Command{
		Use:   "install <repo>[:<quant>]",
		Short: "Download a model and link it into LM Studio",
		Long: `Downloads a model into the local cache and links it into LM Studio. For a GGUF
Repo, installs one quant group and any mmproj-*.gguf projector files; without
a quant, picks Q4_K_M, or the closest available. Other Repos, such as MLX,
are installed whole.`,
		Example: `  hangar lmstudio install bartowski/Llama-3.2-3B-Instruct-GGUF:Q4_K_M
  hangar lmstudio install mlx-community/SmolLM-135M-Instruct-4bit --link-mode copy`,
		Args:              args(cobra.ExactArgs(1)),
		ValidArgsFunction: a.completeRepoArg(ptr(string(model.RepoTypeModel)), "", true),
		RunE: func(cmd *cobra.Command, argv []string) error {
			a := a.with(cmd)
			ref, err := parseRepoRef(argv[0], "model", true)
			if err != nil {
				return err
			}
			inst, err := a.lmInstall(cmd.Context(), ref, lmStudioModelsDir(lmDir), linkMode, revision, cacheDir)
			if err != nil {
				return err
			}
			if a.json {
				return a.printJSON(inst)
			}
			a.printf("Installed %s (%d files) into %s.\n", inst.Repo, len(inst.Files), inst.Dir)
			return nil
		},
	}
	install.Flags().StringVar(&linkMode, "link-mode", defaultLinkMode(), "how files get into LM Studio's folder: symlink, hardlink or copy")
	install.Flags().StringVar(&revision, "revision", "", "branch, tag or Commit SHA (default: the default branch)")
	install.Flags().StringVar(&cacheDir, "cache-dir", "", "Hugging Face cache folder (default: $HF_HUB_CACHE or ~/.cache/huggingface/hub)")
	_ = install.RegisterFlagCompletionFunc("link-mode", values(linkSymlink, linkHardlink, linkCopy))
	_ = install.MarkFlagDirname("cache-dir")

	list := &cobra.Command{
		Use:   "list",
		Short: "List models installed from Hangar, and whether a newer Revision exists",
		Args:  args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.with(cmd).lmList(cmd.Context())
		},
	}

	var prune bool
	remove := &cobra.Command{
		Use:   "remove <repo>",
		Short: "Remove a model's links from LM Studio",
		Long: `Removes the files hangar lmstudio install put in LM Studio's folder. With
--prune, also deletes the model's files from the local cache when no other
install uses them.`,
		Args:              args(cobra.ExactArgs(1)),
		ValidArgsFunction: completeLMInstall,
		RunE: func(cmd *cobra.Command, argv []string) error {
			a := a.with(cmd)
			ref, err := parseRepoRef(argv[0], "model", false)
			if err != nil {
				return err
			}
			return a.lmRemove(ref.ID, prune)
		},
	}
	remove.Flags().BoolVar(&prune, "prune", false, "also delete cached files no other install uses")
	cmd.AddCommand(install, list, remove)
	return cmd
}

// lmInstall downloads and links a model.
func (a *app) lmInstall(ctx context.Context, ref repoRef, lmDir, linkMode, revision, cacheDir string) (*lmInstall, error) {
	switch linkMode {
	case linkSymlink, linkHardlink, linkCopy:
	default:
		return nil, usagef("--link-mode must be symlink, hardlink or copy")
	}
	if cacheDir == "" {
		cacheDir = defaultHFCacheDir()
	}
	c, err := a.client()
	if err != nil {
		return nil, err
	}
	if ref.Quant == "" {
		var groups []gen.QuantGroup
		q, err := c.quants(ctx, ref.ID, revision)
		switch {
		case err == nil:
			groups = q.Items
		case IsNotFound(err):
			// Not in the Arsenal yet: ask the Upstream.
			if d, derr := c.discoverDetail(ctx, ref.ID, "huggingface", revision); derr == nil {
				groups = deref(d.Quants)
			}
		default:
			return nil, err
		}
		if len(groups) > 0 {
			ref.Quant = preferredQuant(groups)
			a.logf("No quant given; installing %s.\n", ref.Quant)
		}
	}
	res, err := a.pull(ctx, ref, pullOptions{Revision: revision, Upstream: "huggingface", CacheDir: cacheDir})
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(lmDir, ref.ID.Namespace, ref.ID.Name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	inst := &lmInstall{Repo: ref.ID.String(), Type: string(ref.ID.Type), URL: c.base.String(), Revision: res.Revision,
		Commit: res.Commit, Quant: ref.Quant, Dir: dir, LinkMode: linkMode, CacheDir: cacheDir, InstalledAt: time.Now().UTC()}
	for _, f := range res.Files {
		blob, err := filepath.EvalSymlinks(f.LocalPath)
		if err != nil {
			return nil, err
		}
		dst := filepath.Join(dir, filepath.FromSlash(f.Path))
		if err := placeFile(blob, dst, linkMode); err != nil {
			return nil, fmt.Errorf("link %s: %w", f.Path, err)
		}
		inst.Files = append(inst.Files, f.Path)
	}
	st, p, err := loadLMState()
	if err != nil {
		return nil, err
	}
	kept := st.Installs[:0]
	for _, i := range st.Installs {
		if i.Repo != inst.Repo || i.Dir != inst.Dir {
			kept = append(kept, i)
		}
	}
	st.Installs = append(kept, *inst)
	return inst, st.save(p)
}

// preferredQuant picks a quant group by the default order, else the
// smallest, then the first by name: the group the Ollama registry serves
// for "latest" (index.PreferredQuant).
func preferredQuant(groups []gen.QuantGroup) string {
	for _, want := range defaultQuantOrder {
		for _, g := range groups {
			if strings.EqualFold(g.Name, want) {
				return g.Name
			}
		}
	}
	best := groups[0]
	for _, g := range groups[1:] {
		if g.Size < best.Size || g.Size == best.Size && g.Name < best.Name {
			best = g
		}
	}
	return best.Name
}

// placeFile puts blob at dst with the link mode, replacing what is there.
func placeFile(blob, dst, mode string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	switch mode {
	case linkSymlink:
		return os.Symlink(blob, dst)
	case linkHardlink:
		if err := os.Link(blob, dst); err != nil {
			return fmt.Errorf("%w (the cache and LM Studio's folder must be on the same disk; use --link-mode copy)", err)
		}
		return nil
	}
	return copyFile(blob, dst)
}

// lmStatus is one row of `hangar lmstudio list`.
type lmStatus struct {
	lmInstall
	Latest          string `json:"latest_commit,omitempty"`
	UpdateAvailable bool   `json:"update_available"`
	Missing         int    `json:"missing_files"`
	Error           string `json:"error,omitempty"`
}

func (a *app) lmList(ctx context.Context) error {
	st, _, err := loadLMState()
	if err != nil {
		return err
	}
	out := make([]lmStatus, 0, len(st.Installs))
	clients := map[string]*client{}
	for _, inst := range st.Installs {
		s := lmStatus{lmInstall: inst}
		for _, f := range inst.Files {
			if _, err := os.Stat(filepath.Join(inst.Dir, filepath.FromSlash(f))); err != nil {
				s.Missing++
			}
		}
		c, ok := clients[inst.URL]
		if !ok {
			c, err = a.clientFor(inst.URL)
			if err != nil {
				return err
			}
			clients[inst.URL] = c
		}
		t, _ := model.ParseRepoType(inst.Type)
		id, err := model.ParseRepoID(t, inst.Repo)
		if err == nil {
			s.Latest, err = c.resolve(ctx, id, inst.Revision)
		}
		if err != nil {
			s.Error = err.Error()
		} else {
			s.UpdateAvailable = s.Latest != inst.Commit
		}
		out = append(out, s)
	}
	if a.json {
		return a.printJSON(out)
	}
	if len(out) == 0 {
		a.printf("Nothing is installed from Hangar.\n")
		return nil
	}
	rows := make([][]string, 0, len(out))
	for _, s := range out {
		state := "up to date"
		switch {
		case s.Error != "":
			state = "unknown: " + s.Error
		case s.UpdateAvailable:
			state = "newer Revision " + shortSHA(s.Latest)
		}
		if s.Missing > 0 {
			state += fmt.Sprintf(", %d files missing", s.Missing)
		}
		rows = append(rows, []string{s.Repo, orDash(s.Quant), s.Revision, shortSHA(s.Commit), state, s.Dir})
	}
	return a.table([]string{"REPO", "QUANT", "REVISION", "COMMIT", "STATUS", "FOLDER"}, rows)
}

// clientFor builds a client for an Instance URL, using the saved Token if
// the URL matches the login.
func (a *app) clientFor(rawURL string) (*client, error) {
	b := *a
	if b.url == "" {
		b.url = rawURL
	}
	return b.client()
}

// resolve returns the Commit a Revision points at.
func (c *client) resolve(ctx context.Context, id model.RepoID, rev string) (string, error) {
	params := &gen.GetTreeRootParams{Limit: ptr(1)}
	r, err := c.api.GetTreeRootWithResponse(ctx, plural(id), id.Namespace, id.Name, rev, params)
	if err != nil {
		return "", c.reach(err)
	}
	if err := check(r.HTTPResponse, r.Body, 200); err != nil {
		return "", err
	}
	return r.JSON200.Commit, nil
}

func (a *app) lmRemove(id model.RepoID, prune bool) error {
	st, p, err := loadLMState()
	if err != nil {
		return err
	}
	var removed []lmInstall
	kept := st.Installs[:0]
	for _, i := range st.Installs {
		if i.Repo == id.String() && i.Type == string(id.Type) {
			removed = append(removed, i)
		} else {
			kept = append(kept, i)
		}
	}
	if len(removed) == 0 {
		return notFoundf("%s is not installed from Hangar", display(id))
	}
	st.Installs = kept
	for _, inst := range removed {
		for _, f := range inst.Files {
			if err := os.Remove(filepath.Join(inst.Dir, filepath.FromSlash(f))); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		removeEmptyDirs(inst.Dir)
		removeEmptyDirs(filepath.Dir(inst.Dir))
	}
	if err := st.save(p); err != nil {
		return err
	}
	freed := int64(0)
	if prune {
		for _, inst := range removed {
			n, err := pruneCache(hfCache{root: inst.CacheDir}, id, inst.Commit, st.Installs)
			if err != nil {
				return err
			}
			freed += n
		}
	}
	if a.json {
		return a.printJSON(map[string]any{"removed": removed, "freed_bytes": freed})
	}
	a.printf("Removed %s from LM Studio.", display(id))
	if prune {
		a.printf(" Freed %s in the cache.", formatBytes(freed))
	}
	a.printf("\n")
	return nil
}

// removeEmptyDirs removes dir and its empty subfolders, if they are empty.
func removeEmptyDirs(dir string) {
	var dirs []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	})
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	for _, d := range dirs {
		_ = os.Remove(d) // fails, as it should, unless empty.
	}
}

// pruneCache deletes a Commit's snapshot from the cache unless another
// install uses it, then deletes the Repo's blobs no snapshot links to. It
// returns the bytes freed.
func pruneCache(h hfCache, id model.RepoID, commit string, others []lmInstall) (int64, error) {
	for _, o := range others {
		if o.Repo == id.String() && o.Type == string(id.Type) && o.Commit == commit && o.CacheDir == h.root {
			return 0, nil
		}
	}
	snap := filepath.Join(h.repoDir(id), "snapshots", commit)
	if err := os.RemoveAll(snap); err != nil {
		return 0, err
	}
	used := map[string]bool{}
	_ = filepath.WalkDir(filepath.Join(h.repoDir(id), "snapshots"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // an unreadable snapshot keeps no blob in use.
		}
		if d.IsDir() {
			return nil
		}
		if real, err := filepath.EvalSymlinks(p); err == nil {
			used[real] = true
		}
		return nil
	})
	blobs := filepath.Join(h.repoDir(id), "blobs")
	entries, err := os.ReadDir(blobs)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	realBlobs, err := filepath.EvalSymlinks(blobs)
	if err != nil {
		return 0, err
	}
	var freed int64
	for _, e := range entries {
		p := filepath.Join(realBlobs, e.Name())
		if used[p] {
			continue
		}
		if info, err := e.Info(); err == nil {
			freed += info.Size()
		}
		if err := os.Remove(p); err != nil {
			return freed, err
		}
	}
	return freed, nil
}
