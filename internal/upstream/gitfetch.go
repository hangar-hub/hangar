package upstream

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/go-git/go-git/v6/plumbing/format/pktline"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp/sideband"

	"github.com/jamesagarside/hangar/internal/model"
)

// mirrorRefPrefixes are the refs a Mirror fetches (04-git.md "Mirrors").
var mirrorRefPrefixes = []string{"refs/heads/", "refs/tags/", "refs/pr/", "refs/convert/"}

func mirrorRef(name string) bool {
	for _, p := range mirrorRefPrefixes {
		if strings.HasPrefix(name, p) && model.ValidateRefName(name) == nil {
			return true
		}
	}
	return false
}

// advertisedRef is one ref from ls-refs or a v0 advertisement.
type advertisedRef struct {
	name   string
	oid    model.CommitSHA
	peeled model.CommitSHA
}

// FetchPack implements Client with git smart HTTP: protocol v2 (ls-refs,
// then fetch with every advertised object as a want and the caller's
// Commits as haves), or protocol v0 when the Upstream does not speak v2.
// The pack streams from the response as the Upstream sent it, with full
// blobs and no thin deltas, so Commit SHAs stay identical. Refs maps each
// ref to the object it names (an annotated tag's tag object); Peeled maps
// annotated tags to their Commits.
func (c *HTTPClient) FetchPack(ctx context.Context, u Upstream, cred Credentials, id model.RepoID, have []model.CommitSHA) (*FetchResult, error) {
	base := apiBase(u) + "/" + id.Type.URLPrefix() + escapeRepo(id)
	adv, err := c.gitRequest(ctx, u, cred, http.MethodGet, base+"/info/refs?service=git-upload-pack", nil, true)
	if err != nil {
		return nil, err
	}
	defer func() { _ = adv.Body.Close() }()
	v2, refs, err := readAdvertisement(adv.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: read upstream ref advertisement: %w", ErrUnavailable, redactErr(err))
	}
	if v2 {
		if refs, err = c.lsRefs(ctx, u, cred, base); err != nil {
			return nil, err
		}
	}
	res := &FetchResult{Refs: map[string]model.CommitSHA{}, Peeled: map[string]model.CommitSHA{}}
	haveSet := map[model.CommitSHA]bool{}
	for _, h := range have {
		haveSet[h] = true
	}
	var wants []model.CommitSHA
	seen := map[model.CommitSHA]bool{}
	for _, r := range refs {
		if !mirrorRef(r.name) {
			continue
		}
		res.Refs[r.name] = r.oid
		if !r.peeled.IsZero() {
			res.Peeled[r.name] = r.peeled
		}
		target := r.oid
		if haveSet[target] || seen[target] {
			continue
		}
		seen[target] = true
		wants = append(wants, target)
	}
	if len(wants) == 0 {
		return res, nil
	}
	sort.Slice(wants, func(i, j int) bool { return wants[i] < wants[j] })
	var body bytes.Buffer
	if v2 {
		writeV2Fetch(&body, c.userAgent(), wants, have)
	} else {
		writeV0Fetch(&body, c.userAgent(), wants, have)
	}
	resp, err := c.gitRequest(ctx, u, cred, http.MethodPost, base+"/git-upload-pack", body.Bytes(), v2)
	if err != nil {
		return nil, err
	}
	pack, err := packStream(resp.Body, v2)
	if err != nil {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%w: read upstream pack: %w", ErrUnavailable, redactErr(err))
	}
	br := bufio.NewReaderSize(pack, 64<<10)
	hdr, err := br.Peek(12)
	if err != nil {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%w: read upstream pack header: %w", ErrUnavailable, redactErr(err))
	}
	if string(hdr[:4]) != "PACK" {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%w: the upstream did not send a git pack", ErrUnavailable)
	}
	if count := uint32(hdr[8])<<24 | uint32(hdr[9])<<16 | uint32(hdr[10])<<8 | uint32(hdr[11]); count == 0 {
		_ = resp.Body.Close()
		return res, nil
	}
	res.Pack = &packReadCloser{r: br, body: resp.Body}
	return res, nil
}

type packReadCloser struct {
	r    io.Reader
	body io.Closer
}

func (p *packReadCloser) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if err != nil && !errors.Is(err, io.EOF) {
		err = redactErr(err)
	}
	return n, err
}

func (p *packReadCloser) Close() error { return p.body.Close() }

func (c *HTTPClient) gitRequest(ctx context.Context, u Upstream, cred Credentials, method, target string, body []byte, v2 bool) (*http.Response, error) {
	h := http.Header{}
	if v2 {
		h.Set("Git-Protocol", "version=2")
	}
	route := "/{repo}/info/refs"
	if method == http.MethodPost {
		h.Set("Content-Type", "application/x-git-upload-pack-request")
		h.Set("Accept", "application/x-git-upload-pack-result")
		route = "/{repo}/git-upload-pack"
	}
	return c.do(ctx, request{method: method, url: target, route: route, body: body, header: h, cred: cred, git: true, upName: u.Name})
}

// readAdvertisement reads GET info/refs. It reports whether the server
// speaks protocol v2; for v0 it returns the advertised refs.
func readAdvertisement(r io.Reader) (bool, []advertisedRef, error) {
	l, line, err := pktline.ReadLine(r)
	if err != nil {
		return false, nil, err
	}
	if l > pktline.LenSize && strings.HasPrefix(string(line), "# service=") {
		// Smart HTTP preamble, then a flush.
		if l, _, err = pktline.ReadLine(r); err != nil || l != pktline.Flush {
			return false, nil, fmt.Errorf("malformed smart HTTP preamble (%w)", err)
		}
		if l, line, err = pktline.ReadLine(r); err != nil {
			return false, nil, err
		}
	}
	if l == pktline.Flush {
		return false, nil, nil // empty repository
	}
	if strings.TrimSpace(string(line)) == "version 2" {
		return true, nil, nil
	}
	var refs []advertisedRef
	byName := map[string]int{}
	for {
		if l == pktline.Flush {
			return false, refs, nil
		}
		text := strings.TrimSuffix(string(line), "\n")
		if i := strings.IndexByte(text, 0); i >= 0 {
			text = text[:i]
		}
		oid, name, ok := strings.Cut(text, " ")
		if !ok {
			return false, nil, fmt.Errorf("malformed ref advertisement line %q", text)
		}
		sha, err := model.ParseCommitSHA(oid)
		if err != nil {
			return false, nil, err
		}
		if base, ok := strings.CutSuffix(name, "^{}"); ok {
			if i, ok := byName[base]; ok {
				refs[i].peeled = sha
			}
		} else if name != "capabilities^{}" {
			byName[name] = len(refs)
			refs = append(refs, advertisedRef{name: name, oid: sha})
		}
		if l, line, err = pktline.ReadLine(r); err != nil {
			return false, nil, err
		}
	}
}

// lsRefs runs the v2 ls-refs command for the refs a Mirror fetches.
func (c *HTTPClient) lsRefs(ctx context.Context, u Upstream, cred Credentials, base string) ([]advertisedRef, error) {
	var body bytes.Buffer
	_, _ = pktline.WriteString(&body, "command=ls-refs\n")
	_, _ = pktline.WriteString(&body, "agent="+c.userAgent()+"\n")
	_ = pktline.WriteDelim(&body)
	_, _ = pktline.WriteString(&body, "peel\n")
	for _, p := range mirrorRefPrefixes {
		_, _ = pktline.WriteString(&body, "ref-prefix "+p+"\n")
	}
	_ = pktline.WriteFlush(&body)
	resp, err := c.gitRequest(ctx, u, cred, http.MethodPost, base+"/git-upload-pack", body.Bytes(), true)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var refs []advertisedRef
	for {
		l, line, err := pktline.ReadLine(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("%w: read ls-refs: %w", ErrUnavailable, redactErr(err))
		}
		if l == pktline.Flush || l == pktline.ResponseEnd {
			return refs, nil
		}
		fields := strings.Fields(string(line))
		if len(fields) < 2 {
			return nil, fmt.Errorf("%w: malformed ls-refs line", ErrUnavailable)
		}
		sha, err := model.ParseCommitSHA(fields[0])
		if err != nil {
			// Unborn refs ("unborn HEAD") have no object.
			continue
		}
		ref := advertisedRef{name: fields[1], oid: sha}
		for _, attr := range fields[2:] {
			if v, ok := strings.CutPrefix(attr, "peeled:"); ok {
				if p, err := model.ParseCommitSHA(v); err == nil {
					ref.peeled = p
				}
			}
		}
		refs = append(refs, ref)
	}
}

func writeV2Fetch(w io.Writer, agent string, wants, haves []model.CommitSHA) {
	_, _ = pktline.WriteString(w, "command=fetch\n")
	_, _ = pktline.WriteString(w, "agent="+agent+"\n")
	_ = pktline.WriteDelim(w)
	_, _ = pktline.WriteString(w, "ofs-delta\n")
	_, _ = pktline.WriteString(w, "no-progress\n")
	for _, o := range wants {
		_, _ = pktline.WriteString(w, "want "+string(o)+"\n")
	}
	for _, o := range haves {
		_, _ = pktline.WriteString(w, "have "+string(o)+"\n")
	}
	_, _ = pktline.WriteString(w, "done\n")
	_ = pktline.WriteFlush(w)
}

func writeV0Fetch(w io.Writer, agent string, wants, haves []model.CommitSHA) {
	for i, o := range wants {
		line := "want " + string(o)
		if i == 0 {
			line += " side-band-64k ofs-delta no-progress agent=" + agent
		}
		_, _ = pktline.WriteString(w, line+"\n")
	}
	_ = pktline.WriteFlush(w)
	for _, o := range haves {
		_, _ = pktline.WriteString(w, "have "+string(o)+"\n")
	}
	_, _ = pktline.WriteString(w, "done\n")
}

// packStream skips to the pack data of a fetch response and returns a
// reader of the demultiplexed pack.
func packStream(r io.Reader, v2 bool) (io.Reader, error) {
	for {
		l, line, err := pktline.ReadLine(r)
		if err != nil {
			return nil, err
		}
		text := strings.TrimSpace(string(line))
		switch {
		case v2 && text == "packfile":
			return sideband.NewDemuxer(sideband.Sideband64k, r), nil
		case !v2 && (text == "NAK" || strings.HasPrefix(text, "ACK ")):
			return sideband.NewDemuxer(sideband.Sideband64k, r), nil
		case l == pktline.Flush && !v2:
			return nil, errors.New("the upstream ended the fetch without a pack")
		case strings.HasPrefix(text, "ERR "):
			return nil, errors.New("the upstream refused the fetch: " + strings.TrimPrefix(text, "ERR "))
		}
		// v2 sections before the pack (acknowledgments, shallow-info,
		// wanted-refs) are skipped up to their delimiter.
	}
}
