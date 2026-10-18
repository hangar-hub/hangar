package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/config"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/org"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/xet"
	"github.com/hangar-hub/hangar/internal/xet/xettest"
)

// TestLFSObjectsOfPrivateReposCannotBeReferenced is the end-to-end test of
// the decision James made on 2026-10-18 ("check at Commit"): LFS objects
// are stored once per Instance, but a Commit may add a pointer to an
// object only when its author uploaded the bytes or can read a Repo that
// holds it.
//
// mallory knows the SHA-256 and size of a file in a private Repo she
// cannot read. The git-lfs batch asks her to upload it, as for an object
// that does not exist; a Hub API commit, a create_pr commit and a git push
// of a pointer to it are refused as for an object nobody uploaded; a Xet
// shard that claims its SHA-256 for other bytes is refused, and none of
// the answers gives its size. carol, a Member who can read the Repo,
// references it without uploading. Once mallory uploads the bytes, through
// git-lfs or Xet, her Commits succeed.
func TestLFSObjectsOfPrivateReposCannotBeReferenced(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("git is required: %v", err)
	}
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	pub := "http://" + srv.Listener.Addr().String()
	in := newTestInstance(t, func(c *config.Config) { c.Server.PublicURL = pub })
	ctx := context.Background()
	h, err := newHandler(ctx, in, newHealth(in.db, in.blobs), false)
	require.NoError(t, err)
	srv.Config.Handler = h
	srv.Start()
	t.Cleanup(srv.Close)

	alice, mallory, carol, xavier := testUser(t, in, "alice"), testUser(t, in, "mallory"), testUser(t, in, "carol"), testUser(t, in, "xavier")
	tokens := map[string]string{}
	for _, p := range []model.Principal{alice, mallory, carol, xavier} {
		_, tok, err := in.auth.TokenStore.Create(ctx, auth.NewToken{UserID: p.UserID, Name: "t", Scope: model.ScopeWrite})
		require.NoError(t, err)
		tokens[p.Username] = tok
	}
	_, err = in.orgs.CreateOrg(ctx, "lab", "Lab", alice)
	require.NoError(t, err)
	require.NoError(t, in.orgs.SetMember(ctx, "lab", carol.UserID, model.RoleRead, org.SourceManual, alice))

	secretID := model.RepoID{Type: model.RepoTypeModel, Namespace: "lab", Name: "secret"}
	_, err = in.repos.Create(ctx, repo.CreateParams{ID: secretID, Actor: alice, InitialReadme: true, Visibility: model.VisibilityPrivate})
	require.NoError(t, err)
	secret := bytes.Repeat([]byte("private weights "), 256) // 4096 bytes
	sum := sha256.Sum256(secret)
	oid, size := hex.EncodeToString(sum[:]), int64(len(secret))
	_, err = in.repos.Commit(ctx, repo.CommitParams{Repo: secretID, Revision: "main", Summary: "Add weights", Actor: alice,
		Operations: []repo.Operation{{Kind: repo.OpAdd, Path: "model.safetensors", Content: bytes.NewReader(secret)}}})
	require.NoError(t, err)
	// Each user's own Repo. carol's is private: once she commits the
	// object to a public Repo, everyone can read it there.
	for _, p := range []model.Principal{mallory, carol, xavier} {
		vis := model.VisibilityPublic
		if p.Username == "carol" {
			vis = model.VisibilityPrivate
		}
		_, err = in.repos.Create(ctx, repo.CreateParams{ID: model.RepoID{Type: model.RepoTypeModel, Namespace: p.Username, Name: "m"},
			Actor: p, InitialReadme: true, Visibility: vis})
		require.NoError(t, err)
	}
	unknownSum := sha256.Sum256([]byte("an object nobody uploaded"))
	unknown := hex.EncodeToString(unknownSum[:])
	const sizeText = "4096 bytes"

	send := func(user, method, url, contentType string, body []byte) (int, http.Header, string) {
		t.Helper()
		req, err := http.NewRequest(method, url, bytes.NewReader(body))
		require.NoError(t, err)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if user != "" {
			setToken(req, user, tokens[user])
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header, string(raw)
	}
	type lfsObj struct {
		OID     string `json:"oid"`
		Actions map[string]struct {
			Href string `json:"href"`
		} `json:"actions"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	batch := func(user string, objs ...[2]any) []lfsObj {
		t.Helper()
		var list []map[string]any
		for _, o := range objs {
			list = append(list, map[string]any{"oid": o[0], "size": o[1]})
		}
		body, _ := json.Marshal(map[string]any{"operation": "upload", "transfers": []string{"basic"}, "objects": list})
		code, _, raw := send(user, http.MethodPost, pub+"/"+user+"/m.git/info/lfs/objects/batch", "application/vnd.git-lfs+json", body)
		require.Equal(t, http.StatusOK, code, raw)
		require.NotContains(t, raw, sizeText)
		var out struct {
			Objects []lfsObj `json:"objects"`
		}
		require.NoError(t, json.Unmarshal([]byte(raw), &out), raw)
		return out.Objects
	}
	hubCommit := func(user, query string, line map[string]any) (int, string) {
		t.Helper()
		var b bytes.Buffer
		for _, l := range []map[string]any{{"key": "header", "value": map[string]any{"summary": "Add", "description": ""}}, line} {
			enc, _ := json.Marshal(l)
			b.Write(enc)
			b.WriteByte('\n')
		}
		code, hdr, raw := send(user, http.MethodPost, pub+"/api/models/"+user+"/m/commit/main"+query, "application/x-ndjson", b.Bytes())
		return code, hdr.Get("X-Error-Message") + raw
	}
	lfsLine := func(o string, sz int64) map[string]any {
		v := map[string]any{"path": "w.safetensors", "algo": "sha256", "oid": o}
		if sz >= 0 {
			v["size"] = sz
		}
		return map[string]any{"key": "lfsFile", "value": v}
	}

	t.Run("batch reveals nothing", func(t *testing.T) {
		for _, sz := range []int64{size, size + 1, 1} {
			objs := batch("mallory", [2]any{oid, sz}, [2]any{unknown, sz})
			require.Len(t, objs, 2)
			for _, o := range objs {
				require.Nil(t, o.Error, "size %d", sz)
				require.Contains(t, o.Actions, "upload", "size %d: upload it, as an object that does not exist", sz)
			}
		}
		objs := batch("carol", [2]any{oid, size})
		require.Empty(t, objs[0].Actions, "carol can read the Repo that holds it: nothing to upload")
	})

	t.Run("Hub API commits are refused", func(t *testing.T) {
		for _, query := range []string{"", "?create_pr=1"} {
			for _, sz := range []int64{size, -1, size + 1} {
				code, msg := hubCommit("mallory", query, lfsLine(oid, sz))
				require.Equal(t, http.StatusBadRequest, code, msg)
				ucode, umsg := hubCommit("mallory", query, lfsLine(unknown, sz))
				require.Equal(t, ucode, code)
				require.Equal(t, strings.ReplaceAll(umsg, unknown, "<oid>"), strings.ReplaceAll(msg, oid, "<oid>"),
					"the same answer as for an unknown object (%q, size %d)", query, sz)
				require.NotContains(t, msg, sizeText)
			}
		}
		code, _, raw := send("mallory", http.MethodGet, pub+"/api/models/mallory/m/discussions", "", nil)
		require.Equal(t, http.StatusOK, code, raw)
		require.Contains(t, raw, `"count":0`, "no pull request was opened")
	})

	// git with a throwaway home, so no global hooks or credentials apply.
	home := t.TempDir()
	runGit := func(dir string, args ...string) (string, error) {
		cmd := exec.Command(gitBin, args...) //nolint:gosec // the test's own git commands.
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "HOME="+home, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	pushPointer := func(user string) (string, error) {
		t.Helper()
		dir := filepath.Join(t.TempDir(), "w")
		remote := strings.Replace(pub, "http://", "http://"+user+":"+tokens[user]+"@", 1) + "/" + user + "/m.git"
		out, err := runGit("", "clone", "-q", remote, dir)
		require.NoError(t, err, out)
		ptr := model.LFSPointer{OID: oid, Size: size}.Bytes()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "pointer.safetensors"), ptr, 0o600))
		for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", "pointer"}} {
			out, err := runGit(dir, args...)
			require.NoError(t, err, out)
		}
		return runGit(dir, "push", "--no-verify", "origin", "main")
	}

	t.Run("git push is refused", func(t *testing.T) {
		out, err := pushPointer("mallory")
		require.Error(t, err, out)
		require.Contains(t, out, "the push refers to LFS object "+oid+" (4096 bytes), which has not been uploaded")
	})

	t.Run("Xet cannot claim the object", func(t *testing.T) {
		code, hdr, raw := send("mallory", http.MethodGet, pub+"/api/models/mallory/m/xet-write-token/main", "", nil)
		require.Equal(t, http.StatusOK, code, raw)
		casURL, casToken := hdr.Get("X-Xet-Cas-Url"), hdr.Get("X-Xet-Access-Token")
		// mallory's own bytes of the same size, with the secret's SHA-256.
		u := xettest.BuildUpload([][]byte{bytes.Repeat([]byte("m"), len(secret))}, xettest.UploadOptions{ChunkSize: 1024})
		claim, err := xet.ParseHash(oid)
		require.NoError(t, err)
		u.Shard.Files[0].SHA256 = &claim
		cas := func(method, url string, body []byte) (int, string) {
			req, err := http.NewRequest(method, url, bytes.NewReader(body))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+casToken)
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)
			return resp.StatusCode, resp.Header.Get("X-Error-Message") + string(raw)
		}
		for _, x := range u.Xorbs {
			code, msg := cas(http.MethodPost, casURL+"/v1/xorbs/default/"+x.Hash.String(), x.Body)
			require.Equal(t, http.StatusOK, code, msg)
		}
		code, msg := cas(http.MethodPost, casURL+"/v1/shards", u.ShardBytes())
		require.Equal(t, http.StatusBadRequest, code, msg)
		require.NotContains(t, msg, sizeText)
		code, msg = hubCommit("mallory", "", lfsLine(oid, size))
		require.Equal(t, http.StatusBadRequest, code, "the refused shard proves nothing: %s", msg)
	})

	t.Run("a reader references it without uploading", func(t *testing.T) {
		code, msg := hubCommit("carol", "", lfsLine(oid, size))
		require.Equal(t, http.StatusOK, code, msg)
		code, _, raw := send("carol", http.MethodGet, pub+"/carol/m/resolve/main/w.safetensors", "", nil)
		require.Equal(t, http.StatusOK, code)
		require.Equal(t, string(secret), raw)
	})

	t.Run("uploading the bytes through git-lfs succeeds", func(t *testing.T) {
		objs := batch("mallory", [2]any{oid, size})
		up, verify := objs[0].Actions["upload"].Href, objs[0].Actions["verify"].Href
		require.NotEmpty(t, up)
		code, _, raw := send("", http.MethodPut, up, "", secret)
		require.Equal(t, http.StatusOK, code, raw)
		body, _ := json.Marshal(map[string]any{"oid": oid, "size": size})
		code, _, raw = send("mallory", http.MethodPost, verify, "application/vnd.git-lfs+json", body)
		require.Equal(t, http.StatusOK, code, raw)
		require.Empty(t, batch("mallory", [2]any{oid, size})[0].Actions, "mallory uploaded it, so it is deduplicated for her now")
		code, msg := hubCommit("mallory", "", lfsLine(oid, size))
		require.Equal(t, http.StatusOK, code, msg)
		out, err := pushPointer("mallory")
		require.NoError(t, err, out)
	})

	t.Run("uploading the bytes through Xet succeeds", func(t *testing.T) {
		code, hdr, raw := send("xavier", http.MethodGet, pub+"/api/models/xavier/m/xet-write-token/main", "", nil)
		require.Equal(t, http.StatusOK, code, raw)
		casURL, casToken := hdr.Get("X-Xet-Cas-Url"), hdr.Get("X-Xet-Access-Token")
		u := xettest.BuildUpload([][]byte{secret}, xettest.UploadOptions{ChunkSize: 1024})
		for _, x := range u.Xorbs {
			req, err := http.NewRequest(http.MethodPost, casURL+"/v1/xorbs/default/"+x.Hash.String(), bytes.NewReader(x.Body))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+casToken)
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			_ = resp.Body.Close()
		}
		req, err := http.NewRequest(http.MethodPost, casURL+"/v1/shards", bytes.NewReader(u.ShardBytes()))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+casToken)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		shardBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode, string(shardBody))
		code, msg := hubCommit("xavier", "", lfsLine(oid, size))
		require.Equal(t, http.StatusOK, code, msg)
		code, _, raw = send("xavier", http.MethodGet, pub+"/xavier/m/resolve/main/w.safetensors", "", nil)
		require.Equal(t, http.StatusOK, code)
		require.Equal(t, strconv.Itoa(len(secret)), strconv.Itoa(len(raw)))
	})
}
