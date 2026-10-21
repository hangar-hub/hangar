package cli

import (
	"context"
	"crypto/sha1" // #nosec G505 -- git object IDs are SHA-1.
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hangar-hub/hangar/internal/api/gen"
	"github.com/hangar-hub/hangar/internal/model"
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
	Only         []string
}

// importResult reports one imported Repo.
type importResult struct {
	Repo     string   `json:"repo"`
	Source   string   `json:"source"`
	Mode     string   `json:"mode"` // mirror or local
	Commit   string   `json:"commit,omitempty"`
	Uploaded []string `json:"uploaded"`
	// Skipped lists the paths of the files that matched nothing in the
	// Mirror; NotMatched says why, file by file.
	Skipped    []string         `json:"skipped,omitempty"`
	NotMatched []notMatchedFile `json:"not_matched,omitempty"`
	Error      string           `json:"error,omitempty"`
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
                           the CLI uploads the files of every cached snapshot
                           whose content matches a file the Mirror is missing.
                           With --offline, or when the Upstream does not have
                           the Repo, each becomes a local Repo with one Commit
                           of the snapshot of main, or else the newest.
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
Files whose content matches nothing in a Mirror are named after the results,
with the reason. Use --dry-run to see what would be imported.

To import some models and not the rest, give --only once for each, as it
appears in the source: a Repo ID such as bartowski/Qwen2.5-7B-Instruct-GGUF
for a Hugging Face cache, a <publisher>/<repo> folder for LM Studio, or a
model and tag such as llama3.2:1b for Ollama (llama3.2 means llama3.2:latest).
Patterns can use * and ?, which do not match /, and ignore case. A pattern
that matches nothing stops the import before anything is uploaded.`,
		Example: `  hangar import --from-hf-cache
  hangar import --from-hf-cache /mnt/models/hf --offline --namespace acme
  hangar import --from-hf-cache --only 'bartowski/*' --only Qwen/Qwen2.5-7B-Instruct --dry-run
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
			if f.Changed("from-dir") && len(o.Only) > 0 {
				return usagef("--only does not apply to --from-dir, which imports one folder")
			}
			for _, p := range o.Only {
				if _, err := path.Match(p, ""); err != nil {
					return usagef("--only %q is not a valid pattern", p)
				}
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
	f.StringArrayVar(&o.Only, "only", nil, "only import models matching this pattern, such as 'bartowski/*' or llama3.2:1b (repeatable)")
	return cmd
}

// importFilter selects the models a bulk import takes, from --only.
type importFilter struct {
	patterns []string
	matched  []bool
}

func newImportFilter(patterns []string) *importFilter {
	return &importFilter{patterns: patterns, matched: make([]bool, len(patterns))}
}

// match reports whether a model passes the filter, given the names it is
// known by in its source. With no patterns, every model passes.
func (f *importFilter) match(names ...string) bool {
	if len(f.patterns) == 0 {
		return true
	}
	ok := false
	for i, p := range f.patterns {
		for _, n := range names {
			if m, _ := path.Match(strings.ToLower(p), strings.ToLower(n)); m {
				f.matched[i] = true
				ok = true
			}
		}
	}
	return ok
}

// unmatched returns an error naming the patterns no model matched, so a
// typo stops the import instead of leaving a model out.
func (f *importFilter) unmatched(source string) error {
	var missed []string
	for i, p := range f.patterns {
		if !f.matched[i] {
			missed = append(missed, fmt.Sprintf("%q", p))
		}
	}
	if len(missed) == 0 {
		return nil
	}
	return notFoundf("nothing in %s matches --only %s", source, strings.Join(missed, ", "))
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
	a.reportNotMatched(results)
	if failed > 0 {
		return fmt.Errorf("%d of %d imports failed", failed, len(results))
	}
	return nil
}

// reportNotMatched names each file that matched nothing in its Mirror,
// with the reason, after the results table.
func (a *app) reportNotMatched(results []importResult) {
	first := true
	for _, r := range results {
		for _, nm := range r.NotMatched {
			if first {
				a.printf("\nNot matched:\n")
				first = false
			}
			name := nm.Path
			if nm.Snapshot != "" {
				name += " (snapshot " + shortSHA(nm.Snapshot) + ")"
			}
			a.printf("  %s: %s: %s\n", r.Repo, name, nm.Reason)
		}
	}
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

// importHFCache imports every Repo in a Hugging Face cache. A Mirror is
// filled from the files of every cached snapshot of the Repo; a local Repo
// gets the snapshot of `main` if cached, otherwise the newest.
func (a *app) importHFCache(ctx context.Context, o importOptions) ([]importResult, error) {
	root := o.FromHFCache
	if root == "default" || root == "" {
		root = defaultHFCacheDir()
	}
	snaps, err := hfCache{root: root}.snapshots()
	if err != nil {
		return nil, err
	}
	only := newImportFilter(o.Only)
	byRepo := map[string][]cachedSnapshot{}
	for _, s := range snaps {
		if !only.match(display(s.ID)) {
			continue
		}
		k := s.ID.Path()
		byRepo[k] = append(byRepo[k], s)
	}
	if err := only.unmatched(root); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(byRepo))
	for k := range byRepo {
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
		list := byRepo[k]
		sortSnapshots(list)
		s := list[0]
		repoDir := filepath.Join(root, repoFolderName(s.ID))
		src := importSource{id: s.ID, commit: s.Commit, files: snapshotFiles(s), src: repoDir}
		if len(list) == 1 {
			src.src = filepath.Join(repoDir, "snapshots", shortSHA(s.Commit))
		}
		outside := map[string]bool{}
		for i, other := range list {
			for _, p := range other.Outside {
				outside[p] = true
			}
			if i > 0 {
				src.others = append(src.others, cachedFiles{commit: other.Commit, files: snapshotFiles(other)})
			}
		}
		a.warnOutside(repoDir, sortedKeys(outside))
		out = append(out, a.importSnapshot(ctx, c, src, o))
	}
	return out, nil
}

// sortSnapshots puts the snapshot a Repo is imported at first: the one
// `main` points at if it is cached, otherwise the newest, by when its
// folder last changed. The rest follow, newest first.
func sortSnapshots(list []cachedSnapshot) {
	sort.SliceStable(list, func(i, j int) bool {
		mi, mj := containsString(list[i].Refs, "main"), containsString(list[j].Refs, "main")
		if mi != mj {
			return mi
		}
		if !list[i].ModTime.Equal(list[j].ModTime) {
			return list[i].ModTime.After(list[j].ModTime)
		}
		return list[i].Commit < list[j].Commit
	})
}

// snapshotFiles lists the files of a cached snapshot, by path.
func snapshotFiles(s cachedSnapshot) []localFile {
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
	return files
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// importSource is one Repo to import: the files of a cached snapshot or a
// folder and, from a Hugging Face cache, the Repo's other cached
// snapshots, whose files a Mirror is filled from too.
type importSource struct {
	id  model.RepoID
	src string
	// commit is the snapshot's Commit; empty for a folder, which is
	// matched against the Mirror's default branch.
	commit string
	files  []localFile
	others []cachedFiles
}

// cachedFiles are the files of one other cached snapshot.
type cachedFiles struct {
	commit string
	files  []localFile
}

// importSnapshot imports one set of files, as a Mirror if the Upstream has
// the Repo and --offline is not set, otherwise as a local Repo.
func (a *app) importSnapshot(ctx context.Context, c *client, s importSource, o importOptions) importResult {
	id := s.id
	res := importResult{Repo: display(id), Source: s.src, Mode: "mirror"}
	if o.Offline {
		res.Mode = "local"
	}
	if o.DryRun {
		res.Uploaded = paths(s.files)
		return res
	}
	if !o.Offline {
		r, err := a.importAsMirror(ctx, c, s, o.Upstream)
		if !errors.Is(err, errNotOnUpstream) {
			// Some files may have failed to upload while the rest were
			// added to the Mirror: report both. Only an Upstream without
			// the Repo makes a local Repo: any other error, such as a 404
			// for a Commit or an upload, is reported, because a Mirror
			// is read-only (issue #28).
			r.Source = s.src
			if err != nil {
				r.Error = err.Error()
			}
			return r
		}
		a.logf("%s is not on %s; importing it as a local Repo.\n", display(id), o.Upstream)
	}
	res.Mode = "local"
	if len(s.others) > 0 {
		a.logf("%s has %d cached snapshots; the local Repo gets the files of %s.\n", display(id), len(s.others)+1, shortSHA(s.commit))
	}
	target := id
	if o.Namespace != "" {
		target.Namespace = o.Namespace
		res.Repo = display(target)
	}
	if err := a.ensureRepo(ctx, c, target, true, o.Private); err != nil {
		res.Error = err.Error()
		return res
	}
	pr, err := a.commitFiles(ctx, c, target, s.files, pushOptions{Message: "Import with hangar import"})
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.Commit = pr.Commit
	res.Uploaded = paths(s.files)
	return res
}

// errNotOnUpstream means the Upstream does not have a Repo, so the import
// makes a local Repo instead of a Mirror.
var errNotOnUpstream = errors.New("not on the Upstream")

// importAsMirror plans a Mirror of the upstream Repo with history only,
// then uploads the local files whose content matches an LFS object the
// Mirror references but does not hold, at the snapshot's Commit, the
// default branch or another cached snapshot's Commit. Files are matched by
// content, not path: the SHA-256 of LFS files and the git blob ID of the
// others. It returns an error wrapping errNotOnUpstream if the Upstream
// does not have the Repo.
func (a *app) importAsMirror(ctx context.Context, c *client, s importSource, upstream string) (importResult, error) {
	id, commit := s.id, s.commit
	res := importResult{Repo: display(id), Mode: "mirror"}
	pulled := false // whether this import has pulled the Mirror's history
	repo, err := c.getRepo(ctx, id)
	switch {
	case IsNotFound(err):
		// The Instance asks the Upstream, which can take a while over a
		// slow link: say what is happening, so the command does not look
		// stuck (bellatrix finding 10).
		stop := a.waitStatus(fmt.Sprintf("Looking up %s on %s", display(id), upstream))
		_, err := c.discoverDetail(ctx, id, upstream, commit)
		stop()
		if IsNotFound(err) {
			return res, fmt.Errorf("%w: %w", errNotOnUpstream, err)
		}
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
		pulled = true
		if repo, err = c.getRepo(ctx, id); err != nil {
			return res, err
		}
	case err != nil:
		return res, err
	case !repo.Mirror:
		return res, fmt.Errorf("%s is already in the Arsenal as a local Repo", display(id))
	}
	rev := commit
	if rev == "" {
		rev = repo.DefaultBranch
	}
	gotCommit, entries, err := c.tree(ctx, id, rev)
	if IsNotFound(err) && commit != "" && !pulled {
		// The Mirror has not fetched this Commit yet, such as a snapshot
		// cached after its last sync: pull its history, then look again.
		if err := a.pullHistory(ctx, c, id, commit); err != nil {
			return res, err
		}
		gotCommit, entries, err = c.tree(ctx, id, rev)
	}
	if err != nil {
		return res, err
	}
	res.Commit = gotCommit
	idx := newMirrorIndex()
	idx.add(gotCommit, entries)
	// Match against the default branch and the other cached snapshots
	// too, so a file of an older snapshot whose content the Mirror has
	// under another Commit or path counts as matched (issue #29). The
	// files of a snapshot whose Commit the Mirror does not have are
	// matched against the rest.
	var revs []string
	if commit != "" {
		revs = append(revs, repo.DefaultBranch)
	}
	for _, o := range s.others {
		revs = append(revs, o.commit)
	}
	for _, r := range revs {
		if idx.has(r) {
			continue
		}
		got, more, err := c.tree(ctx, id, r)
		if IsNotFound(err) {
			continue
		}
		if err != nil {
			return res, err
		}
		idx.add(got, more)
	}

	cands := importCandidates(s)
	if err := a.hashCandidates(ctx, id, cands, idx); err != nil {
		return res, err
	}
	// send maps each object to upload to a local file that holds it;
	// sentBy lists the local files each object stands for.
	send := map[string]localFile{}
	sentBy := map[string][]string{}
	for _, f := range cands {
		if o := idx.lfs[f.SHA256]; f.SHA256 != "" && o != nil && o.size == f.Size {
			if o.present {
				continue // already in the Mirror
			}
			if _, ok := send[f.SHA256]; !ok {
				// Upload under the Mirror's path, so its .gitattributes
				// rules apply.
				send[f.SHA256] = localFile{Path: o.path, Disk: f.Disk, Size: f.Size, SHA256: f.SHA256}
			}
			sentBy[f.SHA256] = append(sentBy[f.SHA256], f.Path)
			continue
		}
		if f.gitOID != "" && idx.git[f.gitOID] {
			continue // a regular file, which comes with the history
		}
		nm := notMatchedFile{Path: f.Path, Reason: idx.reason(f.Path)}
		if len(s.others) > 0 {
			nm.Snapshot = f.snapshot
		}
		res.Skipped = append(res.Skipped, f.Path)
		res.NotMatched = append(res.NotMatched, nm)
	}
	upload := make([]localFile, 0, len(send))
	var total int64
	for _, f := range send {
		upload = append(upload, f)
		total += f.Size
	}
	sort.Slice(upload, func(i, j int) bool { return upload[i].Path < upload[j].Path })
	if len(upload) > 0 && !a.json {
		a.logf("Uploading %s to %s.\n", countFiles(len(upload), total), display(id))
	}
	up := &uploader{c: c, id: id, prog: newProgress(a.stderr, "Uploading "+display(id), total, a.json), waiting: a.waitStatus, note: a.logf}
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
	// Pull the uploaded files into the Mirror at the Commit they were
	// matched against: the Instance holds their objects now, so the Pull
	// marks them present without a transfer, within the Namespace's quota.
	// At another Commit, such as the tracked branch's newest, a file that
	// changed since would be fetched from the Upstream, and one removed
	// since would fail the Pull (issue #28).
	byCommit := map[string][]string{}
	var uploaded []string
	for _, f := range upload {
		o := idx.lfs[f.SHA256]
		byCommit[o.commit] = append(byCommit[o.commit], o.path)
		uploaded = append(uploaded, sentBy[f.SHA256]...)
	}
	sort.Strings(uploaded)
	res.Uploaded = slices.Compact(uploaded)
	for _, at := range idx.commits {
		sel := byCommit[at]
		if len(sel) == 0 {
			continue
		}
		job, err := c.pullIntoMirror(ctx, id, at, gen.Selection{Paths: &sel})
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

// importCandidate is a local file to match against a Mirror.
type importCandidate struct {
	localFile
	// snapshot is the cached Commit the file came from; empty for a folder.
	snapshot string
	// gitOID is the file's git blob ID, if known.
	gitOID string
}

// importCandidates lists the files of an import source: the snapshot's
// first, then the other snapshots'. A file that several snapshots hold
// unchanged, at the same path and with the same blob, is listed once.
func importCandidates(s importSource) []*importCandidate {
	var out []*importCandidate
	seen := map[[2]string]bool{}
	addAll := func(commit string, files []localFile) {
		for _, f := range files {
			k := [2]string{f.Path, f.Disk}
			if seen[k] {
				continue
			}
			seen[k] = true
			c := &importCandidate{localFile: f, snapshot: commit}
			// A Hugging Face cache names the blobs of regular files by
			// their git blob ID.
			if name := filepath.Base(f.Disk); commit != "" && f.SHA256 == "" && fullSHA.MatchString(name) {
				c.gitOID = name
			}
			out = append(out, c)
		}
	}
	addAll(s.commit, s.files)
	for _, o := range s.others {
		addAll(o.commit, o.files)
	}
	return out
}

// maxGitBlobHash is the largest file whose git blob ID the import works
// out to match it against a Mirror's regular files.
const maxGitBlobHash = 64 << 20

// hashCandidates works out the hashes needed to match files against a
// Mirror: the SHA-256 of a file the size of one of its LFS objects, and
// the git blob ID of a file the size of one of its regular files. Other
// files cannot match, so they are not read.
func (a *app) hashCandidates(ctx context.Context, id model.RepoID, cands []*importCandidate, idx *mirrorIndex) error {
	var unhashed []localFile
	var at []int
	var hashBytes int64
	for i, f := range cands {
		if f.SHA256 == "" && idx.lfsSizes[f.Size] {
			unhashed = append(unhashed, f.localFile)
			at = append(at, i)
			hashBytes += f.Size
		}
		if f.gitOID == "" && f.Size <= maxGitBlobHash && idx.gitSizes[f.Size] {
			oid, err := gitBlobIDOf(f.Disk, f.Size)
			if err != nil {
				return fmt.Errorf("%s: %w", f.Path, err)
			}
			f.gitOID = oid
		}
	}
	if len(unhashed) == 0 {
		return nil
	}
	hp := newProgress(a.stderr, "Hashing "+display(id), hashBytes, a.json)
	if err := hashFiles(ctx, unhashed, hp); err != nil {
		return err
	}
	hp.finish()
	for k, i := range at {
		cands[i].SHA256 = unhashed[k].SHA256
	}
	return nil
}

// gitBlobIDOf returns the git blob ID of a file of a known size.
func gitBlobIDOf(disk string, size int64) (string, error) {
	f, err := os.Open(disk) // #nosec G304 -- a file the user asked to import.
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha1.New() // #nosec G401 -- git object IDs are SHA-1.
	fmt.Fprintf(h, "blob %d\x00", size)
	n, err := io.Copy(h, io.LimitReader(f, size+1))
	if err != nil {
		return "", err
	}
	if n != size {
		return "", fmt.Errorf("the file changed size while it was read")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// notMatchedFile is a local file whose content is in no file of the
// Mirror that the import checked.
type notMatchedFile struct {
	Path string `json:"path"`
	// Snapshot is the cached Commit the file came from, when the Repo has
	// more than one cached snapshot.
	Snapshot string `json:"snapshot,omitempty"`
	Reason   string `json:"reason"`
}

// mirrorObject is an LFS object a Mirror references.
type mirrorObject struct {
	size    int64
	present bool
	// commit and path are where the import first saw it.
	commit, path string
}

// mirrorIndex holds the files of a Mirror at the Commits an import
// checked, by content.
type mirrorIndex struct {
	commits  []string
	lfs      map[string]*mirrorObject // by SHA-256
	git      map[string]bool          // git blob IDs of regular files
	paths    map[string]string        // path to the first Commit that has it
	lfsSizes map[int64]bool
	gitSizes map[int64]bool
}

func newMirrorIndex() *mirrorIndex {
	return &mirrorIndex{lfs: map[string]*mirrorObject{}, git: map[string]bool{}, paths: map[string]string{},
		lfsSizes: map[int64]bool{}, gitSizes: map[int64]bool{}}
}

// has reports whether the index holds a Commit.
func (x *mirrorIndex) has(commit string) bool {
	return slices.Contains(x.commits, commit)
}

// add records the files of a Commit.
func (x *mirrorIndex) add(commit string, entries []gen.TreeEntry) {
	if x.has(commit) {
		return
	}
	x.commits = append(x.commits, commit)
	for _, e := range entries {
		if _, ok := x.paths[e.Path]; !ok {
			x.paths[e.Path] = commit
		}
		if e.Lfs == nil {
			x.git[e.Oid] = true
			x.gitSizes[e.Size] = true
			continue
		}
		o := x.lfs[e.Lfs.Oid]
		if o == nil {
			o = &mirrorObject{size: e.Lfs.Size, commit: commit, path: e.Path}
			x.lfs[e.Lfs.Oid] = o
			x.lfsSizes[e.Lfs.Size] = true
		}
		o.present = o.present || e.Present
	}
}

// reason says why a file at path p matched nothing in the Mirror.
func (x *mirrorIndex) reason(p string) string {
	if c, ok := x.paths[p]; ok {
		return fmt.Sprintf("its content differs from %s at Commit %s and from every other file checked", p, shortSHA(c))
	}
	short := make([]string, len(x.commits))
	for i, c := range x.commits {
		short[i] = shortSHA(c)
	}
	if len(short) == 1 {
		return "no file in the Mirror at Commit " + short[0] + " has this content"
	}
	return fmt.Sprintf("no file in the Mirror at Commits %s or %s has this content", strings.Join(short[:len(short)-1], ", "), short[len(short)-1])
}

// pullHistory pulls a Commit's history into an existing Mirror, with no
// files, and waits for it.
func (a *app) pullHistory(ctx context.Context, c *client, id model.RepoID, commit string) error {
	job, err := c.pullIntoMirror(ctx, id, commit, gen.Selection{Paths: &[]string{}})
	if err != nil {
		return err
	}
	_, err = a.watchJob(ctx, c, job.Id, fmt.Sprintf("Fetching Commit %s of %s", shortSHA(commit), display(id)))
	return err
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
	only := newImportFilter(o.Only)
	type folder struct{ name, dir string }
	var found []folder
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
			name := p.Name() + "/" + r.Name()
			if !only.match(name, sanitiseName(p.Name())+"/"+sanitiseName(r.Name())) {
				continue
			}
			found = append(found, folder{name: name, dir: filepath.Join(root, p.Name(), r.Name())})
		}
	}
	if err := only.unmatched(root); err != nil {
		return nil, err
	}
	var c *client
	if !o.DryRun {
		if c, err = a.client(); err != nil {
			return nil, err
		}
	}
	var out []importResult
	for _, m := range found {
		pub, repo, _ := strings.Cut(m.name, "/")
		id, err := model.ParseRepoID(model.RepoTypeModel, sanitiseName(pub)+"/"+sanitiseName(repo))
		if err != nil {
			out = append(out, importResult{Repo: m.name, Source: m.dir, Error: err.Error()})
			continue
		}
		files, outside, err := walkFolder(m.dir, nil, nil)
		if err != nil {
			return nil, err
		}
		a.warnOutside(m.dir, outside)
		if len(files) == 0 {
			continue
		}
		out = append(out, a.importSnapshot(ctx, c, importSource{id: id, src: m.dir, files: files}, o))
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
	only := newImportFilter(o.Only)
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
		model, tag := parts[len(parts)-2], parts[len(parts)-1]
		ref := model // the name Ollama shows, such as llama3.2 or someone/thing
		if ns := parts[len(parts)-3]; ns != "library" && len(parts) >= 4 {
			model = ns + "-" + model
			ref = ns + "/" + ref
		}
		names := []string{ref + ":" + tag}
		if tag == "latest" {
			names = append(names, ref)
		}
		if !only.match(names...) {
			return nil
		}
		found = append(found, entry{name: model, tag: tag, path: p})
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil, notFoundf("no Ollama models at %s", root)
	}
	if err != nil {
		return nil, err
	}
	if err := only.unmatched(root); err != nil {
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
