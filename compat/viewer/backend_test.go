//go:build compat

package viewer

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/compat/harness"
	"github.com/jamesagarside/hangar/internal/datasets/viewertest"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/model"
)

// EnvBackends limits the backends the suite runs on to a comma-separated
// list of names, such as "instance-sqlite". Empty runs every backend.
const EnvBackends = "HANGAR_VIEWER_BACKENDS"

// backend is a Hangar the suite converts datasets in and asks for Dataset
// Viewer responses.
type backend interface {
	// Dataset creates a public dataset with files, waits until its
	// conversion and statistics are done, and returns its Repo ID.
	Dataset(t *testing.T, name string, files map[string][]byte) string
	// ViewerURL is the Dataset Viewer root, ending in /datasets-server.
	ViewerURL() string
	// Croissant returns the Croissant document Hangar serves for a dataset.
	Croissant(t *testing.T, id string) []byte
}

type backendKind struct {
	name  string
	start func(t *testing.T) backend
}

// backends lists where the suite runs:
//
//   - fakes: the pipeline and the Viewer over internal/datasets/viewertest,
//     for fast feedback on the Viewer's own code.
//   - instance-sqlite: the hangar binary on SQLite, so cmd/hangar's
//     routing, the Job worker and the real Repo service are covered.
//   - instance-postgres: the hangar binary on Postgres, when dbtest's
//     Postgres tests are on (HANGAR_TEST_POSTGRES=1 or
//     HANGAR_TEST_POSTGRES_DSN).
func backends(t *testing.T) []backendKind {
	all := []backendKind{
		{"fakes", startFakes},
		{"instance-sqlite", func(t *testing.T) backend { return startInstance(t, "") }},
	}
	if dbtest.PostgresEnabled() {
		all = append(all, backendKind{"instance-postgres", func(t *testing.T) backend { return startInstance(t, dbtest.PostgresDSN(t)) }})
	}
	want := os.Getenv(EnvBackends)
	if want == "" {
		return all
	}
	names := strings.Split(want, ",")
	var out []backendKind
	for _, b := range all {
		if slices.Contains(names, b.name) {
			out = append(out, b)
		}
	}
	require.NotEmpty(t, out, "%s=%s names no backend that can run here", EnvBackends, want)
	return out
}

// fakes is the pipeline and the Viewer over fakes.
type fakes struct {
	s   *viewertest.Stack
	ids map[string]model.RepoID
}

func startFakes(t *testing.T) backend {
	return &fakes{s: viewertest.New(t, viewertest.Options{}), ids: map[string]model.RepoID{}}
}

func (f *fakes) Dataset(t *testing.T, name string, files map[string][]byte) string {
	r, sha := f.s.Dataset(t, name, files)
	f.s.Convert(t, r, sha)
	f.ids[r.ID.String()] = r.ID
	return r.ID.String()
}

func (f *fakes) ViewerURL() string { return f.s.URL }

func (f *fakes) Croissant(t *testing.T, id string) []byte {
	doc, err := f.s.Viewer.Croissant(context.Background(), model.Anonymous(), f.ids[id])
	require.NoError(t, err, id)
	return doc
}

// instance is a hangar binary started by compat/harness. Datasets go up
// with `hangar push` as the bootstrap admin; the Viewer is read
// anonymously, as snapshot.py reads the Hub's.
type instance struct {
	inst  *harness.Instance
	token string
}

// instanceUser is the bootstrap admin, who owns the datasets.
const instanceUser = "hangar-admin"

// conversionTimeout bounds the wait for one dataset's conversion and
// statistics.
const conversionTimeout = 10 * time.Minute

func startInstance(t *testing.T, dsn string) backend {
	secret := make([]byte, 16)
	_, err := rand.Read(secret)
	require.NoError(t, err)
	password := hex.EncodeToString(secret)
	set := []string{"security.anonymous_read=true"}
	if dsn != "" {
		set = append(set, "database.dsn="+dsn)
	}
	inst := harness.Start(t, harness.Options{Set: set, StartTimeout: 3 * time.Minute,
		Env: []string{"HANGAR_SECURITY_BOOTSTRAP_ADMIN_PASSWORD=" + password}})
	i := &instance{inst: inst}
	i.token = i.createToken(t, password)
	return i
}

// createToken signs in as the bootstrap admin and creates a write Token.
func (i *instance) createToken(t *testing.T, password string) string {
	login, err := json.Marshal(map[string]string{"username": instanceUser, "password": password})
	require.NoError(t, err)
	res, err := http.Post(i.inst.URL+"/api/hangar/v1/auth/login", "application/json", bytes.NewReader(login))
	require.NoError(t, err)
	body, err := io.ReadAll(res.Body)
	_ = res.Body.Close()
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, res.StatusCode, "sign in: %s\n%s", body, i.inst.Logs())
	var me struct {
		CSRFToken string `json:"csrf_token"`
	}
	require.NoError(t, json.Unmarshal(body, &me))
	req, err := http.NewRequest(http.MethodPost, i.inst.URL+"/api/hangar/v1/me/tokens",
		strings.NewReader(`{"name":"compat-viewer","scope":"write"}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", me.CSRFToken)
	for _, c := range res.Cookies() {
		req.AddCookie(c)
	}
	res, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	body, err = io.ReadAll(res.Body)
	_ = res.Body.Close()
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, res.StatusCode, "create a Token: %s", body)
	var tok struct {
		Secret string `json:"secret"`
	}
	require.NoError(t, json.Unmarshal(body, &tok))
	require.NotEmpty(t, tok.Secret)
	return tok.Secret
}

func (i *instance) ViewerURL() string { return i.inst.URL + "/datasets-server" }

func (i *instance) Dataset(t *testing.T, name string, files map[string][]byte) string {
	dir := t.TempDir()
	for p, b := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
		require.NoError(t, os.WriteFile(full, b, 0o600))
	}
	id := instanceUser + "/" + name
	cmd := exec.Command(harness.Binary(t), "push", dir, "datasets/"+id, "--create", "--json", "--message", "Add the "+name+" fixture")
	cmd.Env = append(withoutHangarEnv(os.Environ()),
		"HANGAR_CLI_URL="+i.inst.URL,
		"HANGAR_CLI_TOKEN="+i.token,
		"HANGAR_CLI_NO_KEYCHAIN=1",
		"HANGAR_CLI_CONFIG="+filepath.Join(t.TempDir(), "cli.yaml"))
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "hangar push %s: %s", id, out)
	i.waitConverted(t, id)
	return id
}

// withoutHangarEnv drops HANGAR_* variables, so the client depends only
// on what the suite sets.
func withoutHangarEnv(env []string) []string {
	var out []string
	for _, kv := range env {
		if !strings.HasPrefix(kv, "HANGAR_") {
			out = append(out, kv)
		}
	}
	return out
}

// waitConverted waits until the Instance's own Jobs have converted the
// dataset and computed the statistics of every converted Split. The Commit
// queues the conversion; nothing here runs a Job.
func (i *instance) waitConverted(t *testing.T, id string) {
	deadline := time.Now().Add(conversionTimeout)
	var ready [][2]string
	for {
		status, body := i.get(t, i.inst.URL+"/api/hangar/v1/repos/datasets/"+id+"/configs", i.token)
		require.Equal(t, http.StatusOK, status, "the Configs of %s: %s", id, body)
		var configs struct {
			Configs []struct {
				Config string `json:"config"`
				Splits []struct {
					Split  string `json:"split"`
					Status string `json:"status"`
				} `json:"splits"`
			} `json:"configs"`
		}
		require.NoError(t, json.Unmarshal(body, &configs))
		pending := false
		ready = ready[:0]
		for _, c := range configs.Configs {
			for _, s := range c.Splits {
				switch s.Status {
				case "pending":
					pending = true
				case "ready", "partial":
					ready = append(ready, [2]string{c.Config, s.Split})
				}
			}
		}
		if !pending {
			break
		}
		require.True(t, time.Now().Before(deadline), "%s was not converted in %s:\n%s", id, conversionTimeout, i.inst.Logs())
		time.Sleep(250 * time.Millisecond)
	}
	for _, cs := range ready {
		q := url.Values{"dataset": {id}, "config": {cs[0]}, "split": {cs[1]}}
		for {
			res, err := http.Get(i.ViewerURL() + "/statistics?" + q.Encode())
			require.NoError(t, err)
			_ = res.Body.Close()
			if res.Header.Get("X-Error-Code") != "ResponseNotReady" {
				break
			}
			require.True(t, time.Now().Before(deadline), "the statistics of %s %s/%s were not computed in %s:\n%s",
				id, cs[0], cs[1], conversionTimeout, i.inst.Logs())
			time.Sleep(250 * time.Millisecond)
		}
	}
}

func (i *instance) get(t *testing.T, u, token string) (int, []byte) {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return res.StatusCode, body
}

// Croissant reads the Hub API's /api/datasets/{id}/croissant anonymously.
func (i *instance) Croissant(t *testing.T, id string) []byte {
	status, body := i.get(t, i.inst.URL+"/api/datasets/"+id+"/croissant", "")
	require.Equal(t, http.StatusOK, status, "the Croissant of %s: %s", id, body)
	return body
}
