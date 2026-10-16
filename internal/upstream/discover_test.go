package upstream_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/config"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/jobs"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/upstream"
	"github.com/jamesagarside/hangar/internal/upstream/deps"
	"github.com/jamesagarside/hangar/internal/upstream/upstreamtest"
)

func TestDependenciesPullRecursivelyWithCycles(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d, upstreamtest.HarnessOptions{Deps: config.DependencyConfig{AutoPullCode: true, MaxDepth: 5}})
		alice := e.User("alice")
		jina := model.RepoID{Type: model.RepoTypeModel, Namespace: "jinaai", Name: "jina-embeddings-v3"}
		impl := model.RepoID{Type: model.RepoTypeModel, Namespace: "jinaai", Name: "xlm-roberta-flash-implementation"}
		// The Jina form: the model's auto_map names classes in another Repo,
		// which in turn names the model (a cycle).
		e.Seed(jina, map[string][]byte{"config.json": []byte(`{"auto_map":{
			"AutoConfig":"jinaai/xlm-roberta-flash-implementation--configuration_xlm_roberta.XLMRobertaFlashConfig",
			"AutoModel":"jinaai/xlm-roberta-flash-implementation--modeling_lora.XLMRobertaLoRA"}}`),
			"README.md": []byte("---\nbase_model: FacebookAI/xlm-roberta-large\n---\n")}, map[string][]byte{"model.safetensors": []byte("jina weights")})
		e.Seed(impl, map[string][]byte{"modeling_lora.py": []byte("class XLMRobertaLoRA: pass\n"),
			"config.json": []byte(`{"auto_map":{"AutoModel":"jinaai/jina-embeddings-v3--modeling.X"}}`)}, map[string][]byte{"blob.bin": []byte("impl blob")})

		id, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: jina, Actor: alice})
		require.NoError(t, err)
		e.Run(20)
		j := e.Job(id)
		require.Equal(t, jobs.StatusSucceeded, j.Status, j.Error)
		st := pullState(t, j)
		require.Len(t, st["dependencies"], 1)
		dep := st["dependencies"].([]any)[0].(map[string]any)
		require.Equal(t, "jinaai/xlm-roberta-flash-implementation", dep["repo"])
		require.Equal(t, "done", dep["status"])

		r, err := e.Repos.Get(e.Ctx, impl)
		require.NoError(t, err, "the code Dependency was pulled")
		head, _ := e.Repos.Resolve(e.Ctx, impl, "")
		require.Equal(t, []byte("impl blob"), readFile(t, e.Repos, impl, head, "blob.bin"), "code Dependencies are pulled with all their files")
		require.True(t, r.Mirror)

		jr, _ := e.Repos.Get(e.Ctx, jina)
		g, err := e.Service.Graph(e.Ctx, alice, jr)
		require.NoError(t, err)
		kinds := map[deps.Kind]upstream.ArsenalState{}
		for _, n := range g.Needs {
			kinds[n.Kind] = n.Arsenal
		}
		require.Equal(t, upstream.ArsenalFull, kinds[deps.KindCode])
		require.Equal(t, upstream.ArsenalNone, kinds[deps.KindCardBaseModel], "base models are listed, not pulled")
		gi, err := e.Service.Graph(e.Ctx, alice, r)
		require.NoError(t, err)
		require.Len(t, gi.NeededBy, 1)
		require.Equal(t, jina, gi.NeededBy[0].Repo)
		require.Len(t, gi.Needs, 1, "the implementation needs the model back")

		// Only two pull Jobs ran: the cycle back to the model stopped.
		pulls, _, err := e.Queue.List(e.Ctx, jobs.Filter{Kinds: []jobs.Kind{jobs.KindPull}})
		require.NoError(t, err)
		require.Len(t, pulls, 2)
	})
}

func TestDependencyDepthLimitAndAutoPullOff(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d, upstreamtest.HarnessOptions{Deps: config.DependencyConfig{AutoPullCode: true, MaxDepth: 1}})
		alice := e.User("alice")
		a := model.RepoID{Type: model.RepoTypeModel, Namespace: "chain", Name: "a"}
		b := model.RepoID{Type: model.RepoTypeModel, Namespace: "chain", Name: "b"}
		c := model.RepoID{Type: model.RepoTypeModel, Namespace: "chain", Name: "c"}
		e.Seed(a, map[string][]byte{"config.json": []byte(`{"auto_map":{"AutoModel":"chain/b--m.M"}}`)}, nil)
		e.Seed(b, map[string][]byte{"config.json": []byte(`{"auto_map":{"AutoModel":"chain/c--m.M"}}`)}, nil)
		e.Seed(c, map[string][]byte{"m.py": []byte("x")}, nil)
		id, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: a, Actor: alice})
		require.NoError(t, err)
		e.Run(20)
		require.Equal(t, jobs.StatusSucceeded, e.Job(id).Status, e.Job(id).Error)
		_, err = e.Repos.Get(e.Ctx, b)
		require.NoError(t, err)
		_, err = e.Repos.Get(e.Ctx, c)
		require.ErrorIs(t, err, repo.ErrNotFound, "max_depth 1 stops after one level")

		// With auto_pull_code off, code Dependencies are listed, not pulled.
		e2 := newEnv(t, d, upstreamtest.HarnessOptions{Deps: config.DependencyConfig{AutoPullCode: false, MaxDepth: 5}})
		solo := model.RepoID{Type: model.RepoTypeModel, Namespace: "solo", Name: "a"}
		e2.Seed(solo, map[string][]byte{"config.json": []byte(`{"auto_map":{"AutoModel":"solo/b--m.M"}}`)}, nil)
		id, _, err = e2.Service.StartPull(e2.Ctx, upstream.PullRequest{Repo: solo, Actor: e2.User("carol")})
		require.NoError(t, err)
		e2.Run(10)
		require.Equal(t, jobs.StatusSucceeded, e2.Job(id).Status, e2.Job(id).Error)
		pulls, _, err := e2.Queue.List(e2.Ctx, jobs.Filter{Kinds: []jobs.Kind{jobs.KindPull}})
		require.NoError(t, err)
		require.Len(t, pulls, 1)
		sr, _ := e2.Repos.Get(e2.Ctx, solo)
		g, err := e2.Service.Graph(e2.Ctx, model.System(), sr)
		require.NoError(t, err)
		require.Len(t, g.Needs, 1)
		require.Equal(t, upstream.ArsenalNone, g.Needs[0].Arsenal)
	})
}

func TestResolveDependenciesOfUploadedRepos(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d, upstreamtest.HarnessOptions{})
		alice := e.User("alice")
		impl := model.RepoID{Type: model.RepoTypeModel, Namespace: "jinaai", Name: "xlm-roberta-flash-implementation"}
		e.Seed(impl, map[string][]byte{"modeling_lora.py": []byte("x")}, nil)
		mine := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "finetune"}
		r, err := e.Repos.Create(e.Ctx, repo.CreateParams{ID: mine, Actor: alice})
		require.NoError(t, err)
		res, err := e.Repos.Commit(e.Ctx, repo.CommitParams{Repo: mine, Summary: "c", Actor: alice, Operations: []repo.Operation{
			{Kind: repo.OpAdd, Path: "config.json", Content: strings.NewReader(`{"auto_map":{"AutoModel":"jinaai/xlm-roberta-flash-implementation--modeling_lora.L"}}`)}}})
		require.NoError(t, err)
		warnings, err := e.Service.ResolveDependencies(e.Ctx, r.UUID, res.SHA)
		require.NoError(t, err)
		require.Empty(t, warnings)
		e.Run(10)
		_, err = e.Repos.Get(e.Ctx, impl)
		require.NoError(t, err, "a missing code Dependency of an uploaded Repo is pulled from the default Upstream")
		g, err := e.Service.Graph(e.Ctx, alice, r)
		require.NoError(t, err)
		require.Len(t, g.Needs, 1)
		require.Equal(t, upstream.ArsenalFull, g.Needs[0].Arsenal)
	})
}

func TestDiscoverMarksArsenalState(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d, upstreamtest.HarnessOptions{})
		alice := e.User("alice")
		full := model.RepoID{Type: model.RepoTypeModel, Namespace: "acme", Name: "full"}
		part := model.RepoID{Type: model.RepoTypeModel, Namespace: "acme", Name: "part-GGUF"}
		none := model.RepoID{Type: model.RepoTypeModel, Namespace: "acme", Name: "none"}
		e.Seed(full, nil, map[string][]byte{"m.bin": []byte("full")})
		e.Seed(part, nil, map[string][]byte{"m-Q4_0.gguf": []byte("q4"), "m-Q8_0.gguf": []byte("q8")})
		e.Seed(none, nil, map[string][]byte{"m.bin": []byte("none")})
		for _, req := range []upstream.PullRequest{{Repo: full, Actor: alice}, {Repo: part, Actor: alice, Selection: upstream.Selection{Quants: []string{"Q4_0"}}}} {
			_, _, err := e.Service.StartPull(e.Ctx, req)
			require.NoError(t, err)
		}
		e.Run(10)
		newer := e.Seed(full, map[string][]byte{"new.txt": []byte("new")}, nil)

		page, err := e.Service.Discover(e.Ctx, alice, "", upstream.SearchQuery{Type: model.RepoTypeModel, Search: "acme", Limit: 10})
		require.NoError(t, err)
		require.Equal(t, "huggingface", page.Upstream.Name)
		states := map[string]upstream.DiscoverItem{}
		for _, it := range page.Items {
			states[it.ID.String()] = it
		}
		require.Equal(t, upstream.ArsenalFull, states["acme/full"].Arsenal)
		require.NotEmpty(t, states["acme/full"].LocalSHA, "update available: the Upstream moved on")
		require.NotEqual(t, newer, states["acme/full"].LocalSHA)
		require.Equal(t, upstream.ArsenalPartial, states["acme/part-GGUF"].Arsenal)
		require.Equal(t, upstream.ArsenalNone, states["acme/none"].Arsenal)

		// Results are cached per query and token for five minutes.
		searches := e.Server.Requests("search")
		_, err = e.Service.Discover(e.Ctx, alice, "", upstream.SearchQuery{Type: model.RepoTypeModel, Search: "acme", Limit: 10})
		require.NoError(t, err)
		require.Equal(t, searches, e.Server.Requests("search"))
		e.SetToken(alice.UserID, "another-token")
		_, err = e.Service.Discover(e.Ctx, alice, "", upstream.SearchQuery{Type: model.RepoTypeModel, Search: "acme", Limit: 10})
		require.NoError(t, err)
		require.Equal(t, searches+1, e.Server.Requests("search"), "another token never shares cached results")
		e.Advance(6 * time.Minute)
		_, err = e.Service.Discover(e.Ctx, alice, "", upstream.SearchQuery{Type: model.RepoTypeModel, Search: "acme", Limit: 10})
		require.NoError(t, err)
		require.Equal(t, searches+2, e.Server.Requests("search"), "the cache expires")

		// A Mirror the caller cannot read is "none" to them.
		r, _ := e.Repos.Get(e.Ctx, full)
		vis := model.VisibilityPrivate
		_, err = e.Repos.UpdateSettings(e.Ctx, full, repo.SettingsUpdate{Visibility: &vis}, model.System())
		require.NoError(t, err)
		_ = r
		page, err = e.Service.Discover(e.Ctx, e.User("bob"), "", upstream.SearchQuery{Type: model.RepoTypeModel, Search: "acme", Limit: 10})
		require.NoError(t, err)
		for _, it := range page.Items {
			if it.ID == full {
				require.Equal(t, upstream.ArsenalNone, it.Arsenal)
			}
		}
	})
}

func TestDiscoverDetail(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d, upstreamtest.HarnessOptions{})
		alice := e.User("alice")
		gguf := model.RepoID{Type: model.RepoTypeModel, Namespace: "acme", Name: "detail-GGUF"}
		e.Seed(gguf, map[string][]byte{"README.md": []byte("---\nbase_model: acme/base\n---\n# Detail"),
			"config.json": []byte(`{"auto_map":{"AutoModel":"acme/code--m.M"}}`)},
			map[string][]byte{"m-Q4_K_M.gguf": []byte("q4"), "m-Q8_0.gguf": []byte("q8 bigger"), "mmproj-m.gguf": []byte("proj")})
		_, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: gguf, Actor: alice, Selection: upstream.Selection{Quants: []string{"Q4_K_M"}}})
		require.NoError(t, err)
		e.Run(10)
		det, err := e.Service.DiscoverDetail(e.Ctx, alice, "huggingface", gguf, "")
		require.NoError(t, err)
		require.Equal(t, upstream.ArsenalPartial, det.Arsenal)
		require.Contains(t, det.Card, "# Detail")
		require.Len(t, det.Quants, 2)
		for _, q := range det.Quants {
			require.Equal(t, []string{"mmproj-m.gguf"}, q.Projectors)
			require.Equal(t, q.Name == "Q4_K_M", q.Present, q.Name)
		}
		var total int64
		for _, f := range det.Files {
			total += f.Size
			if f.Path == "m-Q8_0.gguf" {
				require.False(t, f.Present)
			}
			if f.Path == "m-Q4_K_M.gguf" {
				require.True(t, f.Present)
			}
		}
		require.Equal(t, total, det.TotalSize)
		require.Len(t, det.Dependencies, 2)
		require.Equal(t, upstream.ArsenalNone, det.Dependencies[0].Arsenal)

		// Datasets show Configs, Splits and a preview from the Upstream's Viewer.
		ds := model.RepoID{Type: model.RepoTypeDataset, Namespace: "acme", Name: "data"}
		e.Seed(ds, map[string][]byte{"data.csv": []byte("a,b\n1,2\n")}, nil)
		q := url.Values{"dataset": {"acme/data"}}
		e.Remote.SetViewerResponse("/splits", q, `{"splits":[{"dataset":"acme/data","config":"default","split":"train"},{"dataset":"acme/data","config":"default","split":"test"}]}`)
		e.Remote.SetViewerResponse("/size", q, `{"size":{"splits":[{"config":"default","split":"train","num_rows":10,"num_bytes_parquet_files":100}]}}`)
		e.Remote.SetViewerResponse("/first-rows", url.Values{"dataset": {"acme/data"}, "config": {"default"}, "split": {"train"}}, `{"rows":[{"row_idx":0,"row":{"a":1}}]}`)
		det, err = e.Service.DiscoverDetail(e.Ctx, alice, "", ds, "")
		require.NoError(t, err)
		require.Len(t, det.Configs, 1)
		require.Len(t, det.Configs[0].Splits, 2)
		require.EqualValues(t, 10, *det.Configs[0].Splits[0].NumRows)
		require.Nil(t, det.Configs[0].Splits[1].NumRows)
		require.Contains(t, string(det.Preview), `"row_idx":0`)
	})
}

func TestPullThroughRepoAndFile(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d, upstreamtest.HarnessOptions{})
		alice := e.User("alice")
		w := bytes.Repeat([]byte("stream me "), 2000)
		commit := e.Seed(tiny, map[string][]byte{"config.json": []byte(`{}`)}, map[string][]byte{"model.safetensors": w})
		pt := e.Service.PullThrough()

		// Off by default.
		_, err := pt.Repo(e.Ctx, alice, tiny)
		require.ErrorIs(t, err, repo.ErrNotFound)

		e.SetSettings(upstream.Settings{PullThroughEnabled: true, PullThroughAllow: []string{"acme/*"}, PullThroughMinRole: "user"})
		_, err = pt.Repo(e.Ctx, model.Anonymous(), tiny)
		require.ErrorIs(t, err, repo.ErrNotFound, "anonymous callers need min_role anonymous")
		other := model.RepoID{Type: model.RepoTypeModel, Namespace: "other", Name: "x"}
		_, err = pt.Repo(e.Ctx, alice, other)
		require.ErrorIs(t, err, repo.ErrNotFound, "not in the allowlist")

		// The worker runs in the background, as in a real Instance. It
		// stops before the test's servers close, even when the test fails.
		ctx, cancel := context.WithCancel(e.Ctx)
		var wg sync.WaitGroup
		defer wg.Wait()
		defer cancel()
		wg.Go(func() {
			for ctx.Err() == nil {
				e.Queue.RunPending(ctx)
				time.Sleep(20 * time.Millisecond)
			}
		})
		r, err := pt.Repo(e.Ctx, alice, tiny)
		require.NoError(t, err)
		require.True(t, r.Mirror)
		head, _ := e.Repos.Resolve(e.Ctx, tiny, "main")
		require.Equal(t, commit, head)
		en, err := e.Repos.Entry(e.Ctx, tiny, commit, "model.safetensors")
		require.NoError(t, err)
		require.False(t, en.Present, "a Pull-through fetches history only")
		missing := model.RepoID{Type: model.RepoTypeModel, Namespace: "acme", Name: "missing"}
		_, err = pt.Repo(e.Ctx, alice, missing)
		require.ErrorIs(t, err, repo.ErrNotFound)

		// Two concurrent requests share one transfer and both get the bytes.
		e.Server.Hold()
		cdn := e.Server.Requests("cdn")
		outs := make([][]byte, 2)
		errs := make([]error, 2)
		var rw sync.WaitGroup
		for i := range 2 {
			rw.Go(func() {
				rc, err := pt.File(e.Ctx, alice, r, commit, en)
				if err != nil {
					errs[i] = err
					return
				}
				defer rc.Close()
				outs[i], errs[i] = io.ReadAll(rc)
			})
			time.Sleep(50 * time.Millisecond)
		}
		time.Sleep(100 * time.Millisecond)
		e.Server.Release()
		rw.Wait()
		for i := range 2 {
			require.NoError(t, errs[i])
			require.Equal(t, w, outs[i])
		}
		require.Equal(t, cdn+1, e.Server.Requests("cdn"), "one upstream transfer for both readers")
		en, _ = e.Repos.Entry(e.Ctx, tiny, commit, "model.safetensors")
		require.True(t, en.Present)
		rc, err := pt.File(e.Ctx, alice, r, commit, en)
		require.NoError(t, err)
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		require.Equal(t, w, b, "a present file streams from the Blob store")
		cancel()
		wg.Wait()
		m, _, err := e.Service.Mirror(e.Ctx, r)
		require.NoError(t, err)
		require.Contains(t, m.Selection.Paths, "model.safetensors", "the Mirror now tracks the file it holds")
	})
}

// TestPullThroughPendingWhilePlanning: a Pull-through that has not
// finished planning within PullThroughWait answers ErrPullPending (the Hub
// API's 503 with Retry-After); a retry joins the same Pull, and once it has
// run the Repo is there.
func TestPullThroughPendingWhilePlanning(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d, upstreamtest.HarnessOptions{PullThroughWait: 100 * time.Millisecond})
		alice := e.User("alice")
		e.Seed(tiny, map[string][]byte{"config.json": []byte(`{}`)}, nil)
		e.SetSettings(upstream.Settings{PullThroughEnabled: true, PullThroughAllow: []string{"acme/*"}, PullThroughMinRole: "user"})
		pt := e.Service.PullThrough()
		pulls := func() []*jobs.Job {
			js, _, err := e.Queue.List(e.Ctx, jobs.Filter{Kinds: []jobs.Kind{jobs.KindPull}})
			require.NoError(t, err)
			return js
		}

		// No worker runs, so planning cannot finish within the wait.
		_, err := pt.Repo(e.Ctx, alice, tiny)
		require.ErrorIs(t, err, upstream.ErrPullPending)
		_, err = pt.Repo(e.Ctx, alice, tiny)
		require.ErrorIs(t, err, upstream.ErrPullPending)
		js := pulls()
		require.Len(t, js, 1, "the retry joins the Pull in progress")
		require.Equal(t, jobs.StatusPending, js[0].Status)

		e.Run(10)
		require.Equal(t, jobs.StatusSucceeded, e.Job(js[0].ID).Status, e.Job(js[0].ID).Error)
		r, err := e.Repos.Get(e.Ctx, tiny)
		require.NoError(t, err)
		require.True(t, r.Mirror)
	})
}

// TestPullThroughWaitsForAMirrorWithoutHistory holds planning after it
// has created the Mirror and before it stores the history. Repo for the
// Mirror joins that Pull and waits as for a missing Repo; a Mirror with
// history, or one Pull-through does not apply to, comes back as it is.
func TestPullThroughWaitsForAMirrorWithoutHistory(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d, upstreamtest.HarnessOptions{PullThroughWait: 100 * time.Millisecond})
		alice := e.User("alice")
		commit := e.Seed(tiny, map[string][]byte{"config.json": []byte(`{}`)}, nil)
		e.SetSettings(upstream.Settings{PullThroughEnabled: true, PullThroughAllow: []string{"acme/*"}, PullThroughMinRole: "user"})
		pt := e.Service.PullThrough()
		pulls := func() []*jobs.Job {
			js, _, err := e.Queue.List(e.Ctx, jobs.Filter{Kinds: []jobs.Kind{jobs.KindPull}})
			require.NoError(t, err)
			return js
		}
		_, err := pt.Repo(e.Ctx, alice, tiny)
		require.ErrorIs(t, err, upstream.ErrPullPending)

		held, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		e.Repos.BeforeApplyPack(func(ctx context.Context, p repo.ApplyPackParams) error {
			if p.Pack != nil {
				once.Do(func() { close(held) })
				<-release
			}
			return nil
		})
		planned := make(chan struct{})
		go func() {
			defer close(planned)
			e.Queue.RunPending(e.Ctx)
		}()
		<-held
		r, err := e.Repos.Get(e.Ctx, tiny)
		require.NoError(t, err)
		require.True(t, r.Mirror, "planning created the Mirror")

		_, err = pt.Repo(e.Ctx, alice, tiny)
		require.ErrorIs(t, err, upstream.ErrPullPending, "the Mirror has no history yet")
		require.Len(t, pulls(), 1, "Repo joined the Pull that is planning")
		// Outside the allowlist, Repo returns the Mirror as it is.
		e.SetSettings(upstream.Settings{PullThroughEnabled: true, PullThroughAllow: []string{"other/*"}, PullThroughMinRole: "user"})
		got, err := pt.Repo(e.Ctx, alice, tiny)
		require.NoError(t, err)
		require.Equal(t, r.UUID, got.UUID)
		e.SetSettings(upstream.Settings{PullThroughEnabled: true, PullThroughAllow: []string{"acme/*"}, PullThroughMinRole: "user"})

		close(release)
		<-planned
		got, err = pt.Repo(e.Ctx, alice, tiny)
		require.NoError(t, err)
		require.Equal(t, r.UUID, got.UUID)
		head, err := e.Repos.Resolve(e.Ctx, tiny, "main")
		require.NoError(t, err)
		require.Equal(t, commit, head)
		e.Run(10)
		n := len(pulls())
		_, err = pt.Repo(e.Ctx, alice, tiny)
		require.NoError(t, err)
		require.Len(t, pulls(), n, "a Mirror with history starts no Pull")
	})
}

func TestPullThroughGitLFSURL(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d, upstreamtest.HarnessOptions{})
		alice := e.User("alice")
		w := []byte("lfs object for git")
		e.Seed(tiny, nil, map[string][]byte{"model.bin": w})
		id, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: tiny, Actor: alice, Selection: upstream.Selection{None: true}})
		require.NoError(t, err)
		e.Run(5)
		require.Equal(t, jobs.StatusSucceeded, e.Job(id).Status, e.Job(id).Error)
		r, _ := e.Repos.Get(e.Ctx, tiny)
		pt := e.Service.PullThrough()
		_, ok, err := pt.DownloadURL(e.Ctx, alice, r, sha(w), int64(len(w)))
		require.NoError(t, err)
		require.False(t, ok, "Pull-through is off")
		e.SetSettings(upstream.Settings{PullThroughEnabled: true, PullThroughAllow: []string{"*"}, PullThroughMinRole: "user"})
		href, ok, err := pt.DownloadURL(e.Ctx, alice, r, sha(w), int64(len(w)))
		require.NoError(t, err)
		require.True(t, ok)
		require.True(t, strings.HasPrefix(href, "http://hangar.test"+upstream.PullThroughPath+r.UUID.String()+"/"+sha(w)))

		srv := httptest.NewServer(pt.Handler())
		defer srv.Close()
		u, _ := url.Parse(href)
		resp, err := http.Get(srv.URL + u.RequestURI())
		require.NoError(t, err)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
		require.Equal(t, w, body)

		tampered := strings.Replace(u.RequestURI(), sha(w), strings.Repeat("0", 64), 1)
		resp, err = http.Get(srv.URL + tampered)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
		resp, err = http.Get(srv.URL + upstream.PullThroughPath + uuid.NewString() + "/" + sha(w))
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
	})
}

// TestPullThroughPullFiles covers the Ollama registry's entry point: it
// queues a Pull of the missing files for the request's Principal.
func TestPullThroughPullFiles(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d, upstreamtest.HarnessOptions{})
		alice := e.User("alice")
		w := []byte("gguf weights for ollama")
		commit := e.Seed(tiny, nil, map[string][]byte{"model-Q4_K_M.gguf": w})
		id, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: tiny, Actor: alice, Selection: upstream.Selection{None: true}})
		require.NoError(t, err)
		e.Run(5)
		require.Equal(t, jobs.StatusSucceeded, e.Job(id).Status, e.Job(id).Error)
		r, err := e.Repos.Get(e.Ctx, tiny)
		require.NoError(t, err)
		pt := e.Service.PullThrough()
		ctx := httpx.WithInfo(e.Ctx, "req")
		httpx.SetPrincipal(ctx, alice)

		require.ErrorIs(t, pt.PullFiles(ctx, r, commit, []string{"model-Q4_K_M.gguf"}), repo.ErrNotPresent, "Pull-through is off")
		e.SetSettings(upstream.Settings{PullThroughEnabled: true, PullThroughAllow: []string{"other/*"}, PullThroughMinRole: "user"})
		require.ErrorIs(t, pt.PullFiles(ctx, r, commit, []string{"model-Q4_K_M.gguf"}), repo.ErrNotPresent, "not in the allowlist")
		e.SetSettings(upstream.Settings{PullThroughEnabled: true, PullThroughAllow: []string{"acme/*"}, PullThroughMinRole: "user"})
		anon := httpx.WithInfo(e.Ctx, "anon")
		httpx.SetPrincipal(anon, model.Anonymous())
		require.ErrorIs(t, pt.PullFiles(anon, r, commit, []string{"model-Q4_K_M.gguf"}), repo.ErrNotPresent, "anonymous callers need min_role anonymous")

		require.NoError(t, pt.PullFiles(ctx, r, commit, []string{"model-Q4_K_M.gguf"}))
		e.Run(10)
		en, err := e.Repos.Entry(e.Ctx, tiny, commit, "model-Q4_K_M.gguf")
		require.NoError(t, err)
		require.True(t, en.Present, "the Pull stored the file")
		js, _, err := e.Queue.List(e.Ctx, jobs.Filter{Kinds: []jobs.Kind{jobs.KindPull}})
		require.NoError(t, err)
		scoped := false
		for _, j := range js {
			scoped = scoped || (j.Repo == r.UUID && j.CreatedBy == alice.UserID)
		}
		require.True(t, scoped, "the Pull belongs to the caller and is scoped to the Mirror")
	})
}

// switchGate is a download gate a test turns on and off.
type switchGate struct {
	mu    sync.Mutex
	block bool
	fail  error
	calls int
}

func (g *switchGate) set(block bool, fail error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.block, g.fail = block, fail
}

func (g *switchGate) CheckObjects(_ context.Context, _ uuid.UUID, _ model.RepoID, oids []string) (map[string]error, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	if g.fail != nil {
		return nil, g.fail
	}
	out := map[string]error{}
	if g.block {
		for _, o := range oids {
			out[o] = errors.New("evil.pkl is marked unsafe by Hangar's security scan")
		}
	}
	return out, nil
}

// TestPullThroughLinkRechecksTheDownloadGate checks that a Pull-through
// link signed before an admin turned on scan.block_unsafe_downloads does
// not stream the object, as the signed git-lfs download link does not.
func TestPullThroughLinkRechecksTheDownloadGate(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		gate := &switchGate{}
		e := newEnv(t, d, upstreamtest.HarnessOptions{Downloads: gate})
		alice := e.User("alice")
		w := []byte("pickle bytes for git-lfs")
		e.Seed(tiny, nil, map[string][]byte{"evil.pkl": w})
		id, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Repo: tiny, Actor: alice, Selection: upstream.Selection{None: true}})
		require.NoError(t, err)
		e.Run(5)
		require.Equal(t, jobs.StatusSucceeded, e.Job(id).Status, e.Job(id).Error)
		r, _ := e.Repos.Get(e.Ctx, tiny)
		e.SetSettings(upstream.Settings{PullThroughEnabled: true, PullThroughAllow: []string{"*"}, PullThroughMinRole: "user"})
		pt := e.Service.PullThrough()
		href, ok, err := pt.DownloadURL(e.Ctx, alice, r, sha(w), int64(len(w)))
		require.NoError(t, err)
		require.True(t, ok)
		srv := httptest.NewServer(pt.Handler())
		defer srv.Close()
		u, _ := url.Parse(href)
		get := func() (int, string) {
			resp, err := http.Get(srv.URL + u.RequestURI())
			require.NoError(t, err)
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			return resp.StatusCode, string(body)
		}

		cdn := e.Server.Requests("cdn")
		gate.set(true, nil)
		status, body := get()
		require.Equal(t, http.StatusForbidden, status, body)
		require.Contains(t, body, "marked unsafe")
		require.Equal(t, cdn, e.Server.Requests("cdn"), "a refused object is not fetched from the Upstream")

		gate.set(false, errors.New("settings unavailable"))
		status, body = get()
		require.Equal(t, http.StatusInternalServerError, status, body)
		require.NotContains(t, body, "settings unavailable", "internal errors are not shown to clients")

		gate.set(false, nil)
		status, body = get()
		require.Equal(t, http.StatusOK, status, body)
		require.Equal(t, string(w), body)
		require.Equal(t, 3, gate.calls)
	})
}
