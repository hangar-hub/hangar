package cli

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/jamesagarside/hangar/internal/api/gen"
	"github.com/jamesagarside/hangar/internal/model"
)

const (
	// planBatch is the most files one upload plan may hold (api/openapi.yaml).
	planBatch = 1000
	// partConcurrency is how many parts of one file go up at once.
	partConcurrency = 4
)

// localFile is a file on disk to upload as a path in a Repo.
type localFile struct {
	Path   string // in the Repo, with /
	Disk   string // on disk
	Size   int64
	SHA256 string
}

// walkFolder lists the files under dir, skipping .git and huggingface_hub's
// .cache/huggingface metadata, and applies include and exclude patterns.
func walkFolder(dir string, include, exclude []string) ([]localFile, error) {
	inc, err := newGlobMatcher(include)
	if err != nil {
		return nil, err
	}
	exc, err := newGlobMatcher(exclude)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, usagef("%s is not a folder", dir)
	}
	var out []localFile
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == ".git" || rel == ".cache/huggingface" || rel == ".cache" && dirOnlyHF(p) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(rel, ".incomplete") {
			return nil
		}
		if !inc.empty() && !inc.match(rel) || exc.match(rel) {
			return nil
		}
		info, err := os.Stat(p) // follows symlinks, as in an HF cache snapshot.
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		out = append(out, localFile{Path: rel, Disk: p, Size: info.Size()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// dirOnlyHF reports whether a .cache folder holds only huggingface_hub's metadata.
func dirOnlyHF(p string) bool {
	entries, err := os.ReadDir(p)
	return err == nil && len(entries) == 1 && entries[0].Name() == "huggingface"
}

// hashFiles fills in SHA256 for every file.
func hashFiles(ctx context.Context, files []localFile, prog *progress) error {
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(4)
	for i := range files {
		g.Go(func() error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			f, err := os.Open(files[i].Disk)
			if err != nil {
				return err
			}
			defer f.Close()
			h := sha256.New()
			if _, err := io.Copy(h, progressReader{r: f, p: prog}); err != nil {
				return fmt.Errorf("%s: %w", files[i].Path, err)
			}
			files[i].SHA256 = hex.EncodeToString(h.Sum(nil))
			return nil
		})
	}
	return g.Wait()
}

// uploader sends files to a Repo through the native upload API: a plan per
// batch, multipart uploads that resume from the parts already stored, and
// completion, which verifies the SHA-256 and size.
type uploader struct {
	c        *client
	id       model.RepoID
	revision string
	prog     *progress
}

// upload sends every file that the Instance does not already have and
// returns the Commit operations that add them.
func (u *uploader) upload(ctx context.Context, files []localFile) ([]gen.CommitOperation, error) {
	ops := make([]gen.CommitOperation, 0, len(files))
	for start := 0; start < len(files); start += planBatch {
		batch := files[start:min(start+planBatch, len(files))]
		req := gen.UploadPlanRequest{Files: make([]gen.UploadFile, len(batch))}
		if u.revision != "" {
			req.Revision = &u.revision
		}
		byPath := map[string]localFile{}
		for i, f := range batch {
			req.Files[i] = gen.UploadFile{Path: f.Path, Size: f.Size, Sha256: f.SHA256}
			byPath[f.Path] = f
		}
		plan, err := u.c.planUploads(ctx, u.id, req)
		if err != nil {
			return nil, err
		}
		for _, item := range plan.Files {
			f, ok := byPath[item.Path]
			if !ok {
				return nil, fmt.Errorf("the upload plan names %q, which was not asked for", item.Path)
			}
			switch item.Action {
			case gen.Inline:
				b, err := os.ReadFile(f.Disk)
				if err != nil {
					return nil, err
				}
				u.prog.add(f.Size)
				ops = append(ops, gen.CommitOperation{Op: gen.Add, Path: f.Path, ContentBase64: ptr(base64.StdEncoding.EncodeToString(b))})
			case gen.Exists:
				u.prog.add(f.Size)
				ops = append(ops, gen.CommitOperation{Op: gen.AddLfs, Path: f.Path, Sha256: ptr(f.SHA256), Size: ptr(f.Size)})
			case gen.Upload:
				if err := u.uploadFile(ctx, f, item); err != nil {
					return nil, fmt.Errorf("%s: %w", f.Path, err)
				}
				ops = append(ops, gen.CommitOperation{Op: gen.AddLfs, Path: f.Path, Sha256: ptr(f.SHA256), Size: ptr(f.Size)})
			default:
				return nil, fmt.Errorf("%s: unknown upload action %q", f.Path, item.Action)
			}
		}
	}
	return ops, nil
}

// uploadFile sends the parts of one file that are not stored yet, then
// completes the upload.
func (u *uploader) uploadFile(ctx context.Context, f localFile, item gen.UploadPlanItem) error {
	if item.UploadId == nil || item.PartUrls == nil || item.PartSize == nil || *item.PartSize <= 0 {
		return fmt.Errorf("the upload plan has no upload ID, part size or part URLs")
	}
	urls := *item.PartUrls
	partSize := *item.PartSize
	want := int((f.Size + partSize - 1) / partSize)
	if f.Size == 0 {
		want = 1
	}
	if len(urls) != want {
		return fmt.Errorf("the upload plan has %d part URLs for %d parts", len(urls), want)
	}
	done := map[int]gen.UploadedPart{}
	st, err := u.c.uploadStatus(ctx, u.id, *item.UploadId)
	if err != nil {
		return err
	}
	if st.State == gen.Verified {
		u.prog.add(f.Size)
		return nil
	}
	for _, p := range st.Parts {
		// Keep a stored part only if it has the size this part must have,
		// so a part cut short by an interruption is sent again.
		off := int64(p.Number-1) * partSize
		if p.Number < 1 || p.Number > want || p.Size == nil || *p.Size != min(partSize, f.Size-off) {
			continue
		}
		done[p.Number] = p
		u.prog.add(*p.Size)
	}
	presigned := deref(item.Presigned)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(partConcurrency)
	results := make([]gen.UploadedPart, want)
	for n := 1; n <= want; n++ {
		if p, ok := done[n]; ok {
			results[n-1] = p
			continue
		}
		g.Go(func() error {
			off := int64(n-1) * partSize
			size := min(partSize, f.Size-off)
			etag, err := u.putPart(gctx, f.Disk, off, size, urls[n-1], presigned)
			if err != nil {
				return fmt.Errorf("part %d: %w", n, err)
			}
			results[n-1] = gen.UploadedPart{Number: n, Etag: etag, Size: ptr(size)}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return fmt.Errorf("%w; run the command again to resume the upload", err)
	}
	fin, err := u.c.completeUpload(ctx, u.id, *item.UploadId, results)
	if err != nil {
		return err
	}
	if fin.State != gen.Verified {
		return fmt.Errorf("the Instance did not verify the upload")
	}
	return nil
}

// putPart uploads one part and returns its ETag.
func (u *uploader) putPart(ctx context.Context, disk string, off, size int64, rawURL string, presigned bool) (string, error) {
	f, err := os.Open(disk) // #nosec G304 -- a file the user asked to upload.
	if err != nil {
		return "", err
	}
	defer f.Close()
	target, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("bad part URL")
	}
	if !target.IsAbs() {
		target = u.c.base.ResolveReference(target)
	}
	body := progressReader{r: io.NewSectionReader(f, off, size), p: u.prog}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target.String(), body)
	if err != nil {
		return "", err
	}
	req.ContentLength = size
	req.Header.Set("User-Agent", u.c.ua)
	if !presigned && u.c.token != "" && u.c.sameOrigin(target) {
		req.Header.Set("Authorization", "Bearer "+u.c.token)
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	resp, err := u.c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("send the part: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		return "", apiError(resp.StatusCode, resp.Header, b)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		var p gen.UploadedPart
		if json.Unmarshal(b, &p) == nil {
			etag = p.Etag
		}
	}
	if etag == "" {
		return "", fmt.Errorf("the part upload answered without an ETag")
	}
	return etag, nil
}
