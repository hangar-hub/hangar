package importer

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jamesagarside/hangar/internal/api/gen"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/upstream"
)

// HTTPTarget is a Target over a Hangar Instance's native API, with a
// Token. Trees come from the Instance's Hub API.
type HTTPTarget struct {
	// BaseURL is the Instance's public URL.
	BaseURL string
	Token   string
	// HTTP sends requests; nil means a client with no overall timeout.
	HTTP *http.Client
	// PollInterval is how often Pulls are polled. Zero means one second.
	PollInterval time.Duration
	// Version goes in the User-Agent.
	Version string
}

var _ Target = (*HTTPTarget)(nil)

func (t *HTTPTarget) client() *http.Client {
	if t.HTTP != nil {
		return t.HTTP
	}
	return &http.Client{}
}

// apiError is a problem detail from the Instance.
type apiError struct {
	Status int
	Detail string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("the Instance answered %d: %s", e.Status, e.Detail)
}

func (t *HTTPTarget) call(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(t.BaseURL, "/")+"/api/hangar/v1"+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "hangar-cli/"+t.Version)
	if t.Token != "" {
		req.Header.Set("Authorization", "Bearer "+t.Token)
	}
	resp, err := t.client().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var p struct {
			Detail string `json:"detail"`
			Title  string `json:"title"`
		}
		_ = json.Unmarshal(raw, &p)
		if p.Detail == "" {
			p.Detail = p.Title
		}
		return &apiError{Status: resp.StatusCode, Detail: p.Detail}
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func repoPath(id model.RepoID) string {
	return "/repos/" + id.Type.Plural() + "/" + url.PathEscape(id.Namespace) + "/" + url.PathEscape(id.Name)
}

// waitPull polls a pull Job until it finishes.
func (t *HTTPTarget) waitPull(ctx context.Context, job gen.Job) error {
	interval := t.PollInterval
	if interval <= 0 {
		interval = time.Second
	}
	for {
		switch job.Status {
		case gen.JobStatus("succeeded"):
			return nil
		case gen.JobStatus("failed"), gen.JobStatus("cancelled"):
			msg := ""
			if job.Error != nil {
				msg = *job.Error
			}
			if job.Result != nil && (*job.Result)["reason"] == "upstream_not_found" {
				return fmt.Errorf("%w: %s", ErrUpstreamMissing, msg)
			}
			return fmt.Errorf("the Pull failed: %s", msg)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
		if err := t.call(ctx, http.MethodGet, "/pulls/"+job.Id.String(), nil, &job); err != nil {
			return err
		}
	}
}

// PullHistory implements Target. An explicit empty list of paths is a
// history-only Pull.
func (t *HTTPTarget) PullHistory(ctx context.Context, up string, id model.RepoID, revision string) error {
	body := gen.PullCreate{Upstream: up, Repo: id.String(), Type: gen.RepoTypeSingular(id.Type), Selection: &gen.Selection{Paths: &[]string{}}}
	if revision != "" {
		body.Revision = &revision
	}
	var job gen.Job
	if err := t.call(ctx, http.MethodPost, "/pulls", body, &job); err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			return fmt.Errorf("%w: %w", ErrUpstreamMissing, err)
		}
		return err
	}
	return t.waitPull(ctx, job)
}

// PullFiles implements Target.
func (t *HTTPTarget) PullFiles(ctx context.Context, id model.RepoID, paths []string) error {
	var job gen.Job
	if err := t.call(ctx, http.MethodPost, repoPath(id)+"/mirror/pull", gen.Selection{Paths: &paths}, &job); err != nil {
		return err
	}
	return t.waitPull(ctx, job)
}

// Tree implements Target through the Hub API.
func (t *HTTPTarget) Tree(ctx context.Context, id model.RepoID, revision string) ([]model.Entry, error) {
	c := upstream.NewHTTPClient(upstream.HTTPOptions{Version: t.Version})
	return c.Tree(ctx, upstream.Upstream{Name: "instance", BaseURL: t.BaseURL}, upstream.Credentials{Token: t.Token}, id, revision, "", true)
}

// CreateRepo implements Target.
func (t *HTTPTarget) CreateRepo(ctx context.Context, id model.RepoID) error {
	err := t.call(ctx, http.MethodPost, "/repos", gen.RepoCreate{Namespace: id.Namespace, Name: id.Name, Type: gen.RepoTypeSingular(id.Type)}, nil)
	var ae *apiError
	if errors.As(err, &ae) && ae.Status == http.StatusConflict {
		return nil
	}
	return err
}

// plan asks how to send each file.
func (t *HTTPTarget) plan(ctx context.Context, id model.RepoID, files []File) ([]gen.UploadPlanItem, error) {
	req := gen.UploadPlanRequest{Files: make([]gen.UploadFile, 0, len(files))}
	for _, f := range files {
		req.Files = append(req.Files, gen.UploadFile{Path: f.Path, Size: f.Size, Sha256: f.SHA256})
	}
	var plan gen.UploadPlan
	if err := t.call(ctx, http.MethodPost, repoPath(id)+"/uploads", req, &plan); err != nil {
		return nil, err
	}
	if len(plan.Files) != len(files) {
		return nil, fmt.Errorf("the Instance planned %d files, not %d", len(plan.Files), len(files))
	}
	return plan.Files, nil
}

// upload sends a file's parts and completes the upload.
func (t *HTTPTarget) upload(ctx context.Context, id model.RepoID, f File, item gen.UploadPlanItem) error {
	if item.UploadId == nil || item.PartUrls == nil || item.PartSize == nil || *item.PartSize <= 0 {
		return fmt.Errorf("the upload plan for %s has no part URLs", f.Path)
	}
	src, err := os.Open(f.Local)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	base, err := url.Parse(t.BaseURL)
	if err != nil {
		return err
	}
	parts := make([]gen.UploadedPart, 0, len(*item.PartUrls))
	for i, u := range *item.PartUrls {
		off := int64(i) * *item.PartSize
		size := min(*item.PartSize, f.Size-off)
		if size < 0 {
			size = 0
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, io.NewSectionReader(src, off, size))
		if err != nil {
			return err
		}
		req.ContentLength = size
		req.Header.Set("Content-Type", "application/octet-stream")
		// Only Hangar's own part URLs get the Token; presigned URLs carry
		// their own authorisation.
		if pu, err := url.Parse(u); err == nil && strings.EqualFold(pu.Host, base.Host) && t.Token != "" {
			req.Header.Set("Authorization", "Bearer "+t.Token)
		}
		resp, err := t.client().Do(req)
		if err != nil {
			return fmt.Errorf("upload part %d of %s: %w", i+1, f.Path, errors.New(strings.SplitN(err.Error(), "?", 2)[0]))
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode >= 300 {
			return fmt.Errorf("upload part %d of %s: the store answered %d", i+1, f.Path, resp.StatusCode)
		}
		etag := resp.Header.Get("ETag")
		s := size
		parts = append(parts, gen.UploadedPart{Number: i + 1, Etag: etag, Size: &s})
	}
	return t.call(ctx, http.MethodPost, repoPath(id)+"/uploads/"+item.UploadId.String()+"/complete", gen.UploadComplete{Parts: parts}, nil)
}

// Store implements Target.
func (t *HTTPTarget) Store(ctx context.Context, id model.RepoID, f File) error {
	items, err := t.plan(ctx, id, []File{f})
	if err != nil {
		return err
	}
	switch items[0].Action {
	case "exists":
		return nil
	case "upload":
		return t.upload(ctx, id, f, items[0])
	default:
		return fmt.Errorf("%s is too small to be an LFS object on this Instance", f.Path)
	}
}

// Commit implements Target.
func (t *HTTPTarget) Commit(ctx context.Context, id model.RepoID, summary string, files []File) error {
	var ops []gen.CommitOperation
	const batch = 1000
	for start := 0; start < len(files); start += batch {
		chunk := files[start:min(start+batch, len(files))]
		items, err := t.plan(ctx, id, chunk)
		if err != nil {
			return err
		}
		for i, f := range chunk {
			switch items[i].Action {
			case "inline":
				b, err := os.ReadFile(f.Local)
				if err != nil {
					return err
				}
				enc := base64.StdEncoding.EncodeToString(b)
				ops = append(ops, gen.CommitOperation{Op: gen.Add, Path: f.Path, ContentBase64: &enc})
				continue
			case "upload":
				if err := t.upload(ctx, id, f, items[i]); err != nil {
					return err
				}
			}
			sum, size := f.SHA256, f.Size
			ops = append(ops, gen.CommitOperation{Op: gen.AddLfs, Path: f.Path, Sha256: &sum, Size: &size})
		}
	}
	branch := model.DefaultBranch
	return t.call(ctx, http.MethodPost, repoPath(id)+"/commits", gen.CommitRequest{Summary: summary, Branch: &branch, Operations: ops}, nil)
}
