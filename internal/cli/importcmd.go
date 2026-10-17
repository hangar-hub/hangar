package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jamesagarside/hangar/internal/api/gen"
	"github.com/jamesagarside/hangar/internal/model"
)

// importOptions are the flags of `hangar import`.
type importOptions struct {
	FromHFCache  string
	FromDir      string
	FromLMStudio string
	FromOllama   bool
	Repo         string
	Namespace    string
	Upstream     string
	Offline      bool
	DryRun       bool
	Private      bool
	OllamaModels string
}

// importResult reports one imported Repo.
type importResult struct {
	Repo     string   `json:"repo"`
	Source   string   `json:"source"`
	Mode     string   `json:"mode"` // mirror or local
	Commit   string   `json:"commit,omitempty"`
	Uploaded []string `json:"uploaded"`
	Skipped  []string `json:"skipped,omitempty"`
	Error    string   `json:"error,omitempty"`
}

func (a *app) importCommand() *cobra.Command {
	var o importOptions
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Add models you already have on disk to the Arsenal without downloading them again",
		Long: `Adds content that is already on this machine to the Arsenal. Choose one source:

  --from-hf-cache [path]   a Hugging Face cache (default ~/.cache/huggingface/hub).
                           Each Repo becomes a Mirror of the Upstream Repo with
                           the same ID: the Instance fetches its history, and
                           the CLI uploads the cached files whose SHA-256
                           matches. With --offline, or when the Upstream does
                           not have the Repo, each becomes a local Repo with
                           one Commit instead.
  --from-dir <dir>         a folder, uploaded to --repo as one Commit, as
                           hangar push does.
  --from-lmstudio [path]   LM Studio's models folder (<publisher>/<repo>/<files>).
                           Folders that match an Upstream Repo of the same name
                           become Partial mirrors; the rest become local Repos.
  --from-ollama            Ollama's models. Each model and tag becomes a local
                           Repo in your Namespace (or --namespace) with its GGUF
                           file and its template, system and params files.

Uploads go through the Instance's API, so you need write access to the
Namespaces involved. A file that fails to upload does not stop the other
files or Repos: files that went up into a Mirror are added to it, and the
results name what failed, so running the command again sends only that.
Use --dry-run to see what would be imported.`,
		Example: `  hangar import --from-hf-cache
  hangar import --from-hf-cache /mnt/models/hf --offline --namespace acme
  hangar import --from-dir ./weights --repo acme/my-model
  hangar import --from-lmstudio
  hangar import --from-ollama --namespace acme`,
		Args: args(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, argv []string) error {
			a := a.with(cmd)
			f := cmd.Flags()
			// --from-hf-cache and --from-lmstudio take an optional path, so
			// `--from-hf-cache /path` arrives as a positional argument.
			if len(argv) == 1 {
				switch {
				case f.Changed("from-hf-cache") && o.FromHFCache == "default":
					o.FromHFCache = argv[0]
				case f.Changed("from-lmstudio") && o.FromLMStudio == "default":
					o.FromLMStudio = argv[0]
				default:
					return usagef("unexpected argument %q", argv[0])
				}
			}
			sources := 0
			for _, n := range []string{"from-hf-cache", "from-dir", "from-lmstudio", "from-ollama"} {
				if f.Changed(n) {
					sources++
				}
			}
			if sources != 1 {
				return usagef("choose one of --from-hf-cache, --from-dir, --from-lmstudio and --from-ollama")
			}
			var results []importResult
			var err error
			switch {
			case f.Changed("from-hf-cache"):
				results, err = a.importHFCache(cmd.Context(), o)
			case f.Changed("from-dir"):
				results, err = a.importDir(cmd.Context(), o)
			case f.Changed("from-lmstudio"):
				results, err = a.importLMStudio(cmd.Context(), o)
			default:
				results, err = a.importOllama(cmd.Context(), o)
			}
			if err != nil {
				return err
			}
			return a.reportImport(results, o.DryRun)
		},
	}
	a.clientFlags(cmd)
	f := cmd.Flags()
	f.StringVar(&o.FromHFCache, "from-hf-cache", "", "import a Hugging Face cache")
	f.Lookup("from-hf-cache").NoOptDefVal = "default"
	f.StringVar(&o.FromDir, "from-dir", "", "import a folder as one Commit to --repo")
	f.StringVar(&o.FromLMStudio, "from-lmstudio", "", "import LM Studio's models folder")
	f.Lookup("from-lmstudio").NoOptDefVal = "default"
	f.BoolVar(&o.FromOllama, "from-ollama", false, "import Ollama's models")
	f.StringVar(&o.Repo, "repo", "", "with --from-dir, the Repo to import into, such as acme/model or datasets/acme/data")
	f.StringVar(&o.Namespace, "namespace", "", "Namespace for local Repos (default: the Repo's own, or yours for Ollama models)")
	f.StringVar(&o.Upstream, "upstream", "huggingface", "Upstream that Mirrors track")
	f.BoolVar(&o.Offline, "offline", false, "never ask the Upstream; create local Repos")
	f.BoolVar(&o.DryRun, "dry-run", false, "only list what would be imported")
	f.BoolVar(&o.Private, "private", false, "make new local Repos private")
	f.StringVar(&o.OllamaModels, "ollama-models", "", "Ollama's models folder (default: $OLLAMA_MODELS or ~/.ollama/models)")
	return cmd
}

func (a *app) reportImport(results []importResult, dryRun bool) error {
	failed := 0
	for _, r := range results {
		if r.Error != "" {
			failed++
		}
	}
	if a.json {
		if err := a.printJSON(results); err != nil {
			return err
		}
		if failed > 0 {
			return fmt.Errorf("%d of %d imports failed", failed, len(results))
		}
		return nil
	}
	if len(results) == 0 {
		a.printf("Nothing to import.\n")
		return nil
	}
	rows := make([][]string, 0, len(results))
	for _, r := range results {
		state := fmt.Sprintf("%d files uploaded", len(r.Uploaded))
		if dryRun {
			state = fmt.Sprintf("%d files", len(r.Uploaded))
		}
		if len(r.Skipped) > 0 {
			state += fmt.Sprintf(", %d not matched", len(r.Skipped))
		}
		if r.Error != "" {
			state = "failed: " + r.Error
		}
		rows = append(rows, []string{r.Repo, r.Mode, r.Source, state})
	}
	if err := a.table([]string{"REPO", "AS", "FROM", "RESULT"}, rows); err != nil {
		return err
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d imports failed", failed, len(results))
	}
	return nil
}

// importDir uploads a folder as one Commit.
func (a *app) importDir(ctx context.Context, o importOptions) ([]importResult, error) {
	if o.Repo == "" {
		return nil, usagef("--from-dir needs --repo")
	}
	ref, err := parseRepoRef(o.Repo, "", false)
	if err != nil {
		return nil, err
	}
	files, outside, err := walkFolder(o.FromDir, nil, nil)
	if err != nil {
		return nil, err
	}
	a.warnOutside(o.FromDir, outside)
	res := importResult{Repo: display(ref.ID), Source: o.FromDir, Mode: "local", Uploaded: paths(files)}
	if o.DryRun {
		return []importResult{res}, nil
	}
	c, err := a.client()
	if err != nil {
		return nil, err
	}
	if err := a.ensureRepo(ctx, c, ref.ID, true, o.Private); err != nil {
		return nil, err
	}
	pr, err := a.commitFiles(ctx, c, ref.ID, files, pushOptions{Message: "Import with hangar import"})
	if err != nil {
		return nil, err
	}
	res.Commit = pr.Commit
	return []importResult{res}, nil
}

func paths(files []localFile) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.Path
	}
	return out
}

// importHFCache imports every snapshot in a Hugging Face cache: for each
// Repo, the snapshot of `main` if cached, otherwise the newest.
func (a *app) importHFCache(ctx context.Context, o importOptions) ([]importResult, error) {
	root := o.FromHFCache
	if root == "default" || root == "" {
		root = defaultHFCacheDir()
	}
	snaps, err := hfCache{root: root}.snapshots()
	if err != nil {
		return nil, err
	}
	best := map[string]cachedSnapshot{}
	for _, s := range snaps {
		k := s.ID.Path()
		cur, ok := best[k]
		if !ok || containsString(s.Refs, "main") && !containsString(cur.Refs, "main") {
			best[k] = s
		}
	}
	keys := make([]string, 0, len(best))
	for k := range best {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var c *client
	if !o.DryRun {
		if c, err = a.client(); err != nil {
			return nil, err
		}
	}
	var out []importResult
	for _, k := range keys {
		s := best[k]
		files := make([]localFile, 0, len(s.Files))
		for p, disk := range s.Files {
			st, err := os.Stat(disk)
			if err != nil {
				continue
			}
			lf := localFile{Path: p, Disk: disk, Size: st.Size()}
			if name := filepath.Base(disk); model.IsSHA256(name) {
				lf.SHA256 = name // blobs of LFS files are named by their SHA-256.
			}
			files = append(files, lf)
		}
		sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
		src := filepath.Join(root, repoFolderName(s.ID), "snapshots", shortSHA(s.Commit))
		a.warnOutside(filepath.Join(root, repoFolderName(s.ID)), s.Outside)
		out = append(out, a.importSnapshot(ctx, c, s.ID, s.Commit, files, src, o))
	}
	return out, nil
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// importSnapshot imports one set of files, as a Mirror if the Upstream has
// the Repo and --offline is not set, otherwise as a local Repo.
func (a *app) importSnapshot(ctx context.Context, c *client, id model.RepoID, commit string, files []localFile, src string, o importOptions) importResult {
	res := importResult{Repo: display(id), Source: src, Mode: "mirror"}
	if o.Offline {
		res.Mode = "local"
	}
	if o.DryRun {
		res.Uploaded = paths(files)
		return res
	}
	if !o.Offline {
		r, err := a.importAsMirror(ctx, c, id, commit, files, o.Upstream)
		var failed *uploadFailures
		if err == nil || errors.As(err, &failed) {
			// Some files may have failed to upload while the rest were
			// added to the Mirror: report both, and never fall back to a
			// local Repo because a failed file's error was a 404.
			r.Source = src
			if err != nil {
				r.Error = err.Error()
			}
			return r
		}
		if !IsNotFound(err) {
			res.Error = err.Error()
			return res
		}
		a.logf("%s is not on %s; importing it as a local Repo.\n", display(id), o.Upstream)
	}
	res.Mode = "local"
	target := id
	if o.Namespace != "" {
		target.Namespace = o.Namespace
		res.Repo = display(target)
	}
	if err := a.ensureRepo(ctx, c, target, true, o.Private); err != nil {
		res.Error = err.Error()
		return res
	}
	pr, err := a.commitFiles(ctx, c, target, files, pushOptions{Message: "Import with hangar import"})
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.Commit = pr.Commit
	res.Uploaded = paths(files)
	return res
}

// importAsMirror plans a Mirror of the upstream Repo with history only,
// then uploads the local files whose SHA-256 matches an LFS object the
// Mirror references but does not hold. It returns a not-found error if
// the Upstream does not have the Repo.
func (a *app) importAsMirror(ctx context.Context, c *client, id model.RepoID, commit string, files []localFile, upstream string) (importResult, error) {
	res := importResult{Repo: display(id), Mode: "mirror"}
	repo, err := c.getRepo(ctx, id)
	switch {
	case IsNotFound(err):
		// The Instance asks the Upstream, which can take a while over a
		// slow link: say what is happening, so the command does not look
		// stuck (bellatrix finding 10).
		stop := a.waitStatus(fmt.Sprintf("Looking up %s on %s", display(id), upstream))
		_, err := c.discoverDetail(ctx, id, upstream, commit)
		stop()
		if err != nil {
			return res, err
		}
		body := gen.PullCreate{Upstream: upstream, Repo: id.String(), Type: singular(id),
			Selection: &gen.Selection{Paths: &[]string{}}}
		if commit != "" {
			body.Revision = &commit
		}
		job, err := c.startPull(ctx, body)
		if err != nil {
			return res, err
		}
		if _, err := a.watchJob(ctx, c, job.Id, "Creating a Mirror of "+display(id)); err != nil {
			return res, err
		}
	case err != nil:
		return res, err
	case !repo.Mirror:
		return res, fmt.Errorf("%s is already in the Arsenal as a local Repo", display(id))
	}
	rev := commit
	if rev == "" {
		if repo == nil {
			if repo, err = c.getRepo(ctx, id); err != nil {
				return res, err
			}
		}
		rev = repo.DefaultBranch
	}
	gotCommit, entries, err := c.tree(ctx, id, rev)
	if err != nil {
		return res, err
	}
	res.Commit = gotCommit
	wanted := map[string]gen.TreeEntry{}
	for _, e := range entries {
		if e.Lfs != nil && !e.Present {
			wanted[e.Path] = e
		}
	}
	var candidates, unhashed []localFile
	var hashBytes int64
	for _, f := range files {
		if _, ok := wanted[f.Path]; !ok {
			continue
		}
		if f.SHA256 == "" {
			unhashed = append(unhashed, f)
			hashBytes += f.Size
			continue
		}
		candidates = append(candidates, f)
	}
	if len(unhashed) > 0 {
		hp := newProgress(a.stderr, "Hashing "+display(id), hashBytes, a.json)
		if err := hashFiles(ctx, unhashed, hp); err != nil {
			return res, err
		}
		hp.finish()
		candidates = append(candidates, unhashed...)
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].Path < candidates[j].Path })
	}
	var upload []localFile
	for _, f := range candidates {
		e := wanted[f.Path]
		if f.SHA256 != e.Lfs.Oid || f.Size != e.Lfs.Size {
			res.Skipped = append(res.Skipped, f.Path)
			continue
		}
		upload = append(upload, f)
	}
	var total int64
	for _, f := range upload {
		total += f.Size
	}
	if len(upload) > 0 && !a.json {
		a.logf("Uploading %s to %s.\n", countFiles(len(upload), total), display(id))
	}
	up := &uploader{c: c, id: id, prog: newProgress(a.stderr, "Uploading "+display(id), total, a.json), waiting: a.waitStatus}
	_, err = up.upload(ctx, upload)
	up.prog.finish()
	var failed *uploadFailures
	if err != nil && !errors.As(err, &failed) {
		return res, err
	}
	if failed != nil {
		// The other files went up, so add them to the Mirror now and
		// report the ones that failed; running the import again sends
		// only those.
		skip := map[string]bool{}
		for _, p := range failed.paths() {
			skip[p] = true
		}
		var sent []localFile
		for _, f := range upload {
			if !skip[f.Path] {
				sent = append(sent, f)
			}
		}
		upload = sent
	}
	res.Uploaded = paths(upload)
	if len(upload) > 0 {
		// Pull the uploaded files into the Mirror: the Instance holds their
		// objects now, so the Pull marks them present without a transfer,
		// within the Namespace's quota.
		sel := res.Uploaded
		job, err := c.pullIntoMirror(ctx, id, gen.Selection{Paths: &sel})
		if err != nil {
			return res, err
		}
		if _, err := a.watchJob(ctx, c, job.Id, "Adding the files to "+display(id)); err != nil {
			return res, err
		}
	}
	if failed != nil {
		return res, failed
	}
	return res, nil
}

// countFiles describes a number of files and their size, such as
// "3 files, 15.0 GiB".
func countFiles(n int, size int64) string {
	if n == 1 {
		return "1 file, " + formatBytes(size)
	}
	return fmt.Sprintf("%d files, %s", n, formatBytes(size))
}

// importLMStudio imports every <publisher>/<repo> folder in LM Studio's
// models folder.
func (a *app) importLMStudio(ctx context.Context, o importOptions) ([]importResult, error) {
	root := o.FromLMStudio
	if root == "default" || root == "" {
		root = lmStudioModelsDir("")
	}
	pubs, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read LM Studio's models folder: %w", err)
	}
	var c *client
	if !o.DryRun {
		if c, err = a.client(); err != nil {
			return nil, err
		}
	}
	var out []importResult
	for _, p := range pubs {
		if !p.IsDir() || strings.HasPrefix(p.Name(), ".") {
			continue
		}
		repos, err := os.ReadDir(filepath.Join(root, p.Name()))
		if err != nil {
			return nil, err
		}
		for _, r := range repos {
			if !r.IsDir() || strings.HasPrefix(r.Name(), ".") {
				continue
			}
			dir := filepath.Join(root, p.Name(), r.Name())
			id, err := model.ParseRepoID(model.RepoTypeModel, sanitiseName(p.Name())+"/"+sanitiseName(r.Name()))
			if err != nil {
				out = append(out, importResult{Repo: p.Name() + "/" + r.Name(), Source: dir, Error: err.Error()})
				continue
			}
			files, outside, err := walkFolder(dir, nil, nil)
			if err != nil {
				return nil, err
			}
			a.warnOutside(dir, outside)
			if len(files) == 0 {
				continue
			}
			out = append(out, a.importSnapshot(ctx, c, id, "", files, dir, o))
		}
	}
	return out, nil
}

// ollamaManifest is the part of an Ollama image manifest the import reads.
type ollamaManifest struct {
	Layers []struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Size      int64  `json:"size"`
	} `json:"layers"`
}

// ollamaModelsDir returns $OLLAMA_MODELS or ~/.ollama/models.
func ollamaModelsDir(flag string) string {
	if flag != "" {
		return flag
	}
	if d := os.Getenv("OLLAMA_MODELS"); d != "" {
		return d
	}
	return filepath.Join(ollamaDir(), "models")
}

// importOllama imports each model and tag as a local Repo.
func (a *app) importOllama(ctx context.Context, o importOptions) ([]importResult, error) {
	root := ollamaModelsDir(o.OllamaModels)
	manifests := filepath.Join(root, "manifests")
	type entry struct{ name, tag, path string }
	var found []entry
	err := filepath.WalkDir(manifests, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(manifests, p)
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) < 3 { // host/[namespace/]model/tag
			return nil
		}
		model := parts[len(parts)-2]
		if ns := parts[len(parts)-3]; ns != "library" && len(parts) >= 4 {
			model = ns + "-" + model
		}
		found = append(found, entry{name: model, tag: parts[len(parts)-1], path: p})
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil, notFoundf("no Ollama models at %s", root)
	}
	if err != nil {
		return nil, err
	}
	var c *client
	ns := o.Namespace
	if !o.DryRun {
		if c, err = a.client(); err != nil {
			return nil, err
		}
		if ns == "" {
			me, err := c.me(ctx)
			if err != nil {
				return nil, err
			}
			ns = me.Username
		}
	}
	if ns == "" {
		ns = "<you>"
	}
	var out []importResult
	for _, e := range found {
		name := sanitiseName(e.name)
		if e.tag != "latest" {
			name += "-" + sanitiseName(e.tag)
		}
		res := importResult{Repo: ns + "/" + name, Source: e.name + ":" + e.tag, Mode: "local"}
		files, err := ollamaFiles(root, e.path, sanitiseName(e.name), sanitiseName(e.tag))
		if err != nil {
			res.Error = err.Error()
			out = append(out, res)
			continue
		}
		res.Uploaded = paths(files)
		if o.DryRun {
			out = append(out, res)
			continue
		}
		id, err := model.ParseRepoID(model.RepoTypeModel, ns+"/"+name)
		if err == nil {
			err = a.ensureRepo(ctx, c, id, true, o.Private)
		}
		if err == nil {
			var pr *pushResult
			pr, err = a.commitFiles(ctx, c, id, files, pushOptions{Message: "Import " + e.name + ":" + e.tag + " from Ollama"})
			if pr != nil {
				res.Commit = pr.Commit
			}
		}
		if err != nil {
			res.Error = err.Error()
		}
		out = append(out, res)
	}
	return out, nil
}

// ollamaFiles maps an Ollama manifest's layers to Repo files.
func ollamaFiles(root, manifestPath, name, tag string) ([]localFile, error) {
	b, err := os.ReadFile(manifestPath) // #nosec G304 -- inside Ollama's models folder.
	if err != nil {
		return nil, err
	}
	var m ollamaManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("read the manifest: %w", err)
	}
	base := name
	if tag != "latest" {
		base += "-" + tag
	}
	var files []localFile
	for _, l := range m.Layers {
		var p string
		switch l.MediaType {
		case "application/vnd.ollama.image.model":
			p = base + ".gguf"
		case "application/vnd.ollama.image.projector":
			p = "mmproj-" + base + ".gguf"
		case "application/vnd.ollama.image.template":
			p = "template"
		case "application/vnd.ollama.image.system":
			p = "system"
		case "application/vnd.ollama.image.params":
			p = "params"
		case "application/vnd.ollama.image.license":
			p = "LICENSE"
		default:
			continue
		}
		hexDigest, ok := strings.CutPrefix(l.Digest, "sha256:")
		if !ok || !model.IsSHA256(hexDigest) {
			return nil, fmt.Errorf("layer %s has an unexpected digest", l.MediaType)
		}
		disk := filepath.Join(root, "blobs", "sha256-"+hexDigest)
		st, err := os.Stat(disk)
		if err != nil {
			return nil, fmt.Errorf("blob for %s: %w", p, err)
		}
		files = append(files, localFile{Path: p, Disk: disk, Size: st.Size(), SHA256: hexDigest})
	}
	if len(files) == 0 || !strings.HasSuffix(files[0].Path, ".gguf") && !hasGGUF(files) {
		return nil, fmt.Errorf("the manifest has no GGUF model layer")
	}
	return files, nil
}

func hasGGUF(files []localFile) bool {
	for _, f := range files {
		if strings.HasSuffix(f.Path, ".gguf") && !strings.HasPrefix(f.Path, "mmproj-") {
			return true
		}
	}
	return false
}

var nameUnsafe = regexp.MustCompile(`[^A-Za-z0-9_.\-]+`)

// sanitiseName turns a folder or model name into a valid Repo name part.
func sanitiseName(s string) string {
	s = strings.Trim(nameUnsafe.ReplaceAllString(s, "-"), "-.")
	if len(s) > 96 {
		s = s[:96]
	}
	return s
}
