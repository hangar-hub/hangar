// Package upstreamtest provides a fake upstream.Client. Upstream Repos
// live in a repotest.Fake, so the fake serves real git packs with the
// same Commit SHAs that a Mirror must keep.
package upstreamtest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hangar-hub/hangar/internal/blob"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/repo/repotest"
	"github.com/hangar-hub/hangar/internal/upstream"
)

// Fake is an upstream.Client over Repos in Repos. RequireToken makes
// private Repos (visibility other than public) need that token. Viewer
// answers come from ViewerResponses, keyed by endpoint and encoded query.
type Fake struct {
	Repos *repotest.Fake
	// RequireToken, when set, is the token private Repos need.
	RequireToken string
	// Gated lists Repo IDs (`ns/name`) that refuse every caller as gated.
	Gated []string

	mu       sync.Mutex
	viewer   map[string]json.RawMessage
	failures map[string]error
	calls    map[string]int
	// holdFor makes each download wait until that many are in flight at
	// once, or holdTimeout passes; released closes when they are.
	// inFlight and maxInFlight count downloads running at once.
	holdFor               int
	holdTimeout           time.Duration
	released              chan struct{}
	inFlight, maxInFlight int
}

// HoldDownloads makes every file download, through Download or the fake
// Hub's resolve route, wait until n downloads are in flight at once, then
// lets them all through. A test uses it to show that requests run
// concurrently without depending on how fast the machine is: code that
// downloads one file after another never gets n in flight, so each
// download waits the full timeout and MaxConcurrentDownloads stays below
// n. Pick a timeout well beyond any scheduling delay, such as 15 seconds.
func (f *Fake) HoldDownloads(n int, timeout time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.holdFor, f.holdTimeout, f.released = n, timeout, make(chan struct{})
}

// slowFile holds a file as HoldDownloads asks before it is served,
// through Download or the fake Hub's resolve route, and counts it as in
// flight.
func (f *Fake) slowFile(ctx context.Context) error {
	f.mu.Lock()
	f.inFlight++
	f.maxInFlight = max(f.maxInFlight, f.inFlight)
	released, timeout := f.released, f.holdTimeout
	if released != nil && f.inFlight >= f.holdFor {
		select {
		case <-released:
		default:
			close(released)
		}
	}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inFlight--
		f.mu.Unlock()
	}()
	if released == nil {
		return nil
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-released:
		return nil
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// MaxConcurrentDownloads returns the most file downloads that ran at once.
func (f *Fake) MaxConcurrentDownloads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxInFlight
}

var _ upstream.Client = (*Fake)(nil)

// NewFake returns a Fake with an empty Repo store.
func NewFake(t testing.TB) *Fake {
	return &Fake{Repos: repotest.NewFake(t), viewer: map[string]json.RawMessage{}, failures: map[string]error{}, calls: map[string]int{}}
}

// SetViewerResponse makes Viewer return body for endpoint and params.
func (f *Fake) SetViewerResponse(endpoint string, params url.Values, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.viewer[endpoint+"?"+params.Encode()] = json.RawMessage(body)
}

// FailNext makes the next call of method return err, to test retries.
func (f *Fake) FailNext(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures[method] = err
}

// Calls returns how many times method was called.
func (f *Fake) Calls(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}

func (f *Fake) enter(method string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[method]++
	if err, ok := f.failures[method]; ok {
		delete(f.failures, method)
		return err
	}
	return nil
}

func (f *Fake) access(ctx context.Context, cred upstream.Credentials, id model.RepoID) (*repo.Repo, error) {
	r, err := f.Repos.Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", upstream.ErrNotFound, id.Path())
	}
	if slices.Contains(f.Gated, id.String()) {
		return nil, upstream.ErrGated
	}
	if r.Visibility != model.VisibilityPublic && (f.RequireToken == "" || cred.Token != f.RequireToken) {
		return nil, fmt.Errorf("%w: %s", upstream.ErrNotFound, id.Path())
	}
	return r, nil
}

func mapRepoErr(err error) error {
	switch {
	case errors.Is(err, repo.ErrRevisionNotFound), errors.Is(err, repo.ErrEntryNotFound), errors.Is(err, repo.ErrNotFound):
		return fmt.Errorf("%w: %w", upstream.ErrNotFound, err)
	default:
		return err
	}
}

// RepoInfo implements upstream.Client.
func (f *Fake) RepoInfo(ctx context.Context, _ upstream.Upstream, cred upstream.Credentials, id model.RepoID, revision string) (*upstream.RepoInfo, error) {
	if err := f.enter("RepoInfo"); err != nil {
		return nil, err
	}
	r, err := f.access(ctx, cred, id)
	if err != nil {
		return nil, err
	}
	sha, err := f.Repos.Resolve(ctx, id, revision)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	entries, err := f.Repos.Tree(ctx, id, sha, "", repo.TreeOpts{Recursive: true})
	if err != nil {
		return nil, mapRepoErr(err)
	}
	info := &upstream.RepoInfo{ID: r.ID, SHA: sha, Private: r.Visibility.Private(), Tags: r.Tags, LastModified: r.LastModified}
	for _, e := range entries {
		if e.Type == model.EntryFile {
			info.Siblings = append(info.Siblings, upstream.Sibling{Path: e.Path, Size: e.Size, LFS: e.LFS})
		}
	}
	raw, err := json.Marshal(map[string]any{"id": r.ID.String(), "sha": sha, "private": info.Private, "tags": r.Tags})
	if err != nil {
		return nil, err
	}
	info.Raw = raw
	return info, nil
}

// Search implements upstream.Client with the Repo store's name search.
func (f *Fake) Search(ctx context.Context, _ upstream.Upstream, cred upstream.Credentials, q upstream.SearchQuery) (*upstream.SearchPage, error) {
	if err := f.enter("Search"); err != nil {
		return nil, err
	}
	viewer := model.Anonymous()
	if f.RequireToken != "" && cred.Token == f.RequireToken {
		viewer = model.Principal{Kind: model.PrincipalUser, IsAdmin: true}
	}
	var namespaces []string
	if q.Author != "" {
		namespaces = []string{q.Author}
	}
	res, err := f.Repos.List(ctx, repo.ListOptions{Viewer: viewer, Type: q.Type, Search: q.Search, Namespaces: namespaces, Tags: q.Filter, Limit: q.Limit, Cursor: q.Cursor, Sort: "updated", Desc: true})
	if err != nil {
		return nil, err
	}
	page := &upstream.SearchPage{NextCursor: res.NextCursor}
	for _, r := range res.Repos {
		sha, _ := f.Repos.Resolve(ctx, r.ID, "")
		page.Repos = append(page.Repos, upstream.RepoSummary{ID: r.ID, SHA: sha, Tags: r.Tags, LastModified: r.LastModified, Private: r.Visibility.Private()})
	}
	return page, nil
}

// Tree implements upstream.Client.
func (f *Fake) Tree(ctx context.Context, _ upstream.Upstream, cred upstream.Credentials, id model.RepoID, revision, path string, recursive bool) ([]model.Entry, error) {
	if err := f.enter("Tree"); err != nil {
		return nil, err
	}
	if _, err := f.access(ctx, cred, id); err != nil {
		return nil, err
	}
	sha, err := f.Repos.Resolve(ctx, id, revision)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	entries, err := f.Repos.Tree(ctx, id, sha, path, repo.TreeOpts{Recursive: recursive})
	return entries, mapRepoErr(err)
}

// Download implements upstream.Client.
func (f *Fake) Download(ctx context.Context, _ upstream.Upstream, cred upstream.Credentials, id model.RepoID, commit model.CommitSHA, path string, rng *blob.Range) (io.ReadCloser, upstream.FileMeta, error) {
	if err := f.enter("Download"); err != nil {
		return nil, upstream.FileMeta{}, err
	}
	if err := f.slowFile(ctx); err != nil {
		return nil, upstream.FileMeta{}, err
	}
	if _, err := f.access(ctx, cred, id); err != nil {
		return nil, upstream.FileMeta{}, err
	}
	rc, e, err := f.Repos.OpenFile(ctx, id, commit, path, rng)
	if err != nil {
		return nil, upstream.FileMeta{}, mapRepoErr(err)
	}
	return rc, upstream.FileMeta{Commit: commit, ETag: e.ETag(), Size: e.Size, LFS: e.IsLFS()}, nil
}

// FetchPack implements upstream.Client with `git pack-objects`.
func (f *Fake) FetchPack(ctx context.Context, _ upstream.Upstream, cred upstream.Credentials, id model.RepoID, have []model.CommitSHA) (*upstream.FetchResult, error) {
	if err := f.enter("FetchPack"); err != nil {
		return nil, err
	}
	if _, err := f.access(ctx, cred, id); err != nil {
		return nil, err
	}
	refs, err := f.Repos.Refs(ctx, id)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	res := &upstream.FetchResult{Refs: map[string]model.CommitSHA{}}
	var revs strings.Builder
	for _, group := range [][]repo.Ref{refs.Branches, refs.Tags, refs.PullRequests, refs.Converts} {
		for _, r := range group {
			res.Refs[r.Name] = r.Target
			revs.WriteString(string(r.Target) + "\n")
		}
	}
	for _, h := range have {
		revs.WriteString("^" + string(h) + "\n")
	}
	if len(res.Refs) == 0 {
		return res, nil
	}
	dir, err := f.Repos.Dir(id)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "git", "pack-objects", "--stdout", "--revs", "-q")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	cmd.Stdin = strings.NewReader(revs.String())
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("pack-objects: %w: %s", err, stderr.String())
	}
	if objectCount(out.Bytes()) > 0 {
		res.Pack = io.NopCloser(&out)
	}
	return res, nil
}

// objectCount reads the object count from a pack header.
func objectCount(pack []byte) uint32 {
	if len(pack) < 12 || string(pack[:4]) != "PACK" {
		return 0
	}
	return uint32(pack[8])<<24 | uint32(pack[9])<<16 | uint32(pack[10])<<8 | uint32(pack[11])
}

// Viewer implements upstream.Client.
func (f *Fake) Viewer(_ context.Context, _ upstream.Upstream, _ upstream.Credentials, endpoint string, params url.Values) (json.RawMessage, error) {
	if err := f.enter("Viewer"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	body, ok := f.viewer[endpoint+"?"+params.Encode()]
	if !ok {
		return nil, fmt.Errorf("%w: viewer %s %s", upstream.ErrNotFound, endpoint, strconv.Quote(params.Encode()))
	}
	return body, nil
}
