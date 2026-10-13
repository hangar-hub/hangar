package hfapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/community"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/telemetry"
	"github.com/jamesagarside/hangar/internal/webhook"
)

// spoolMemory is how many bytes of regular file content one commit keeps
// in memory; the rest is spooled to temporary files.
const spoolMemory = 32 << 20

// MsgFileMissing is the text huggingface_hub matches on a failed
// deletedFile to add its "is it a folder?" hint (contract §7.7).
const MsgFileMissing = "A file with this name doesn't exist"

// commitHeader is the first NDJSON line of a commit.
type commitHeader struct {
	Summary      string `json:"summary"`
	Description  string `json:"description"`
	ParentCommit string `json:"parentCommit"`
}

// commitRequest is a parsed NDJSON commit body.
type commitRequest struct {
	header     commitHeader
	ops        []repo.Operation
	regularLen int64
	temp       []*os.File
}

// close removes spooled content.
func (c *commitRequest) close() {
	for _, f := range c.temp {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}
	c.temp = nil
}

// commitError is a client error found while reading a commit body.
type commitError struct {
	status int
	msg    string
}

func (e *commitError) Error() string { return e.msg }

func badCommit(format string, args ...any) error {
	return &commitError{status: http.StatusBadRequest, msg: fmt.Sprintf(format, args...)}
}

// readCommit parses an NDJSON commit body line by line (contract §7.7).
// Regular file content is decoded as it arrives and kept in memory up to
// spoolMemory, then spooled to temporary files, so no request holds every
// file in memory at once.
func (h *Handler) readCommit(body io.Reader) (*commitRequest, error) {
	c := &commitRequest{}
	br := bufio.NewReaderSize(body, 64<<10)
	var inMemory int64
	first := true
	for {
		line, err := readLine(br, h.d.Settings.CommitLineMax)
		if err != nil && !errors.Is(err, io.EOF) {
			c.close()
			return nil, err
		}
		if len(bytes.TrimSpace(line)) > 0 {
			var item struct {
				Key   string          `json:"key"`
				Value json.RawMessage `json:"value"`
			}
			if jerr := json.Unmarshal(line, &item); jerr != nil {
				c.close()
				return nil, badCommit("Line %d of the commit body is not valid JSON.", len(c.ops)+1)
			}
			if first != (item.Key == "header") {
				c.close()
				return nil, badCommit("The commit body must start with exactly one header line.")
			}
			first = false
			if perr := h.parseCommitLine(c, item.Key, item.Value, &inMemory); perr != nil {
				c.close()
				return nil, perr
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
	}
	if first {
		return nil, badCommit("The commit body is empty.")
	}
	return c, nil
}

// readLine reads one line of at most limit bytes.
func readLine(br *bufio.Reader, limit int64) ([]byte, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		line = append(line, chunk...)
		if int64(len(line)) > limit {
			return nil, &commitError{status: http.StatusRequestEntityTooLarge, msg: "A line of the commit body is larger than " + strconv.FormatInt(limit, 10) + " bytes. Upload large files with LFS."}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return line, err
	}
}

func (h *Handler) parseCommitLine(c *commitRequest, key string, value json.RawMessage, inMemory *int64) error {
	switch key {
	case "header":
		if err := json.Unmarshal(value, &c.header); err != nil {
			return badCommit("The commit header is not valid.")
		}
		if strings.TrimSpace(c.header.Summary) == "" {
			return badCommit("The commit summary is empty.")
		}
	case "file":
		var v struct {
			Path     string `json:"path"`
			Content  string `json:"content"`
			Encoding string `json:"encoding"`
		}
		if err := json.Unmarshal(value, &v); err != nil {
			return badCommit("A file line is not valid.")
		}
		var content []byte
		switch strings.ToLower(v.Encoding) {
		case "base64":
			b, err := base64.StdEncoding.DecodeString(v.Content)
			if err != nil {
				return badCommit("The content of %s is not valid base64.", v.Path)
			}
			content = b
		case "", "utf-8", "utf8":
			content = []byte(v.Content)
		default:
			return badCommit("Unknown encoding %q for %s. Use base64.", v.Encoding, v.Path)
		}
		c.regularLen += int64(len(content))
		r, err := c.spool(content, inMemory)
		if err != nil {
			return err
		}
		c.ops = append(c.ops, repo.Operation{Kind: repo.OpAdd, Path: v.Path, Content: r})
	case "lfsFile":
		var v struct {
			Path string `json:"path"`
			Algo string `json:"algo"`
			OID  string `json:"oid"`
			Size *int64 `json:"size"`
		}
		if err := json.Unmarshal(value, &v); err != nil {
			return badCommit("An lfsFile line is not valid.")
		}
		if v.Algo != "" && v.Algo != "sha256" {
			return badCommit("Unsupported hash algorithm %q for %s. Use sha256.", v.Algo, v.Path)
		}
		op := repo.Operation{Kind: repo.OpAddLFS, Path: v.Path, OID: strings.ToLower(v.OID)}
		if v.Size != nil {
			op.Size = *v.Size
		}
		c.ops = append(c.ops, op)
	case "deletedFile", "deletedFolder":
		var v struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(value, &v); err != nil {
			return badCommit("A %s line is not valid.", key)
		}
		kind := repo.OpDelete
		if key == "deletedFolder" {
			kind = repo.OpDeleteFolder
		}
		c.ops = append(c.ops, repo.Operation{Kind: kind, Path: v.Path})
	default:
		return badCommit("Unknown commit line key %q.", key)
	}
	return nil
}

// spool keeps content in memory while the commit is under spoolMemory and
// writes it to a temporary file after that.
func (c *commitRequest) spool(content []byte, inMemory *int64) (io.Reader, error) {
	if *inMemory+int64(len(content)) <= spoolMemory {
		*inMemory += int64(len(content))
		return bytes.NewReader(content), nil
	}
	f, err := os.CreateTemp("", "hangar-commit-*")
	if err != nil {
		return nil, err
	}
	c.temp = append(c.temp, f)
	if _, err := f.Write(content); err != nil {
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return f, nil
}

// commit serves POST /api/{type}s/{repo}/commit/{rev}[?create_pr=1]
// (contract §7.7).
func (h *Handler) commit(w http.ResponseWriter, r *http.Request, ref repoRef, rev string) {
	ctx := r.Context()
	p := httpx.Principal(ctx)
	createPR := httpx.QueryBool(r.URL.Query(), "create_pr", false)
	_, isPRRef := model.ParsePRRef(rev)
	action := auth.ActionWrite
	if createPR || isPRRef {
		action = auth.ActionDiscuss
	}
	rp, ok := h.load(w, r, ref, action)
	if !ok {
		return
	}
	if isPRRef && !createPR && !h.mayPushToPR(ctx, rp, rev, p) {
		h.writeForbidden(w, r, "Only the author of the pull request and users with write access can commit to "+rev+".")
		return
	}
	req, err := h.readCommit(r.Body)
	if err != nil {
		var ce *commitError
		if errors.As(err, &ce) {
			httpx.WriteHubError(w, r, &httpx.HubError{Status: ce.status, Message: ce.msg})
			return
		}
		h.writeBadRequest(w, r, "The commit body could not be read.")
		return
	}
	defer req.close()
	params := repo.CommitParams{
		Repo: rp.ID, Revision: rev, Summary: req.header.Summary, Description: req.header.Description,
		Operations: req.ops, Actor: p,
	}
	base, err := h.d.Repos.Resolve(ctx, rp.ID, rev)
	if errors.Is(err, repo.ErrRevisionNotFound) && isPRRef {
		// The first Commit to a pull request opened with create_pull_request
		// creates its ref from the base branch.
		base, err = h.prBase(ctx, rp, rev)
		params.CreateFrom = base
	}
	if err != nil {
		if errors.Is(err, repo.ErrRevisionNotFound) || errors.Is(err, community.ErrNotFound) {
			httpx.HubRevisionNotFound(w, r, rev)
			return
		}
		h.serviceError(w, r, rp.ID, err)
		return
	}
	if req.header.ParentCommit != "" {
		parent, err := h.parentCommit(ctx, rp, req.header.ParentCommit)
		if err != nil {
			httpx.WriteHubError(w, r, &httpx.HubError{Status: http.StatusPreconditionFailed, Message: "Parent commit " + req.header.ParentCommit + " is not the head of " + rev + "."})
			return
		}
		if createPR && parent != base {
			httpx.WriteHubError(w, r, &httpx.HubError{Status: http.StatusPreconditionFailed, Message: "Parent commit " + req.header.ParentCommit + " is not the head of " + rev + "."})
			return
		}
		params.ParentCommit = parent
	}
	if err := repo.ValidateCommitParams(params); err != nil {
		h.writeBadRequest(w, r, errMessage(err))
		return
	}
	var disc *community.Discussion
	if createPR {
		disc, err = h.d.Community.Create(ctx, community.CreateParams{
			RepoUUID: rp.UUID, Repo: rp.ID, Title: req.header.Summary, Description: req.header.Description,
			PullRequest: true, Base: rev, Author: p,
		})
		if err != nil {
			h.communityError(w, r, rp, err)
			return
		}
		params.Revision, params.CreateFrom, params.ParentCommit = disc.PRRef, base, ""
	}
	res, err := h.d.Repos.Commit(ctx, params)
	if err != nil {
		if errors.Is(err, repo.ErrEntryNotFound) {
			httpx.WriteHubError(w, r, &httpx.HubError{Status: http.StatusNotFound, Code: httpx.CodeEntryNotFound, Message: MsgFileMissing + ": " + err.Error()})
			return
		}
		h.serviceError(w, r, rp.ID, err)
		return
	}
	if in := h.d.Instruments; in != nil && in.UploadBytes != nil && req.regularLen > 0 {
		in.UploadBytes.Add(ctx, req.regularLen, metric.WithAttributes(attribute.String(telemetry.MetricProtocol, string(telemetry.ProtocolHub))))
	}
	if !res.NoOp {
		h.emit(r, webhook.RepoUpdated, rp, map[string]any{"refs": []map[string]any{{"ref": res.Ref, "before": string(res.Parent), "after": string(res.SHA)}}, "via": "hub_api"})
	}
	out := map[string]any{
		"success":   true,
		"commitOid": string(res.SHA),
		"commitUrl": h.repoURL(rp.ID) + "/commit/" + string(res.SHA),
	}
	if disc != nil {
		out["pullRequestUrl"] = h.repoURL(rp.ID) + "/discussions/" + strconv.Itoa(disc.Num)
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

// prBase resolves the base branch of the pull request a `refs/pr/N` ref
// belongs to.
func (h *Handler) prBase(ctx context.Context, rp *repo.Repo, ref string) (model.CommitSHA, error) {
	num, _ := model.ParsePRRef(ref)
	d, err := h.d.Community.Get(ctx, rp.UUID, num)
	if err != nil {
		return "", err
	}
	if !d.IsPullRequest() || d.Status == community.StatusMerged || d.Status == community.StatusClosed {
		return "", repo.ErrRevisionNotFound
	}
	base := d.PRBase
	if base == "" {
		base = rp.DefaultBranch
	}
	return h.d.Repos.Resolve(ctx, rp.ID, base)
}

// parentCommit expands a `parentCommit` of 5 to 40 hex digits to a full SHA.
func (h *Handler) parentCommit(ctx context.Context, rp *repo.Repo, s string) (model.CommitSHA, error) {
	s = strings.ToLower(s)
	if len(s) < 5 || len(s) > 40 || strings.Trim(s, "0123456789abcdef") != "" {
		return "", errors.New("invalid parent commit")
	}
	if model.IsFullSHA(s) {
		return model.CommitSHA(s), nil
	}
	return h.d.Repos.Resolve(ctx, rp.ID, s)
}

// mayPushToPR reports whether p may commit to a pull request ref: writers
// can, and so can the pull request's author.
func (h *Handler) mayPushToPR(ctx context.Context, rp *repo.Repo, ref string, p model.Principal) bool {
	if h.d.Authz.Can(ctx, p, auth.ActionWrite, target(rp)) == nil {
		return true
	}
	num, _ := model.ParsePRRef(ref)
	d, err := h.d.Community.Get(ctx, rp.UUID, num)
	return err == nil && d.IsPullRequest() && !p.IsAnonymous() && strings.EqualFold(d.Author, p.Username)
}
