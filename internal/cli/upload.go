package cli

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/jamesagarside/hangar/internal/api/gen"
	"github.com/jamesagarside/hangar/internal/model"
)

const (
	// planBatch is the most files one upload plan may hold (api/openapi.yaml).
	planBatch = 1000
	// partConcurrency is how many parts of one file go up at once.
	partConcurrency = 4
	// completeAttempts is how many times one upload completion is sent while
	// the Instance answers 503 with Retry-After. hangar push --help says 10.
	completeAttempts = 10
)

// waitRetry waits d or until ctx ends. Tests replace it to skip the wait.
var waitRetry = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

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
	// waiting shows a status line while the Instance checks an upload;
	// nil shows none.
	waiting func(msg string) func()
}

// upload sends every file that the Instance does not already have and
// returns the Commit operations that add them. A file that still fails
// after its retries does not stop the others (bellatrix finding 8): upload
// sends the rest, then returns the operations for the files that are on
// the Instance with an *uploadFailures naming the ones that are not, so
// running the command again sends only those. A cancelled context stops
// at once.
func (u *uploader) upload(ctx context.Context, files []localFile) ([]gen.CommitOperation, error) {
	ops := make([]gen.CommitOperation, 0, len(files))
	failed := &uploadFailures{}
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
					if ctx.Err() != nil {
						return nil, fmt.Errorf("%s: %w", f.Path, err)
					}
					failed.files = append(failed.files, fileFailure{path: f.Path, err: err})
					continue
				}
				ops = append(ops, gen.CommitOperation{Op: gen.AddLfs, Path: f.Path, Sha256: ptr(f.SHA256), Size: ptr(f.Size)})
			default:
				return nil, fmt.Errorf("%s: unknown upload action %q", f.Path, item.Action)
			}
		}
	}
	if len(failed.files) > 0 {
		return ops, failed
	}
	return ops, nil
}

// uploadFailures lists the files that failed to upload after every retry,
// while the other files of the same push or import went up.
type uploadFailures struct {
	files []fileFailure
}

type fileFailure struct {
	path string
	err  error
}

func (e *uploadFailures) Error() string {
	if len(e.files) == 1 {
		return fmt.Sprintf("%s: %v", e.files[0].path, e.files[0].err)
	}
	msgs := make([]string, len(e.files))
	for i, f := range e.files {
		msgs[i] = fmt.Sprintf("%s: %v", f.path, f.err)
	}
	return fmt.Sprintf("%d files failed to upload: %s", len(e.files), strings.Join(msgs, "; "))
}

// Unwrap returns each file's error, so errors.As finds an API error and
// its exit code.
func (e *uploadFailures) Unwrap() []error {
	out := make([]error, len(e.files))
	for i, f := range e.files {
		out[i] = f.err
	}
	return out
}

// paths returns the paths of the files that failed.
func (e *uploadFailures) paths() []string {
	out := make([]string, len(e.files))
	for i, f := range e.files {
		out[i] = f.path
	}
	return out
}

// uploadFile sends the parts of one file that are not stored yet, then
// completes the upload. A part that fails for a reason that may pass is
// sent again (sendPart); presigned part URLs that expired are replaced by
// planning the file again, which resumes the same upload.
func (u *uploader) uploadFile(ctx context.Context, f localFile, item gen.UploadPlanItem) error {
	if item.UploadId == nil || item.PartUrls == nil || item.PartSize == nil || *item.PartSize <= 0 {
		return fmt.Errorf("the upload plan has no upload ID, part size or part URLs")
	}
	partSize := *item.PartSize
	want := int((f.Size + partSize - 1) / partSize)
	if f.Size == 0 {
		want = 1
	}
	if len(*item.PartUrls) != want {
		return fmt.Errorf("the upload plan has %d part URLs for %d parts", len(*item.PartUrls), want)
	}
	urls := &partURLs{list: *item.PartUrls, upload: *item.UploadId}
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
			etag, err := u.sendPart(gctx, f, n, off, size, urls, presigned)
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
	stop := u.status(fmt.Sprintf("Checking %s (%s) on the Instance", f.Path, formatBytes(f.Size)))
	fin, err := u.complete(ctx, f, *item.UploadId, results)
	stop()
	if err != nil {
		return err
	}
	if fin.State != gen.Verified {
		return fmt.Errorf("the Instance did not verify the upload")
	}
	return nil
}

// status shows a line while something slow happens, if the uploader has a
// way to show one, and returns a function that removes it.
func (u *uploader) status(msg string) func() {
	if u.waiting == nil {
		return func() {}
	}
	return u.waiting(msg)
}

// partURLs holds an upload's part URLs, which a refresh replaces when
// presigned URLs expire during a long upload.
type partURLs struct {
	mu     sync.Mutex
	list   []string
	upload gen.ID
	// gen counts refreshes, so parts that fail together refresh once.
	gen int
}

func (p *partURLs) get(n int) (string, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.list[n-1], p.gen
}

// refresh plans the file again, which resumes the same upload with new
// part URLs, unless another part has refreshed them since seen.
func (u *uploader) refresh(ctx context.Context, f localFile, urls *partURLs, seen int) error {
	urls.mu.Lock()
	defer urls.mu.Unlock()
	if urls.gen != seen {
		return nil
	}
	req := gen.UploadPlanRequest{Files: []gen.UploadFile{{Path: f.Path, Size: f.Size, Sha256: f.SHA256}}}
	if u.revision != "" {
		req.Revision = &u.revision
	}
	plan, err := u.c.planUploads(ctx, u.id, req)
	if err != nil {
		return err
	}
	if len(plan.Files) != 1 || plan.Files[0].Action != gen.Upload || plan.Files[0].UploadId == nil || *plan.Files[0].UploadId != urls.upload ||
		plan.Files[0].PartUrls == nil || len(*plan.Files[0].PartUrls) != len(urls.list) {
		return fmt.Errorf("the Instance no longer has this upload")
	}
	urls.list = *plan.Files[0].PartUrls
	urls.gen++
	return nil
}

// sendPart uploads one part, sending it again after a failure that may
// pass (transient) with a growing wait, up to transferAttempts times. A
// presigned URL that answers 403 has usually expired, so the URLs are
// refreshed once and the part is sent again. Progress counts the part's
// bytes once, however many times it is sent.
func (u *uploader) sendPart(ctx context.Context, f localFile, n int, off, size int64, urls *partURLs, presigned bool) (string, error) {
	refreshed := false
	for attempt := 1; ; attempt++ {
		raw, seen := urls.get(n)
		etag, err := u.putPart(ctx, f.Disk, off, size, raw, presigned)
		if err == nil {
			return etag, nil
		}
		var ae *APIError
		if presigned && !refreshed && errors.As(err, &ae) && ae.Status == http.StatusForbidden && ctx.Err() == nil {
			refreshed = true
			if rerr := u.refresh(ctx, f, urls, seen); rerr != nil {
				return "", fmt.Errorf("%w; renew the part URLs: %w", err, rerr)
			}
			continue
		}
		if !transient(ctx, err) || attempt >= transferAttempts {
			return "", err
		}
		if werr := waitRetry(ctx, retryWait(err, attempt)); werr != nil {
			return "", werr
		}
	}
}

// complete finishes an upload. A 503 with Retry-After means the Instance
// kept the upload and its parts (api/openapi.yaml), so the same request is
// sent again after the wait it asks for. A connection that fails or times
// out, or a proxy that answers 502, 504, 408 or 429, may cut off a
// completion the Instance carries on with, so it is sent again too, after
// a growing wait. If a completion sent again finds the upload gone, an
// earlier one finished it: planning the file again then says the object
// is stored. It sends the completion at most completeAttempts times.
func (u *uploader) complete(ctx context.Context, f localFile, upload gen.ID, parts []gen.UploadedPart) (*gen.UploadStatus, error) {
	for attempt := 1; ; attempt++ {
		fin, err := u.c.completeUpload(ctx, u.id, upload, parts)
		if err == nil {
			return fin, nil
		}
		var ae *APIError
		var wait time.Duration
		switch {
		case errors.As(err, &ae) && ae.Retryable:
			wait = ae.RetryAfter
		case attempt > 1 && IsNotFound(err):
			if ok, perr := u.stored(ctx, f); perr == nil && ok {
				return &gen.UploadStatus{Id: upload, Sha256: f.SHA256, Size: f.Size, Parts: parts, State: gen.Verified}, nil
			}
			return nil, err
		case completionCutOff(ctx, err):
			wait = retryWait(err, attempt)
		default:
			return nil, err
		}
		if attempt == completeAttempts {
			return nil, fmt.Errorf("%w; the Instance kept the upload, so run the command again to finish it", err)
		}
		if err := waitRetry(ctx, wait); err != nil {
			return nil, err
		}
	}
}

// completionCutOff reports whether a completion may have been cut off on
// the way rather than refused: the connection failed or timed out, or a
// proxy gave up waiting (502, 504, 408) or throttled it (429). A 500 or a
// 503 without Retry-After is the Instance's own answer, so it is not.
func completionCutOff(ctx context.Context, err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		switch ae.Status {
		case http.StatusBadGateway, http.StatusGatewayTimeout, http.StatusRequestTimeout, http.StatusTooManyRequests:
			return ctx.Err() == nil
		}
		return false
	}
	return transient(ctx, err)
}

// stored reports whether the Instance holds f's object, by planning it.
func (u *uploader) stored(ctx context.Context, f localFile) (bool, error) {
	req := gen.UploadPlanRequest{Files: []gen.UploadFile{{Path: f.Path, Size: f.Size, Sha256: f.SHA256}}}
	if u.revision != "" {
		req.Revision = &u.revision
	}
	plan, err := u.c.planUploads(ctx, u.id, req)
	if err != nil {
		return false, err
	}
	return len(plan.Files) == 1 && plan.Files[0].Action == gen.Exists, nil
}

// putPart uploads one part and returns its ETag. The part's bytes count
// towards progress as they are sent; if the request fails, they are taken
// off again, so a part sent twice counts once.
func (u *uploader) putPart(ctx context.Context, disk string, off, size int64, rawURL string, presigned bool) (etag string, err error) {
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
	body := &countingProgress{r: io.NewSectionReader(f, off, size), p: u.prog}
	defer func() {
		if err != nil {
			body.undo()
		}
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target.String(), body)
	if err != nil {
		return "", redactURLError(err)
	}
	req.ContentLength = size
	req.Header.Set("User-Agent", u.c.ua)
	if !presigned && u.c.token != "" && u.c.sameOrigin(target) {
		req.Header.Set("Authorization", "Bearer "+u.c.token)
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	resp, err := u.c.send(req)
	if err != nil {
		return "", fmt.Errorf("send the part: %w", err)
	}
	defer resp.Body.Close()
	b, rerr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		return "", apiError(resp.StatusCode, resp.Header, b)
	}
	if rerr != nil {
		return "", fmt.Errorf("read the part upload's answer: %w", &transportError{err: rerr})
	}
	etag = resp.Header.Get("ETag")
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
