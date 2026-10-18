package hfapi_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/hfapi/hfapitest"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
)

func newEnv(t *testing.T, opts ...func(*hfapitest.Options)) *hfapitest.Env {
	t.Helper()
	var o hfapitest.Options
	for _, f := range opts {
		f(&o)
	}
	return hfapitest.New(t, o)
}

func modelID(ns, name string) model.RepoID {
	return model.RepoID{Type: model.RepoTypeModel, Namespace: ns, Name: name}
}

func datasetID(ns, name string) model.RepoID {
	return model.RepoID{Type: model.RepoTypeDataset, Namespace: ns, Name: name}
}

// seed creates a Repo owned by user and commits files to main.
func seed(t *testing.T, e *hfapitest.Env, user string, id model.RepoID, vis model.Visibility, files map[string]string) (*repo.Repo, model.CommitSHA) {
	t.Helper()
	ctx := context.Background()
	p := e.Users[user].Principal
	rp, err := e.Repos.Create(ctx, repo.CreateParams{ID: id, Visibility: vis, Actor: p})
	require.NoError(t, err)
	sha, err := e.Repos.Resolve(ctx, id, "main")
	require.NoError(t, err)
	if len(files) > 0 {
		sha = commitFiles(t, e, user, id, "main", files)
	}
	return rp, sha
}

func commitFiles(t *testing.T, e *hfapitest.Env, user string, id model.RepoID, branch string, files map[string]string) model.CommitSHA {
	t.Helper()
	ops := make([]repo.Operation, 0, len(files))
	for p, c := range files {
		if isLFSPath(p) {
			sum := sha256.Sum256([]byte(c))
			oid := hex.EncodeToString(sum[:])
			_, err := e.LFS.Ingest(context.Background(), oid, int64(len(c)), strings.NewReader(c))
			require.NoError(t, err)
			// The user uploaded it, so their Commit may reference it.
			require.NoError(t, e.LFS.RecordUploader(context.Background(), oid, e.Users[user].Principal.UserID))
			ops = append(ops, repo.Operation{Kind: repo.OpAddLFS, Path: p, OID: oid, Size: int64(len(c))})
			continue
		}
		ops = append(ops, repo.Operation{Kind: repo.OpAdd, Path: p, Content: bytes.NewReader([]byte(c))})
	}
	res, err := e.Repos.Commit(context.Background(), repo.CommitParams{Repo: id, Revision: branch, Summary: "Add files", Operations: ops, Actor: e.Users[user].Principal})
	require.NoError(t, err)
	return res.SHA
}

// isLFSPath reports whether tests store a path in LFS, as the default
// .gitattributes would.
func isLFSPath(p string) bool {
	for _, ext := range []string{".safetensors", ".gguf", ".bin", ".parquet"} {
		if strings.HasSuffix(p, ext) {
			return true
		}
	}
	return false
}

// readBody reads the body and puts it back, so it can be read again.
func readBody(t *testing.T, r *http.Response) []byte {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	require.NoError(t, err)
	r.Body = io.NopCloser(bytes.NewReader(b))
	return b
}

func decode[T any](t *testing.T, r *http.Response) T {
	t.Helper()
	var v T
	require.NoError(t, json.Unmarshal(readBody(t, r), &v), "status %d", r.StatusCode)
	return v
}

func requireHubError(t *testing.T, r *http.Response, status int, code string) {
	t.Helper()
	require.Equal(t, status, r.StatusCode, string(readBody(t, r)))
	require.Equal(t, code, r.Header.Get("X-Error-Code"))
	require.NotEmpty(t, r.Header.Get("X-Error-Message"))
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}
