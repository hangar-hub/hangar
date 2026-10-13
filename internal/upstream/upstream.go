// Package upstream talks to Upstreams (huggingface.co, another Hangar, a
// corporate mirror) through the Hub API and git smart HTTP, and plans and
// runs Pulls, Discover searches and Dependency resolution
// (docs/spec/06-upstreams-and-pulls.md). Client is the one client every
// feature uses; upstreamtest has a fake backed by real git repositories.
package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/model"
)

// Errors.
var (
	// ErrNotFound means the Repo, Revision or file does not exist upstream,
	// or the credentials cannot see it.
	ErrNotFound = errors.New("not found on upstream")
	// ErrUnauthorized means the Upstream rejected the credentials.
	ErrUnauthorized = errors.New("upstream rejected the credentials")
	// ErrGated means the upstream Repo is gated and the user has no access.
	ErrGated = errors.New("upstream repo is gated")
	// ErrUnavailable means the Upstream failed after retries (5xx or network).
	ErrUnavailable = errors.New("upstream unavailable")
)

// RateLimitError means the Upstream answered 429; RetryAfter says when to try again.
type RateLimitError struct{ RetryAfter time.Duration }

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("upstream rate limit; retry after %s", e.RetryAfter)
}

// Upstream is a configured Upstream.
type Upstream struct {
	ID uuid.UUID
	// Name is the config ID, such as "huggingface".
	Name            string
	DisplayName     string
	BaseURL         string
	ViewerURL       string
	Enabled         bool
	NamespacePrefix string
}

// DatasetViewerURL returns the Upstream's Dataset Viewer base URL: ViewerURL,
// or the base URL with `datasets-server.` added to the host (DATA-7).
func (u Upstream) DatasetViewerURL() string {
	if u.ViewerURL != "" {
		return strings.TrimRight(u.ViewerURL, "/")
	}
	p, err := url.Parse(u.BaseURL)
	if err != nil || p.Host == "" {
		return ""
	}
	p.Host = "datasets-server." + p.Host
	p.Path = ""
	return strings.TrimRight(p.String(), "/")
}

// Credentials are what a request presents: the requesting user's upstream
// Token, else the Instance Token, else nothing (anonymous).
type Credentials struct {
	Token string
}

// Sibling is a file in upstream Repo info.
type Sibling struct {
	Path string
	Size int64
	// LFS is set for LFS files.
	LFS *model.LFSInfo
}

// RepoInfo is upstream `model_info` or `dataset_info` for a Revision.
type RepoInfo struct {
	ID           model.RepoID
	SHA          model.CommitSHA
	Private      bool
	Gated        string
	Siblings     []Sibling
	CardData     map[string]any
	Tags         []string
	PipelineTag  string
	LibraryName  string
	Downloads    int64
	Likes        int64
	LastModified time.Time
	// Raw is the full JSON the Upstream returned, for Discover details.
	Raw json.RawMessage
}

// SearchQuery maps to the Upstream's GET /api/models or /api/datasets.
type SearchQuery struct {
	Type   model.RepoType
	Search string
	Author string
	// Filter holds tags, such as gguf or task_categories:text-classification.
	Filter []string
	// Sort is trending, downloads, likes or lastModified.
	Sort   string
	Limit  int
	Cursor string
}

// RepoSummary is one search result.
type RepoSummary struct {
	ID           model.RepoID
	SHA          model.CommitSHA
	Tags         []string
	PipelineTag  string
	LibraryName  string
	Downloads    int64
	Likes        int64
	LastModified time.Time
	Private      bool
	Gated        string
}

// SearchPage is a page of search results.
type SearchPage struct {
	Repos      []RepoSummary
	NextCursor string
}

// FileMeta describes a file download.
type FileMeta struct {
	Commit model.CommitSHA
	// ETag is the SHA-256 for LFS files and the git blob SHA-1 otherwise.
	ETag string
	// Size is the whole file's size.
	Size int64
	LFS  bool
}

// FetchResult is a git fetch from an Upstream: a pack holding every object
// reachable from Refs that the caller does not already have, stored
// verbatim so Commit SHAs stay identical (04-git.md "Mirrors").
type FetchResult struct {
	// Pack is nil when there is nothing new.
	Pack io.ReadCloser
	// Refs maps every fetched ref (refs/heads/*, refs/tags/*, refs/pr/*,
	// refs/convert/*) to its Commit.
	Refs map[string]model.CommitSHA
}

// Client talks to Upstreams. Requests send `User-Agent: hangar/<version>`,
// honour Retry-After, back off on 429 and 5xx, and never ask for Xet.
type Client interface {
	// RepoInfo returns upstream Repo info at a Revision ("" means the default branch).
	RepoInfo(ctx context.Context, u Upstream, cred Credentials, id model.RepoID, revision string) (*RepoInfo, error)
	// Search lists upstream Repos.
	Search(ctx context.Context, u Upstream, cred Credentials, q SearchQuery) (*SearchPage, error)
	// Tree lists files at a Revision, recursively when asked.
	Tree(ctx context.Context, u Upstream, cred Credentials, id model.RepoID, revision, path string, recursive bool) ([]model.Entry, error)
	// Download streams a file, or a range of it, at a Commit.
	Download(ctx context.Context, u Upstream, cred Credentials, id model.RepoID, commit model.CommitSHA, path string, rng *blob.Range) (io.ReadCloser, FileMeta, error)
	// FetchPack fetches git objects not reachable from have.
	FetchPack(ctx context.Context, u Upstream, cred Credentials, id model.RepoID, have []model.CommitSHA) (*FetchResult, error)
	// Viewer calls the Upstream's Dataset Viewer API, such as endpoint
	// "/first-rows" with dataset, config and split parameters, and returns
	// the JSON body.
	Viewer(ctx context.Context, u Upstream, cred Credentials, endpoint string, params url.Values) (json.RawMessage, error)
}
