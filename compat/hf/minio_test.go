//go:build compat

package hf

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/compat/harness"
	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/blob/blobtest"
	"github.com/jamesagarside/hangar/internal/blob/s3"
	"github.com/jamesagarside/hangar/internal/model"
)

// minioInstance is the hangar binary on a MinIO Blob store, in its default
// direct upload and redirect download modes, so clients PUT file bytes to
// presigned MinIO URLs as they do on a real deployment.
type minioInstance struct {
	inst   *harness.Instance
	token  string
	blobs  *s3.Store
	prefix string
}

const minioOwner = "hangar-admin"

func startMinIOInstance(t *testing.T) *minioInstance {
	t.Helper()
	srv := blobtest.S3TestServer(t)
	opts := srv.Options()
	opts.Prefix = "hf-compat-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	st, err := s3.New(opts)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	require.NoError(t, st.EnsureBucket(ctx, srv.Region))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = st.List(ctx, "", func(i blob.Info) error { return st.Delete(ctx, i.Key) })
	})

	secret := make([]byte, 16)
	_, err = rand.Read(secret)
	require.NoError(t, err)
	password := hex.EncodeToString(secret)
	inst := harness.Start(t, harness.Options{
		Set: []string{
			"storage.backend=s3", "storage.s3.endpoint=" + srv.Endpoint, "storage.s3.region=" + srv.Region,
			"storage.s3.bucket=" + srv.Bucket, "storage.s3.prefix=" + opts.Prefix, "storage.s3.use_path_style=true",
			"security.bootstrap_admin.username=" + minioOwner,
		},
		Env: []string{
			"HANGAR_STORAGE_S3_ACCESS_KEY_ID=" + srv.AccessKey, "HANGAR_STORAGE_S3_SECRET_ACCESS_KEY=" + srv.SecretKey,
			"HANGAR_SECURITY_BOOTSTRAP_ADMIN_PASSWORD=" + password,
		},
	})
	m := &minioInstance{inst: inst, blobs: st, prefix: opts.Prefix}
	m.token = m.createToken(t, password)
	return m
}

// createToken signs in as the bootstrap admin and creates a write Token,
// the scope huggingface_hub users have.
func (m *minioInstance) createToken(t *testing.T, password string) string {
	t.Helper()
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	c := &http.Client{Jar: jar, Timeout: 30 * time.Second}
	call := func(path string, headers map[string]string, body any, want int, out any) {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		req, err := http.NewRequest(http.MethodPost, m.inst.URL+path, bytes.NewReader(raw))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := c.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		got, _ := io.ReadAll(resp.Body)
		// Never print the body of a successful call: it holds secrets.
		require.Equal(t, want, resp.StatusCode, "POST %s", path)
		require.NoError(t, json.Unmarshal(got, out))
	}
	var me struct {
		CSRFToken string `json:"csrf_token"`
	}
	call("/api/hangar/v1/auth/login", nil, map[string]string{"username": minioOwner, "password": password}, http.StatusOK, &me)
	var tok struct {
		Secret string `json:"secret"`
	}
	call("/api/hangar/v1/me/tokens", map[string]string{"X-CSRF-Token": me.CSRFToken},
		map[string]string{"name": "compat hf minio", "scope": "write"}, http.StatusCreated, &tok)
	require.NotEmpty(t, tok.Secret)
	return tok.Secret
}

// python runs a huggingface_hub script from the compat lock against the
// Instance with Xet turned off, as HF_HUB_DISABLE_XET=1 does, and returns
// its last line of output.
func (m *minioInstance) python(t *testing.T, script string, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("uv"); err != nil {
		t.Fatal("uv is required for the huggingface_hub client")
	}
	cmdArgs := append([]string{"run", "--frozen", "--project", filepath.Join(harness.RepoRoot(), "compat"), "python", "-I", "-c", script}, args...)
	cmd := exec.Command("uv", cmdArgs...)
	cmd.Env = append(os.Environ(), "HF_ENDPOINT="+m.inst.URL, "HF_TOKEN="+m.token, "HF_HOME="+t.TempDir(),
		"HF_HUB_DISABLE_XET=1", "HF_HUB_DISABLE_TELEMETRY=1", "HF_HUB_DISABLE_PROGRESS_BARS=1")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s\nhangar log:\n%s", out, m.inst.Logs())
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	return lines[len(lines)-1]
}

// TestHubBasicUploadToMinIOWithoutXet is the regression test for bellatrix
// finding 1. huggingface_hub uploads an LFS file between the regular file
// limit (10 MiB) and lfs.multipart_threshold (64 MiB) with one PUT to the
// URL the LFS batch API returns, and sends no headers on it. The URL used
// to sign Content-Type and x-amz-checksum-sha256, so MinIO answered
// "AccessDenied: There were headers present in the request which were not
// signed" and every such upload failed when hf_xet was off or absent.
func TestHubBasicUploadToMinIOWithoutXet(t *testing.T) {
	if !blobtest.S3Enabled() {
		t.Logf("MinIO test not registered: set %s=1 (needs Docker) or %s; make test-compat sets it", blobtest.EnvMinIO, blobtest.EnvS3Endpoint)
		return
	}
	m := startMinIOInstance(t)
	dir := t.TempDir()
	weights := make([]byte, 20<<20)
	_, err := rand.Read(weights)
	require.NoError(t, err)
	path := filepath.Join(dir, "model.safetensors")
	require.NoError(t, os.WriteFile(path, weights, 0o600))
	sum := sha256.Sum256(weights)
	oid := hex.EncodeToString(sum[:])

	script := `import hashlib, sys
from huggingface_hub import HfApi, constants, hf_hub_download
assert constants.HF_HUB_DISABLE_XET
api = HfApi()
repo = api.create_repo("` + minioOwner + `/basic-upload", exist_ok=True).repo_id
api.upload_file(path_or_fileobj=sys.argv[1], path_in_repo="model.safetensors", repo_id=repo)
p = hf_hub_download(repo, "model.safetensors")
print(hashlib.sha256(open(p, "rb").read()).hexdigest())`
	require.Equal(t, oid, m.python(t, script, path), "the file downloads with the bytes uploaded")

	ctx := context.Background()
	info, err := m.blobs.Stat(ctx, model.LFSObjectKey(oid))
	require.NoError(t, err, "the object is stored at its key in MinIO")
	require.Equal(t, int64(len(weights)), info.Size)
	var staged []string
	require.NoError(t, m.blobs.List(ctx, blob.PrefixTmp, func(i blob.Info) error { staged = append(staged, i.Key); return nil }))
	require.Empty(t, staged, "the staged upload is removed once it is verified")
	require.NotContains(t, m.inst.Logs(), "AccessDenied")
}
