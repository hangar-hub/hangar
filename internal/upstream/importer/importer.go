// Package importer adds models and datasets that are already on disk to a
// Hangar Instance without downloading them again (PULL-12,
// docs/spec/06-upstreams-and-pulls.md "Bulk import"): a Hugging Face cache,
// an LM Studio models folder, any directory, and Ollama's models. It talks
// to the Instance through its native API (Target), so `hangar import`
// works against any Instance the user can write to.
package importer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/jamesagarside/hangar/internal/model"
)

// File is a local file to upload.
type File struct {
	// Path is the path in the Repo.
	Path string
	// Local is the file on disk.
	Local  string
	Size   int64
	SHA256 string
}

// Target is the Instance an import writes to.
type Target interface {
	// PullHistory Pulls the history of an upstream Repo at a Revision
	// with no files, creating the Mirror, and waits for it.
	PullHistory(ctx context.Context, upstream string, id model.RepoID, revision string) error
	// PullFiles Pulls files into a Mirror and waits. Files whose objects
	// the Instance already holds need no transfer.
	PullFiles(ctx context.Context, id model.RepoID, paths []string) error
	// Tree lists every file of a Repo at a Revision, with LFS details.
	Tree(ctx context.Context, id model.RepoID, revision string) ([]model.Entry, error)
	// Store uploads an LFS object for a Repo unless the Instance holds it.
	Store(ctx context.Context, id model.RepoID, f File) error
	// CreateRepo creates a Repo; an existing one is not an error.
	CreateRepo(ctx context.Context, id model.RepoID) error
	// Commit uploads files as needed and commits them to the default branch.
	Commit(ctx context.Context, id model.RepoID, summary string, files []File) error
}

// ErrUpstreamMissing means the Upstream does not have the Repo; Targets
// wrap it so imports can fall back to a local Repo.
var ErrUpstreamMissing = errors.New("the upstream does not have this repo")

// Options configure an Importer.
type Options struct {
	Target Target
	// Upstream names the Upstream to plan Mirrors against; "" means the
	// Instance's default.
	Upstream string
	// Offline imports Hugging Face cache Repos as local Repos with one
	// Commit, without asking the Upstream.
	Offline bool
	// Namespace receives local Repos from LM Studio and Ollama; "" uses
	// the publisher or "ollama".
	Namespace string
	// Progress, when set, receives one line per step.
	Progress func(msg string)
}

// Importer runs imports.
type Importer struct{ opts Options }

// New returns an Importer.
func New(opts Options) *Importer { return &Importer{opts: opts} }

func (im *Importer) say(format string, a ...any) {
	if im.opts.Progress != nil {
		im.opts.Progress(fmt.Sprintf(format, a...))
	}
}

// Result says what happened to one Repo.
type Result struct {
	Repo model.RepoID
	// Kind is "mirror" or "local".
	Kind string
	// Files and Bytes count what was uploaded or found already stored.
	Files int
	Bytes int64
	// Unmatched lists local files that matched nothing upstream.
	Unmatched []string
	Err       error
}

// Report is the outcome of an import.
type Report struct{ Results []Result }

// Failed counts Repos that failed.
func (r *Report) Failed() int {
	n := 0
	for _, x := range r.Results {
		if x.Err != nil {
			n++
		}
	}
	return n
}

// hashFile returns a file's SHA-256 and size.
func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path) //nolint:gosec // the user names the folders to import
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// walkFiles lists regular files under root, with paths relative to root
// using `/`. Hidden files and folders (starting with `.`) are skipped,
// except .gitattributes. Symbolic links are followed only to files inside
// root or, for a Hugging Face cache snapshot, inside the Repo's cache
// folder, so an import never uploads a file from elsewhere on the machine
// (06-upstreams-and-pulls.md "Bulk import").
func walkFiles(root string) ([]File, error) {
	scope, err := linkScope(root)
	if err != nil {
		return nil, err
	}
	var out []File
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if p != root && strings.HasPrefix(name, ".") && name != ".gitattributes" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 && !linkInScope(p, scope) {
			return nil
		}
		info, err := os.Stat(p) // follows symlinks, as the HF cache uses them
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		// Names a Repo cannot hold (such as .git folders) are skipped.
		if clean, cerr := model.CleanRepoPath(filepath.ToSlash(rel)); cerr == nil && clean != "" {
			out = append(out, File{Path: clean, Local: p, Size: info.Size()})
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, err
}

// linkScope returns the folder symbolic links under root may resolve into:
// root, or the Repo folder of a Hugging Face cache snapshot
// (<type>s--<ns>--<name>/snapshots/<commit>), whose files link into its
// blobs folder.
func linkScope(root string) (string, error) {
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	if real, err = filepath.Abs(real); err != nil {
		return "", err
	}
	snapshots := filepath.Dir(real)
	if filepath.Base(snapshots) == "snapshots" && cacheDirRe.MatchString(filepath.Base(filepath.Dir(snapshots))) {
		return filepath.Dir(snapshots), nil
	}
	return real, nil
}

// linkInScope reports whether the symbolic link p resolves inside scope,
// or to a blob of a Hugging Face cache, as files hangar lmstudio links
// into LM Studio's folder do.
func linkInScope(p, scope string) bool {
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return false
	}
	if real, err = filepath.Abs(real); err != nil {
		return false
	}
	if blobs := filepath.Dir(real); filepath.Base(blobs) == "blobs" && cacheDirRe.MatchString(filepath.Base(filepath.Dir(blobs))) {
		return true
	}
	rel, err := filepath.Rel(scope, real)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func hashAll(files []File) error {
	for i := range files {
		sum, n, err := hashFile(files[i].Local)
		if err != nil {
			return err
		}
		files[i].SHA256, files[i].Size = sum, n
	}
	return nil
}

// FromDir uploads a directory as a new Commit to id, creating the Repo
// if needed, as `hangar push` does.
func (im *Importer) FromDir(ctx context.Context, dir string, id model.RepoID, summary string) Result {
	res := Result{Repo: id, Kind: "local"}
	files, err := walkFiles(dir)
	if err == nil {
		err = hashAll(files)
	}
	if err == nil && len(files) == 0 {
		err = fmt.Errorf("%s has no files to import", dir)
	}
	if err != nil {
		res.Err = err
		return res
	}
	if summary == "" {
		summary = "Import from " + filepath.Base(dir)
	}
	im.say("Uploading %d files to %s", len(files), id.Path())
	if err := im.opts.Target.CreateRepo(ctx, id); err != nil {
		res.Err = err
		return res
	}
	if err := im.opts.Target.Commit(ctx, id, summary, files); err != nil {
		res.Err = err
		return res
	}
	for _, f := range files {
		res.Files++
		res.Bytes += f.Size
	}
	return res
}

// cacheDirRe matches Hugging Face cache Repo folders.
var cacheDirRe = regexp.MustCompile(`^(models|datasets)--(.+)$`)

// DefaultHFCache returns the Hugging Face cache folder huggingface_hub
// uses: HF_HUB_CACHE, else HF_HOME/hub, else ~/.cache/huggingface/hub.
func DefaultHFCache() string {
	if v := os.Getenv("HF_HUB_CACHE"); v != "" {
		return v
	}
	if v := os.Getenv("HF_HOME"); v != "" {
		return filepath.Join(v, "hub")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "huggingface", "hub")
}

// cacheRepo is one Repo in a Hugging Face cache.
type cacheRepo struct {
	id       model.RepoID
	revision string
	snapshot string
}

func readCache(root string) ([]cacheRepo, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []cacheRepo
	for _, e := range entries {
		m := cacheDirRe.FindStringSubmatch(e.Name())
		if !e.IsDir() || m == nil {
			continue
		}
		parts := strings.SplitN(m[2], "--", 2)
		if len(parts) != 2 {
			continue
		}
		t := model.RepoTypeModel
		if m[1] == "datasets" {
			t = model.RepoTypeDataset
		}
		id, err := model.ParseRepoID(t, parts[0]+"/"+parts[1])
		if err != nil {
			continue
		}
		dir := filepath.Join(root, e.Name())
		rev := ""
		if b, err := os.ReadFile(filepath.Join(dir, "refs", "main")); err == nil { //nolint:gosec // inside the cache folder
			rev = strings.TrimSpace(string(b))
		}
		snaps, _ := os.ReadDir(filepath.Join(dir, "snapshots"))
		if !model.IsFullSHA(rev) {
			rev = ""
			var newest int64
			for _, s := range snaps {
				if info, err := s.Info(); err == nil && model.IsFullSHA(s.Name()) && info.ModTime().UnixNano() > newest {
					rev, newest = s.Name(), info.ModTime().UnixNano()
				}
			}
		}
		if rev == "" {
			continue
		}
		snap := filepath.Join(dir, "snapshots", rev)
		if _, err := os.Stat(snap); err != nil { //nolint:gosec // inside the cache folder the user named
			continue
		}
		out = append(out, cacheRepo{id: id, revision: rev, snapshot: snap})
	}
	return out, nil
}

// FromHFCache imports every Repo in a Hugging Face cache. Online, each
// becomes a Mirror planned against the Upstream at the cached Commit, and
// only the cached files whose SHA-256 matches the planned LFS objects are
// uploaded. With Offline, each becomes a local Repo with one Commit.
func (im *Importer) FromHFCache(ctx context.Context, root string) (*Report, error) {
	repos, err := readCache(root)
	if err != nil {
		return nil, fmt.Errorf("read the Hugging Face cache at %s: %w", root, err)
	}
	rep := &Report{}
	for _, cr := range repos {
		if ctx.Err() != nil {
			return rep, ctx.Err()
		}
		if im.opts.Offline {
			im.say("Importing %s from the cache as a local Repo", cr.id.Path())
			rep.Results = append(rep.Results, im.FromDir(ctx, cr.snapshot, cr.id, "Import from the Hugging Face cache at "+cr.revision[:12]))
			continue
		}
		rep.Results = append(rep.Results, im.mirror(ctx, cr.id, cr.revision, cr.snapshot, false))
	}
	return rep, nil
}

// mirror plans a Mirror of id at revision and uploads the local files that
// match its LFS objects. byHash matches files by content wherever they are
// (LM Studio renames nothing, but keeps no folder structure); otherwise
// files match by path and content.
func (im *Importer) mirror(ctx context.Context, id model.RepoID, revision, dir string, byHash bool) Result {
	res := Result{Repo: id, Kind: "mirror"}
	im.say("Planning a Mirror of %s", id.Path())
	if err := im.opts.Target.PullHistory(ctx, im.opts.Upstream, id, revision); err != nil {
		res.Err = err
		return res
	}
	rev := revision
	if rev == "" {
		rev = model.DefaultBranch
	}
	entries, err := im.opts.Target.Tree(ctx, id, rev)
	if err != nil {
		res.Err = err
		return res
	}
	byPath := map[string]model.Entry{}
	byOID := map[string][]string{}
	for _, e := range entries {
		if e.Type == model.EntryFile && e.LFS != nil {
			byPath[e.Path] = e
			byOID[e.LFS.OID] = append(byOID[e.LFS.OID], e.Path)
		}
	}
	files, err := walkFiles(dir)
	if err != nil {
		res.Err = err
		return res
	}
	var paths []string
	for _, f := range files {
		var targets []string
		if byHash {
			sum, n, err := hashFile(f.Local)
			if err != nil {
				res.Err = err
				return res
			}
			f.SHA256, f.Size = sum, n
			targets = byOID[sum]
		} else if e, ok := byPath[f.Path]; ok && e.LFS.Size == f.Size {
			sum, n, err := hashFile(f.Local)
			if err != nil {
				res.Err = err
				return res
			}
			f.SHA256, f.Size = sum, n
			if sum == e.LFS.OID {
				targets = []string{f.Path}
			}
		} else if !ok {
			// Regular files come with the history.
			continue
		}
		if len(targets) == 0 {
			res.Unmatched = append(res.Unmatched, f.Path)
			continue
		}
		im.say("Uploading %s (%d bytes)", f.Path, f.Size)
		if err := im.opts.Target.Store(ctx, id, File{Path: targets[0], Local: f.Local, Size: f.Size, SHA256: f.SHA256}); err != nil {
			res.Err = err
			return res
		}
		res.Files++
		res.Bytes += f.Size
		paths = append(paths, targets...)
	}
	if len(paths) == 0 {
		if byHash {
			res.Err = fmt.Errorf("no file in %s matches %s on the Upstream", dir, id.Path())
		}
		return res
	}
	sort.Strings(paths)
	if err := im.opts.Target.PullFiles(ctx, id, paths); err != nil {
		res.Err = err
	}
	return res
}

// DefaultLMStudio returns LM Studio's models folder: ~/.lmstudio/models,
// or the older ~/.cache/lm-studio/models when only that exists.
func DefaultLMStudio() string {
	home, _ := os.UserHomeDir()
	p := filepath.Join(home, ".lmstudio", "models")
	if _, err := os.Stat(p); err != nil {
		if old := filepath.Join(home, ".cache", "lm-studio", "models"); dirExists(old) {
			return old
		}
	}
	return p
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// FromLMStudio imports LM Studio's models folder (`<publisher>/<repo>/<file>`).
// A folder whose files match the Upstream Repo of the same name (by
// SHA-256) becomes a Partial mirror holding those files; a folder the
// Upstream does not have becomes a local Repo in Options.Namespace.
func (im *Importer) FromLMStudio(ctx context.Context, root string) (*Report, error) {
	pubs, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read the LM Studio models folder at %s: %w", root, err)
	}
	rep := &Report{}
	for _, pub := range pubs {
		if !pub.IsDir() || strings.HasPrefix(pub.Name(), ".") {
			continue
		}
		repos, err := os.ReadDir(filepath.Join(root, pub.Name()))
		if err != nil {
			return rep, err
		}
		for _, rd := range repos {
			if !rd.IsDir() || strings.HasPrefix(rd.Name(), ".") {
				continue
			}
			if ctx.Err() != nil {
				return rep, ctx.Err()
			}
			dir := filepath.Join(root, pub.Name(), rd.Name())
			id, err := model.ParseRepoID(model.RepoTypeModel, pub.Name()+"/"+rd.Name())
			if err != nil {
				rep.Results = append(rep.Results, Result{Repo: model.RepoID{Type: model.RepoTypeModel, Namespace: pub.Name(), Name: rd.Name()}, Err: err})
				continue
			}
			res := im.mirror(ctx, id, "", dir, true)
			if errors.Is(res.Err, ErrUpstreamMissing) {
				local := id
				if im.opts.Namespace != "" {
					local.Namespace = im.opts.Namespace
				}
				im.say("%s is not on the Upstream; importing it as the local Repo %s", id.Path(), local.Path())
				res = im.FromDir(ctx, dir, local, "Import from LM Studio")
			}
			rep.Results = append(rep.Results, res)
		}
	}
	return rep, nil
}

// DefaultOllama returns Ollama's models folder: OLLAMA_MODELS, else
// ~/.ollama/models.
func DefaultOllama() string {
	if v := os.Getenv("OLLAMA_MODELS"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ollama", "models")
}

type ollamaManifest struct {
	Layers []struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Size      int64  `json:"size"`
	} `json:"layers"`
}

// ollamaFiles maps Ollama layer media types to Repo file names.
var ollamaFiles = map[string]string{
	"application/vnd.ollama.image.template":  "template",
	"application/vnd.ollama.image.system":    "system",
	"application/vnd.ollama.image.params":    "params",
	"application/vnd.ollama.image.license":   "LICENSE",
	"application/vnd.ollama.image.projector": "mmproj.gguf",
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

func cleanName(s string) string {
	s = strings.Trim(unsafeName.ReplaceAllString(s, "-"), "-.")
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	return s
}

// FromOllama imports Ollama's models as local Repos: one Repo per model,
// a GGUF file per tag (`<model>-<tag>.gguf`), and the template, system
// prompt, parameters and licence of the first tag.
func (im *Importer) FromOllama(ctx context.Context, root string) (*Report, error) {
	manifests := filepath.Join(root, "manifests")
	type tagged struct {
		tag string
		m   ollamaManifest
	}
	models := map[string][]tagged{}
	err := filepath.WalkDir(manifests, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(manifests, p)
		if err != nil {
			return err
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) < 3 {
			return nil
		}
		b, err := os.ReadFile(p) //nolint:gosec // inside Ollama's models folder
		if err != nil {
			return err
		}
		// Files that are not manifests are skipped.
		var m ollamaManifest
		if json.Unmarshal(b, &m) == nil {
			name := parts[len(parts)-2]
			models[name] = append(models[name], tagged{tag: parts[len(parts)-1], m: m})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read Ollama's manifests at %s: %w", manifests, err)
	}
	names := make([]string, 0, len(models))
	for n := range models {
		names = append(names, n)
	}
	sort.Strings(names)
	rep := &Report{}
	ns := im.opts.Namespace
	if ns == "" {
		ns = "ollama"
	}
	for _, name := range names {
		tags := models[name]
		sort.Slice(tags, func(i, j int) bool { return tags[i].tag < tags[j].tag })
		id := model.RepoID{Type: model.RepoTypeModel, Namespace: ns, Name: cleanName(name)}
		res := Result{Repo: id, Kind: "local"}
		if err := id.Validate(); err != nil {
			res.Err = err
			rep.Results = append(rep.Results, res)
			continue
		}
		var files []File
		seen := map[string]bool{}
		for i, t := range tags {
			for _, l := range t.m.Layers {
				hexd, ok := strings.CutPrefix(l.Digest, "sha256:")
				if !ok || !model.IsSHA256(hexd) {
					continue
				}
				var path string
				switch {
				case l.MediaType == "application/vnd.ollama.image.model":
					path = cleanName(name) + "-" + cleanName(t.tag) + ".gguf"
				case i == 0 && ollamaFiles[l.MediaType] != "":
					path = ollamaFiles[l.MediaType]
				default:
					continue
				}
				if seen[path] {
					continue
				}
				seen[path] = true
				files = append(files, File{Path: path, Local: filepath.Join(root, "blobs", "sha256-"+hexd), Size: l.Size, SHA256: hexd})
			}
		}
		if len(files) == 0 {
			continue
		}
		im.say("Importing the Ollama model %s as %s", name, id.Path())
		if err := im.opts.Target.CreateRepo(ctx, id); err != nil {
			res.Err = err
		} else if err := im.opts.Target.Commit(ctx, id, "Import from Ollama", files); err != nil {
			res.Err = err
		}
		if res.Err == nil {
			for _, f := range files {
				res.Files++
				res.Bytes += f.Size
			}
		}
		rep.Results = append(rep.Results, res)
	}
	return rep, nil
}
