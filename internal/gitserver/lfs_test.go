package gitserver_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/config"
	"github.com/jamesagarside/hangar/internal/gitserver"
	"github.com/jamesagarside/hangar/internal/gitserver/gitservertest"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/repo/repotest"
)

type lfsAction struct {
	Href      string            `json:"href"`
	Header    map[string]string `json:"header"`
	ExpiresIn int64             `json:"expires_in"`
}

type lfsObject struct {
	OID           string                `json:"oid"`
	Size          int64                 `json:"size"`
	Authenticated bool                  `json:"authenticated"`
	Actions       map[string]*lfsAction `json:"actions"`
	Error         *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type lfsBatch struct {
	Transfer string      `json:"transfer"`
	Objects  []lfsObject `json:"objects"`
	HashAlgo string      `json:"hash_algo"`
	Message  string      `json:"message"`
}

func randomContent(t *testing.T, n int) ([]byte, string) {
	t.Helper()
	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)
	sum := sha256.Sum256(b)
	return b, hex.EncodeToString(sum[:])
}

func (f *fixture) batch(t *testing.T, id model.RepoID, token string, body any) (*http.Response, *lfsBatch) {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	resp := f.do(t, http.MethodPost, "/"+id.Path()+".git/info/lfs/objects/batch", token, bytes.NewReader(raw),
		http.Header{"Content-Type": {"application/vnd.git-lfs+json"}, "Accept": {"application/vnd.git-lfs+json"}})
	var out lfsBatch
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return resp, &out
}

func batchReq(op string, transfers []string, objs ...map[string]any) map[string]any {
	req := map[string]any{"operation": op, "objects": objs, "hash_algo": "sha256"}
	if transfers != nil {
		req["transfers"] = transfers
	}
	return req
}

func obj(oid string, size int) map[string]any { return map[string]any{"oid": oid, "size": size} }

// call sends a request to an action href with the action headers and no
// credentials, as git-lfs does for authenticated actions.
func call(t *testing.T, method string, a *lfsAction, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, a.Href, bytes.NewReader(body))
	require.NoError(t, err)
	for k, v := range a.Header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func verifyBody(oid string, size int) []byte {
	b, _ := json.Marshal(obj(oid, size))
	return b
}

func TestLFSBasicUploadDownloadAndPush(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	content, oid := randomContent(t, 3000)

	resp, b := f.batch(t, publicID, tokAlice, batchReq("upload", []string{"basic"}, obj(oid, len(content))))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "application/vnd.git-lfs+json", resp.Header.Get("Content-Type"))
	require.Equal(t, "basic", b.Transfer)
	require.Len(t, b.Objects, 1)
	o := b.Objects[0]
	require.True(t, o.Authenticated)
	up, verify := o.Actions["upload"], o.Actions["verify"]
	require.NotNil(t, up)
	require.NotNil(t, verify)
	require.True(t, strings.HasPrefix(up.Href, f.inst.URL+"/alice/tiny.git/info/lfs/objects/"+oid+"?"))
	require.Positive(t, up.ExpiresIn)

	// A tampered or corrupt upload is refused.
	bad, _ := url.Parse(up.Href)
	q := bad.Query()
	q.Set("size", "1")
	bad.RawQuery = q.Encode()
	require.Equal(t, http.StatusForbidden, call(t, http.MethodPut, &lfsAction{Href: bad.String()}, []byte("x")).StatusCode)
	corrupt := bytes.Clone(content)
	corrupt[0] ^= 1
	require.Equal(t, http.StatusUnprocessableEntity, call(t, http.MethodPut, up, corrupt).StatusCode)
	require.Equal(t, http.StatusNotFound, call(t, http.MethodPost, verify, verifyBody(oid, len(content))).StatusCode)

	require.Equal(t, http.StatusOK, call(t, http.MethodPut, up, content).StatusCode)
	require.Equal(t, http.StatusOK, call(t, http.MethodPost, verify, verifyBody(oid, len(content))).StatusCode)
	require.Equal(t, http.StatusForbidden, call(t, http.MethodPost, verify, verifyBody(oid, 1)).StatusCode, "the body must match the signed scope")

	// A second upload batch deduplicates.
	_, b = f.batch(t, datasetID, tokAlice, batchReq("upload", nil, obj(oid, len(content))))
	require.Nil(t, b.Objects[0].Actions)
	require.Nil(t, b.Objects[0].Error)
	_, b = f.batch(t, datasetID, tokAlice, batchReq("upload", nil, obj(oid, 5)))
	require.Equal(t, http.StatusUnprocessableEntity, b.Objects[0].Error.Code)

	// Before any Commit refers to it, the Repo cannot download it.
	_, b = f.batch(t, publicID, "", batchReq("download", nil, obj(oid, len(content))))
	require.Equal(t, http.StatusNotFound, b.Objects[0].Error.Code)

	// Push a pointer to it.
	dir := f.clone(t, publicID, tokAlice)
	pointer := model.LFSPointer{OID: oid, Size: int64(len(content))}.Bytes()
	writeFile(t, dir, "model.safetensors", pointer)
	f.git.Must(t, dir, "add", "-A")
	f.git.Must(t, dir, "commit", "-q", "-m", "weights")
	f.git.Must(t, dir, "push", "-q", "origin", "main")
	rc, e, err := f.repos.OpenFile(context.Background(), publicID, model.CommitSHA(f.resolve(t, publicID, "main")), "model.safetensors", nil)
	require.NoError(t, err)
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	require.Equal(t, content, got)
	require.Equal(t, oid, e.LFS.OID)

	// Now anyone who can read the Repo can download it, with ranges.
	_, b = f.batch(t, publicID, "", batchReq("download", nil, obj(oid, len(content))))
	o = b.Objects[0]
	require.Nil(t, o.Error)
	require.True(t, o.Authenticated)
	dl := o.Actions["download"]
	resp = call(t, http.MethodGet, dl, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "application/octet-stream", resp.Header.Get("Content-Type"))
	require.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
	require.Equal(t, "attachment", resp.Header.Get("Content-Disposition"))
	require.Equal(t, content, []byte(readAll(t, resp.Body)))
	req, _ := http.NewRequest(http.MethodGet, dl.Href, nil)
	req.Header.Set("Range", "bytes=10-19")
	rr, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer rr.Body.Close()
	require.Equal(t, http.StatusPartialContent, rr.StatusCode)
	require.Equal(t, fmt.Sprintf("bytes 10-19/%d", len(content)), rr.Header.Get("Content-Range"))
	require.Equal(t, content[10:20], []byte(readAll(t, rr.Body)))
	req.Header.Set("Range", "bytes=999999-")
	rr2, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = rr2.Body.Close()
	require.Equal(t, http.StatusRequestedRangeNotSatisfiable, rr2.StatusCode)
	head := call(t, http.MethodHead, dl, nil)
	require.Equal(t, http.StatusOK, head.StatusCode)
	require.Equal(t, fmt.Sprint(len(content)), head.Header.Get("Content-Length"))

	// A download URL is bound to its Repo.
	other := strings.Replace(dl.Href, "/alice/tiny.git/", "/alice/data.git/", 1)
	require.Equal(t, http.StatusForbidden, call(t, http.MethodGet, &lfsAction{Href: other}, nil).StatusCode)

	// A wrong size in a download request is reported per object.
	_, b = f.batch(t, publicID, "", batchReq("download", nil, obj(oid, 1)))
	require.Equal(t, http.StatusUnprocessableEntity, b.Objects[0].Error.Code)
}

func TestLFSPushRefusesMissingObjects(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, oid := randomContent(t, 100)
	dir := f.clone(t, publicID, tokAlice)
	writeFile(t, dir, "model.bin", model.LFSPointer{OID: oid, Size: 100}.Bytes())
	f.git.Must(t, dir, "add", "-A")
	f.git.Must(t, dir, "commit", "-q", "-m", "pointer")
	out, err := f.git.Run(context.Background(), dir, "push", "origin", "main")
	require.Error(t, err)
	require.Contains(t, out, "Hangar does not have")

	// A pointer whose size disagrees with the stored object is refused.
	content, oid2 := randomContent(t, 50)
	_, err = f.repos.LFS.Put(context.Background(), content)
	require.NoError(t, err)
	writeFile(t, dir, "model.bin", model.LFSPointer{OID: oid2, Size: 51}.Bytes())
	f.git.Must(t, dir, "add", "-A")
	f.git.Must(t, dir, "commit", "-q", "--amend", "-m", "pointer")
	out, err = f.git.Run(context.Background(), dir, "push", "origin", "main")
	require.Error(t, err)
	require.Contains(t, out, "says 51 bytes")
}

// slowVerify reports chosen objects as unverified for a number of checks.
type slowVerify struct {
	*repotest.FakeLFS
	mu      sync.Mutex
	pending map[string]int
}

func (s *slowVerify) set(oid string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending[oid] = n
}

func (s *slowVerify) StatMany(ctx context.Context, oids []string) (map[string]repo.LFSObject, error) {
	out, err := s.FakeLFS.StatMany(ctx, oids)
	s.mu.Lock()
	defer s.mu.Unlock()
	for oid, o := range out {
		if s.pending[oid] > 0 {
			s.pending[oid]--
			o.Verified = false
			out[oid] = o
		}
	}
	return out, err
}

func TestLFSPushWaitsForVerification(t *testing.T) {
	t.Parallel()
	var slow *slowVerify
	f := newFixture(t, func(o *gitservertest.Options, f *fixture) {
		slow = &slowVerify{FakeLFS: f.repos.LFS, pending: map[string]int{}}
		o.LFS = slow
		o.Config.VerifyWait = 2 * time.Second
	})
	ctx := context.Background()
	content, oid := randomContent(t, 64)
	_, err := f.repos.LFS.Put(ctx, content)
	require.NoError(t, err)
	slow.set(oid, 2)
	dir := f.clone(t, publicID, tokAlice)
	writeFile(t, dir, "a.bin", model.LFSPointer{OID: oid, Size: 64}.Bytes())
	f.git.Must(t, dir, "add", "-A")
	f.git.Must(t, dir, "commit", "-q", "-m", "a")
	f.git.Must(t, dir, "push", "-q", "origin", "main")

	// An object that never finishes verifying times out with a clear error.
	content, oid = randomContent(t, 65)
	_, err = f.repos.LFS.Put(ctx, content)
	require.NoError(t, err)
	slow.set(oid, 1<<30)
	writeFile(t, dir, "b.bin", model.LFSPointer{OID: oid, Size: 65}.Bytes())
	f.git.Must(t, dir, "add", "-A")
	f.git.Must(t, dir, "commit", "-q", "-m", "b")
	out, err := f.git.Run(ctx, dir, "push", "origin", "main")
	require.Error(t, err)
	require.Contains(t, out, "still being verified")

	// Downloads of an object still being verified are refused per object.
	slow.set(oid, 0)
	f.git.Must(t, dir, "push", "-q", "origin", "main")
	slow.set(oid, 1)
	_, b := f.batch(t, publicID, "", batchReq("download", nil, obj(oid, 65)))
	require.Contains(t, b.Objects[0].Error.Message, "still being verified")
}

func TestLFSMultipartThroughHangar(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	content, oid := randomContent(t, (1<<20)+700<<10) // over the 1 MiB threshold, 7 parts of 256 KiB
	small, smallOID := randomContent(t, 10)

	_, b := f.batch(t, publicID, tokAlice, batchReq("upload", []string{"basic", "multipart"}, obj(oid, len(content)), obj(smallOID, len(small))))
	require.Equal(t, "multipart", b.Transfer)
	for _, o := range b.Objects {
		up := o.Actions["upload"]
		chunk := up.Header["chunk_size"]
		require.Equal(t, "262144", chunk)
		var parts []string
		for k, v := range up.Header {
			if k != "chunk_size" {
				parts = append(parts, k)
				require.True(t, strings.HasPrefix(v, f.inst.URL+"/alice/tiny.git/info/lfs/uploads/"))
			}
		}
		data := content
		if o.OID == smallOID {
			data = small
		}
		require.Len(t, parts, (len(data)+262143)/262144)
		// The parts appear in part order in the JSON, as the hf agent needs.
		var etags []map[string]any
		for n := 1; n <= len(parts); n++ {
			key := fmt.Sprintf("%05d", n)
			lo, hi := (n-1)*262144, min(n*262144, len(data))
			resp := call(t, http.MethodPut, &lfsAction{Href: up.Header[key]}, data[lo:hi])
			require.Equal(t, http.StatusOK, resp.StatusCode)
			etags = append(etags, map[string]any{"partNumber": n, "etag": resp.Header.Get("ETag")})
		}
		// The completion URL needs no credentials.
		bad, _ := json.Marshal(map[string]any{"oid": strings.Repeat("0", 64), "parts": etags})
		require.Equal(t, http.StatusUnprocessableEntity, call(t, http.MethodPost, &lfsAction{Href: up.Href}, bad).StatusCode)
		done, _ := json.Marshal(map[string]any{"oid": o.OID, "parts": etags})
		require.Equal(t, http.StatusOK, call(t, http.MethodPost, &lfsAction{Href: up.Href}, done).StatusCode)
		require.Equal(t, http.StatusOK, call(t, http.MethodPost, o.Actions["verify"], verifyBody(o.OID, len(data))).StatusCode)
		// Completing again finds no upload.
		require.Equal(t, http.StatusNotFound, call(t, http.MethodPost, &lfsAction{Href: up.Href}, done).StatusCode)
	}
	stored, err := f.repos.LFS.Stat(context.Background(), oid)
	require.NoError(t, err)
	require.True(t, stored.Verified)

	// A client that does not offer multipart gets basic, even for large objects.
	_, b = f.batch(t, publicID, tokAlice, batchReq("upload", []string{"basic"}, obj(strings.Repeat("a", 64), 5<<20)))
	require.Equal(t, "basic", b.Transfer)
}

func TestLFSMultipartCorruptPartsRefused(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	content, oid := randomContent(t, 2<<20)
	_, b := f.batch(t, publicID, tokAlice, batchReq("upload", []string{"multipart"}, obj(oid, len(content))))
	up := b.Objects[0].Actions["upload"]
	var etags []map[string]any
	for n := 1; n <= 8; n++ {
		part := bytes.Clone(content[(n-1)*262144 : n*262144])
		if n == 3 {
			part[0] ^= 1
		}
		resp := call(t, http.MethodPut, &lfsAction{Href: up.Header[fmt.Sprintf("%05d", n)]}, part)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		etags = append(etags, map[string]any{"partNumber": n, "etag": resp.Header.Get("ETag")})
	}
	done, _ := json.Marshal(map[string]any{"oid": oid, "parts": etags})
	require.Equal(t, http.StatusUnprocessableEntity, call(t, http.MethodPost, &lfsAction{Href: up.Href}, done).StatusCode)
	_, err := f.repos.LFS.Stat(context.Background(), oid)
	require.ErrorIs(t, err, repo.ErrNotFound)
}

func TestLFSPresignedStore(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(_ *gitservertest.Options, f *fixture) {
		f.blobs.PresignBaseURL = "https://s3.example.com/bucket"
		f.blobs.MaxPresignPut = 1 << 20
	})
	content, oid := randomContent(t, 1000)
	_, b := f.batch(t, publicID, tokAlice, batchReq("upload", nil, obj(oid, len(content))))
	up := b.Objects[0].Actions["upload"]
	require.True(t, strings.HasPrefix(up.Href, "https://s3.example.com/bucket/lfs/sha256/"))
	require.True(t, b.Objects[0].Authenticated)
	require.True(t, strings.HasPrefix(b.Objects[0].Actions["verify"].Href, f.inst.URL+"/alice/tiny.git/info/lfs/verify?"))

	// Above the store's single PUT limit, uploads stream through Hangar.
	_, b = f.batch(t, publicID, tokAlice, batchReq("upload", nil, obj(strings.Repeat("b", 64), 2<<20)))
	require.True(t, strings.HasPrefix(b.Objects[0].Actions["upload"].Href, f.inst.URL))

	// Multipart parts are presigned.
	_, b = f.batch(t, publicID, tokAlice, batchReq("upload", []string{"multipart"}, obj(strings.Repeat("c", 64), 2<<20)))
	require.True(t, strings.HasPrefix(b.Objects[0].Actions["upload"].Header["00001"], "https://s3.example.com/bucket/"))

	// Simulate the presigned PUT, then verify re-hashes the object.
	require.NoError(t, f.blobs.Put(context.Background(), model.LFSObjectKey(oid), bytes.NewReader(content), int64(len(content))))
	require.Equal(t, http.StatusOK, call(t, http.MethodPost, b2verify(t, f, oid, len(content)), verifyBody(oid, len(content))).StatusCode)

	// Downloads redirect to the store in redirect mode.
	dir := f.clone(t, publicID, tokAlice)
	writeFile(t, dir, "w.bin", model.LFSPointer{OID: oid, Size: int64(len(content))}.Bytes())
	f.git.Must(t, dir, "add", "-A")
	f.git.Must(t, dir, "commit", "-q", "-m", "w")
	f.git.Must(t, dir, "push", "-q", "origin", "main")
	_, b = f.batch(t, publicID, "", batchReq("download", nil, obj(oid, len(content))))
	require.True(t, strings.HasPrefix(b.Objects[0].Actions["download"].Href, "https://s3.example.com/bucket/"))
}

func b2verify(t *testing.T, f *fixture, oid string, size int) *lfsAction {
	t.Helper()
	_, b := f.batch(t, publicID, tokAlice, batchReq("upload", nil, obj(oid, size)))
	return b.Objects[0].Actions["verify"]
}

func TestLFSProxyMode(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(o *gitservertest.Options, f *fixture) {
		f.blobs.PresignBaseURL = "https://s3.example.com/bucket"
		o.Config.DownloadMode = config.DownloadProxy
	})
	content, oid := randomContent(t, 10)
	_, err := f.repos.LFS.Put(context.Background(), content)
	require.NoError(t, err)
	require.NoError(t, f.repos.SetLFSPresence(context.Background(), publicID, []repo.LFSRef{{OID: oid, Size: 10, Present: true}}))
	_, b := f.batch(t, publicID, "", batchReq("download", nil, obj(oid, 10)))
	dl := b.Objects[0].Actions["download"]
	require.True(t, strings.HasPrefix(dl.Href, f.inst.URL))
	require.Equal(t, content, []byte(readAll(t, call(t, http.MethodGet, dl, nil).Body)))
}

type pullThrough struct{ ok bool }

func (p pullThrough) DownloadURL(_ context.Context, _ model.Principal, r *repo.Repo, oid string, _ int64) (string, bool, error) {
	if oid == strings.Repeat("e", 64) {
		return "", false, errors.New("upstream down")
	}
	return "https://hangar.example.com/pull/" + r.ID.Path() + "/" + oid, p.ok, nil
}

func TestLFSPartialMirror(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		pt   gitserver.PullThrough
	}{{"no pull-through", nil}, {"pull-through off for the caller", pullThrough{ok: false}}, {"pull-through", pullThrough{ok: true}}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, func(o *gitservertest.Options, _ *fixture) { o.PullThrough = tc.pt })
			oid := strings.Repeat("d", 64)
			require.NoError(t, f.repos.SetLFSPresence(context.Background(), mirrorID, []repo.LFSRef{{OID: oid, Size: 10, Present: false}}))
			_, b := f.batch(t, mirrorID, "", batchReq("download", nil, obj(oid, 10)))
			o := b.Objects[0]
			if tc.pt != nil && tc.pt.(pullThrough).ok {
				require.Equal(t, "https://hangar.example.com/pull/alice/mirror/"+oid, o.Actions["download"].Href)
				return
			}
			require.Equal(t, http.StatusNotFound, o.Error.Code)
			require.Contains(t, o.Error.Message, "Not in arsenal. Pull this file with: hangar pull alice/mirror --include <path>")
		})
	}
	f := newFixture(t, func(o *gitservertest.Options, _ *fixture) { o.PullThrough = pullThrough{ok: true} })
	oid := strings.Repeat("e", 64)
	require.NoError(t, f.repos.SetLFSPresence(context.Background(), mirrorID, []repo.LFSRef{{OID: oid, Size: 10, Present: false}}))
	resp, _ := f.batch(t, mirrorID, "", batchReq("download", nil, obj(oid, 10)))
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}
