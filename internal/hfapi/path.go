package hfapi

import (
	"net/http"
	"net/url"
	"strings"
)

// reqPath is a request path split on '/' twice: segs holds the unescaped
// segments and esc the escaped ones, so redirects can rebuild the path
// byte for byte.
type reqPath struct {
	segs []string
	esc  []string
}

func parsePath(r *http.Request) (reqPath, error) {
	raw := strings.TrimPrefix(r.URL.EscapedPath(), "/")
	if raw == "" {
		return reqPath{}, nil
	}
	esc := strings.Split(raw, "/")
	segs := make([]string, len(esc))
	for i, s := range esc {
		u, err := url.PathUnescape(s)
		if err != nil {
			return reqPath{}, err
		}
		segs[i] = u
	}
	return reqPath{segs: segs, esc: esc}, nil
}

func (p reqPath) len() int { return len(p.segs) }

// shift drops the first n segments.
func (p reqPath) shift(n int) reqPath {
	if n > len(p.segs) {
		n = len(p.segs)
	}
	return reqPath{segs: p.segs[n:], esc: p.esc[n:]}
}

// at returns segment i, or "".
func (p reqPath) at(i int) string {
	if i < len(p.segs) {
		return p.segs[i]
	}
	return ""
}

// joined returns the unescaped segments from i on, joined with '/'.
func (p reqPath) joined(i int) string {
	if i >= len(p.segs) {
		return ""
	}
	return strings.Join(p.segs[i:], "/")
}

// takeRevision reads a Revision at the start of p. Revisions are one
// percent-encoded segment (`refs%2Fpr%2F1`), but the special refs
// `refs/pr/N` and `refs/convert/<name>` are also accepted unencoded, as the
// client's SPECIAL_REFS_REVISION_REGEX allows.
func takeRevision(p reqPath) (string, reqPath) {
	if p.len() >= 3 && p.segs[0] == "refs" && (p.segs[1] == "pr" || p.segs[1] == "convert") {
		return "refs/" + p.segs[1] + "/" + p.segs[2], p.shift(3)
	}
	if p.len() == 0 {
		return "", p
	}
	return p.segs[0], p.shift(1)
}

// repoActions are the path segments that follow `ns/name` in the Repo API.
// A single-segment Repo ID shows up as one of them in the name position.
var repoActions = map[string]bool{
	"revision": true, "tree": true, "paths-info": true, "refs": true, "commits": true,
	"branch": true, "tag": true, "settings": true, "preupload": true, "commit": true,
	"discussions": true, "like": true, "xet-write-token": true, "xet-read-token": true,
	"parquet": true, "croissant": true,
}
