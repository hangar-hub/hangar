package hfapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/hfapi"
	"github.com/jamesagarside/hangar/internal/index"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/scan"
)

// hubDate is the only date format huggingface_hub parses.
var hubDate = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$`)

func TestRepoInfo(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	weights := strings.Repeat("w", 100)
	rp, sha := seed(t, e, "alice", modelID("alice", "bert"), model.VisibilityPublic, map[string]string{
		"config.json": configJSON, "model.safetensors": weights, "unet/config.json": "{}", "unet/diffusion_pytorch_model.safetensors": "u",
	})
	e.Index.SetSummary(rp.UUID, sha, &index.Summary{
		Formats: []string{"safetensors"}, Parameters: &index.Parameters{Total: 25, ByDType: map[string]int64{"F32": 25}},
		CardData: map[string]any{"license": "mit", "base_model": "x/y"}, PipelineTag: "fill-mask", LibraryName: "transformers",
		Tags: []string{"bert", "safetensors"}, Config: json.RawMessage(`{"model_type":"bert"}`),
		GGUF: &index.GGUF{Total: 3, Architecture: "llama", ContextLength: 2048, ChatTemplate: "t", BOSToken: "<s>", EOSToken: "</s>"},
	})
	ctx := context.Background()
	_, err := e.Index.Index(ctx, rp.UUID, rp.ID, sha)
	require.NoError(t, err)
	require.NoError(t, e.Community.Like(ctx, e.Users["bob"].Principal.UserID, rp.UUID))
	require.NoError(t, e.Community.RecordDownload(ctx, rp.UUID, time.Now()))

	info := decode[map[string]any](t, e.Request(t, http.MethodGet, "/api/models/alice/bert/revision/main?blobs=True&securityStatus=1", "", ""))
	require.Equal(t, "alice/bert", info["id"])
	require.Equal(t, "alice/bert", info["modelId"])
	require.Equal(t, "alice", info["author"])
	require.Equal(t, string(sha), info["sha"])
	require.Equal(t, false, info["private"])
	require.Equal(t, false, info["gated"])
	require.Equal(t, false, info["disabled"])
	require.EqualValues(t, 1, info["likes"])
	require.EqualValues(t, 1, info["downloads"])
	require.Regexp(t, hubDate, info["lastModified"])
	require.Regexp(t, hubDate, info["createdAt"])
	require.Equal(t, "fill-mask", info["pipeline_tag"])
	require.Equal(t, "transformers", info["library_name"])
	require.Equal(t, map[string]any{"model_type": "bert"}, info["config"])
	require.Equal(t, map[string]any{"parameters": map[string]any{"F32": float64(25)}, "total": float64(25)}, info["safetensors"])
	require.Equal(t, "llama", info["gguf"].(map[string]any)["architecture"])
	require.Equal(t, "mit", info["cardData"].(map[string]any)["license"])
	require.ElementsMatch(t, []any{"bert", "safetensors"}, info["tags"])
	require.Equal(t, map[string]any{"scansDone": false, "filesWithIssues": []any{}}, info["securityRepoStatus"])
	for _, k := range []string{"transformersInfo", "inferenceProviderMapping", "evalResults", "xetHash"} {
		require.NotContains(t, info, k)
	}
	siblings := info["siblings"].([]any)
	var names []string
	for _, s := range siblings {
		m := s.(map[string]any)
		names = append(names, m["rfilename"].(string))
		require.Contains(t, m, "blobId")
		require.Contains(t, m, "size")
		if m["rfilename"] == "model.safetensors" {
			require.Equal(t, map[string]any{"sha256": sha256hex(weights), "size": float64(100), "pointerSize": float64(model.LFSPointer{OID: sha256hex(weights), Size: 100}.PointerSize())}, m["lfs"])
		}
	}
	sort.Strings(names)
	require.Equal(t, []string{".gitattributes", "config.json", "model.safetensors", "unet/config.json", "unet/diffusion_pytorch_model.safetensors"}, names)

	// Without blobs, siblings carry only rfilename; expand selects fields.
	info = decode[map[string]any](t, e.Request(t, http.MethodGet, "/api/models/alice/bert", "", ""))
	require.Equal(t, map[string]any{"rfilename": ".gitattributes"}, info["siblings"].([]any)[0])
	info = decode[map[string]any](t, e.Request(t, http.MethodGet, "/api/models/alice/bert?expand=downloadsAllTime&expand[]=baseModels&expand=sha&expand=trendingScore&expand=model-index&expand=unknown", "", ""))
	require.Equal(t, map[string]any{"_id": rp.UUID.String(), "id": "alice/bert", "downloadsAllTime": float64(1), "baseModels": "x/y", "sha": string(sha), "trendingScore": float64(0)}, info)

	// Scan results give the Repo status and per-file status.
	e.Scan.SetResult("model.safetensors", scan.StatusUnsafe, scan.Finding{Module: "os", Name: "system", Status: scan.StatusUnsafe})
	_, err = e.Scan.ScanCommit(ctx, rp.UUID, rp.ID, sha)
	require.NoError(t, err)
	info = decode[map[string]any](t, e.Request(t, http.MethodGet, "/api/models/alice/bert?securityStatus=true", "", ""))
	require.Equal(t, true, info["securityRepoStatus"].(map[string]any)["scansDone"])
	require.Equal(t, []any{map[string]any{"path": "model.safetensors", "level": "danger"}}, info["securityRepoStatus"].(map[string]any)["filesWithIssues"])

	requireHubError(t, e.Request(t, http.MethodGet, "/api/models/alice/bert/revision/nope", "", ""), http.StatusNotFound, "RevisionNotFound")
	requireHubError(t, e.Request(t, http.MethodGet, "/api/models/alice/none", "", ""), http.StatusUnauthorized, "RepoNotFound")
	requireHubError(t, e.Request(t, http.MethodGet, "/api/datasets/alice/bert", "bob", ""), http.StatusNotFound, "RepoNotFound")
}

func TestDatasetInfoAndEmptyMirror(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	_, err := e.Repos.Create(context.Background(), repo.CreateParams{ID: datasetID("alice", "d"), Visibility: model.VisibilityPrivate, Description: "Rows", Actor: e.Users["alice"].Principal})
	require.NoError(t, err)
	info := decode[map[string]any](t, e.Request(t, http.MethodGet, "/api/datasets/alice/d", "alice", ""))
	require.Equal(t, "Rows", info["description"])
	require.Equal(t, true, info["private"])
	require.NotContains(t, info, "modelId")

	_, err = e.Repos.Create(context.Background(), repo.CreateParams{ID: modelID("alice", "planning"), Mirror: true, Actor: e.Users["alice"].Principal})
	require.NoError(t, err)
	info = decode[map[string]any](t, e.Request(t, http.MethodGet, "/api/models/alice/planning", "", ""))
	require.NotContains(t, info, "sha")
	require.Equal(t, []any{}, info["siblings"])
}

func TestTree(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	weights := "abc"
	rp, sha := seed(t, e, "alice", modelID("alice", "tree"), model.VisibilityPublic, map[string]string{
		"config.json": configJSON, "model.safetensors": weights, "onnx/model.onnx": "o", "onnx/sub/x.txt": "x",
	})
	entries := decode[[]map[string]any](t, e.Request(t, http.MethodGet, "/api/models/alice/tree/tree/main?recursive=true&expand=false", "", ""))
	byPath := map[string]map[string]any{}
	for _, en := range entries {
		byPath[en["path"].(string)] = en
		require.NotContains(t, en, "xetHash")
	}
	require.Equal(t, "directory", byPath["onnx"]["type"])
	require.Equal(t, "directory", byPath["onnx/sub"]["type"])
	require.Equal(t, "file", byPath["onnx/sub/x.txt"]["type"])
	require.Equal(t, model.GitBlobID([]byte(configJSON)), byPath["config.json"]["oid"])
	require.EqualValues(t, 3, byPath["model.safetensors"]["size"])
	require.Equal(t, sha256hex(weights), byPath["model.safetensors"]["lfs"].(map[string]any)["oid"])

	// Non-recursive listing of a subdirectory, with the path encoded as one segment.
	sub := decode[[]map[string]any](t, e.Request(t, http.MethodGet, "/api/models/alice/tree/tree/"+string(sha)+"/onnx%2Fsub?recursive=false&expand=false", "", ""))
	require.Len(t, sub, 1)
	require.Equal(t, "onnx/sub/x.txt", sub[0]["path"])
	sub = decode[[]map[string]any](t, e.Request(t, http.MethodGet, "/api/models/alice/tree/tree/main/onnx/sub", "", ""))
	require.Len(t, sub, 1)

	// expand adds lastCommit, and securityFileStatus once the Commit is scanned.
	e.Scan.SetResult("model.safetensors", scan.StatusSuspicious, scan.Finding{Module: "x", Name: "y", Status: scan.StatusSuspicious})
	_, err := e.Scan.ScanCommit(context.Background(), rp.UUID, rp.ID, sha)
	require.NoError(t, err)
	expanded := decode[[]map[string]any](t, e.Request(t, http.MethodGet, "/api/models/alice/tree/tree/main?expand=True", "", ""))
	for _, en := range expanded {
		if en["type"] != "file" {
			continue
		}
		lc := en["lastCommit"].(map[string]any)
		if en["path"] != ".gitattributes" {
			require.Equal(t, string(sha), lc["id"])
		}
		require.Regexp(t, hubDate, lc["date"])
		sec := en["securityFileStatus"].(map[string]any)
		for _, k := range []string{"status", "avScan", "pickleImportScan"} {
			require.Contains(t, sec, k)
		}
		if en["path"] == "model.safetensors" {
			require.Equal(t, "caution", sec["status"])
		}
	}

	requireHubError(t, e.Request(t, http.MethodGet, "/api/models/alice/tree/tree/main/config.json", "", ""), http.StatusNotFound, "EntryNotFound")
	requireHubError(t, e.Request(t, http.MethodGet, "/api/models/alice/tree/tree/main/nope", "", ""), http.StatusNotFound, "EntryNotFound")
	requireHubError(t, e.Request(t, http.MethodGet, "/api/models/alice/tree/tree/nope", "", ""), http.StatusNotFound, "RevisionNotFound")
	require.Equal(t, http.StatusBadRequest, e.Request(t, http.MethodGet, "/api/models/alice/tree/tree/main?cursor=!!!", "", "").StatusCode)
}

func TestTreePagination(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	files := map[string]string{}
	for i := 0; i < 7; i++ {
		files["f"+string(rune('a'+i))+".txt"] = "x"
	}
	seed(t, e, "alice", modelID("alice", "paged"), model.VisibilityPublic, files)
	next := "/api/models/alice/paged/tree/main?recursive=true&expand=false&limit=3"
	var seen []string
	for pages := 0; next != ""; pages++ {
		require.Less(t, pages, 5)
		r := e.Request(t, http.MethodGet, next, "", "")
		require.Equal(t, http.StatusOK, r.StatusCode)
		for _, en := range decode[[]map[string]any](t, r) {
			seen = append(seen, en["path"].(string))
		}
		next = ""
		if link := r.Header.Get("Link"); link != "" {
			m := regexp.MustCompile(`^<([^>]+)>; rel="next"$`).FindStringSubmatch(link)
			require.NotNil(t, m, link)
			require.True(t, strings.HasPrefix(m[1], e.URL+"/"), "Link must be absolute on public_url: %s", m[1])
			u, err := url.Parse(m[1])
			require.NoError(t, err)
			next = u.RequestURI()
		}
	}
	require.Len(t, seen, 8) // seven files and .gitattributes
}

func TestPathsInfo(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	seed(t, e, "alice", modelID("alice", "pi"), model.VisibilityPublic, map[string]string{"config.json": configJSON, "onnx/m.onnx": "m"})
	r := e.Request(t, http.MethodPost, "/api/models/alice/pi/paths-info/main", "", "paths=config.json&paths=onnx&paths=nope.txt&paths=..%2Fbad&expand=false",
		"Content-Type", "application/x-www-form-urlencoded")
	entries := decode[[]map[string]any](t, r)
	require.Len(t, entries, 2)
	r = e.Request(t, http.MethodPost, "/api/models/alice/pi/paths-info/main", "", `{"paths":["onnx/m.onnx"],"expand":"true"}`, "Content-Type", "application/json")
	entries = decode[[]map[string]any](t, r)
	require.Len(t, entries, 1)
	require.Contains(t, entries[0], "lastCommit")
	require.Equal(t, []map[string]any{}, decode[[]map[string]any](t, e.Request(t, http.MethodPost, "/api/models/alice/pi/paths-info/main", "", "paths=nope", "Content-Type", "application/x-www-form-urlencoded")))
	require.Equal(t, http.StatusBadRequest, e.Request(t, http.MethodPost, "/api/models/alice/pi/paths-info/main", "", `{`, "Content-Type", "application/json").StatusCode)
	require.Equal(t, http.StatusBadRequest, e.Request(t, http.MethodPost, "/api/models/alice/pi/paths-info/main", "", "%zz", "Content-Type", "application/x-www-form-urlencoded").StatusCode)
	many := strings.Repeat("paths=a&", 1001)
	require.Equal(t, http.StatusBadRequest, e.Request(t, http.MethodPost, "/api/models/alice/pi/paths-info/main", "", many, "Content-Type", "application/x-www-form-urlencoded").StatusCode)
	require.Equal(t, http.StatusMethodNotAllowed, e.Request(t, http.MethodGet, "/api/models/alice/pi/paths-info/main", "", "").StatusCode)
}

func TestRefsAndCommits(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := modelID("alice", "hist")
	_, first := seed(t, e, "alice", id, model.VisibilityPublic, map[string]string{"a.txt": "1"})
	for i := 0; i < 3; i++ {
		commitFiles(t, e, "alice", id, "main", map[string]string{"a.txt": strings.Repeat("x", i+2)})
	}
	ctx := context.Background()
	alice := e.Users["alice"].Principal
	require.NoError(t, e.Repos.CreateTag(ctx, repo.TagParams{Repo: id, Tag: "v1", Revision: string(first), Actor: alice}))
	_, err := e.Repos.Commit(ctx, repo.CommitParams{Repo: id, Revision: "refs/pr/1", CreateFrom: first, Summary: "pr", Actor: alice,
		Operations: []repo.Operation{{Kind: repo.OpAdd, Path: "b.txt", Content: strings.NewReader("b")}}})
	require.NoError(t, err)

	refs := decode[map[string]any](t, e.Request(t, http.MethodGet, "/api/models/alice/hist/refs", "", ""))
	require.Contains(t, refs, "converts")
	require.NotContains(t, refs, "pullRequests")
	require.Equal(t, "main", refs["branches"].([]any)[0].(map[string]any)["name"])
	require.Equal(t, map[string]any{"name": "v1", "ref": "refs/tags/v1", "targetCommit": string(first)}, refs["tags"].([]any)[0])
	refs = decode[map[string]any](t, e.Request(t, http.MethodGet, "/api/models/alice/hist/refs?include_prs=1", "", ""))
	require.Equal(t, "refs/pr/1", refs["pullRequests"].([]any)[0].(map[string]any)["ref"])

	r := e.Request(t, http.MethodGet, "/api/models/alice/hist/commits/main?limit=2&expand[]=formatted", "", "")
	page := decode[[]map[string]any](t, r)
	require.Len(t, page, 2)
	require.NotEmpty(t, r.Header.Get("Link"))
	for _, c := range page {
		for _, k := range []string{"id", "title", "message", "date", "authors", "formatted"} {
			require.Contains(t, c, k)
		}
		require.Equal(t, "alice", c["authors"].([]any)[0].(map[string]any)["user"])
		require.Regexp(t, hubDate, c["date"])
	}
	all := decode[[]map[string]any](t, e.Request(t, http.MethodGet, "/api/models/alice/hist/commits/main", "", ""))
	require.Len(t, all, 5) // initial .gitattributes Commit plus four
	require.Equal(t, string(first), all[3]["id"])
	requireHubError(t, e.Request(t, http.MethodGet, "/api/models/alice/hist/commits/nope", "", ""), http.StatusNotFound, "RevisionNotFound")
}

func TestSafetensorsConversionGuard(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	seed(t, e, "alice", modelID("alice", "pickle"), model.VisibilityPublic, map[string]string{"pytorch_model.bin": "pickle", "config.json": configJSON})
	seed(t, e, "alice", modelID("alice", "safe"), model.VisibilityPublic, map[string]string{"model.safetensors": "st"})
	ua := []string{"User-Agent", "transformers/5.19.0; python/3.11.9; session_id/x; torch/2.14.1"}
	r := e.Request(t, http.MethodGet, "/api/models/alice/pickle/commits/main", "", "", ua...)
	require.Equal(t, http.StatusForbidden, r.StatusCode)
	require.Equal(t, hfapi.SafetensorsGuardMessage, r.Header.Get("X-Error-Message"))
	require.Equal(t, http.StatusOK, e.Request(t, http.MethodGet, "/api/models/alice/safe/commits/main", "", "", ua...).StatusCode)
	require.Equal(t, http.StatusOK, e.Request(t, http.MethodGet, "/api/models/alice/pickle/commits/main", "", "", "User-Agent", "unknown/None; hf_hub/2.2.0").StatusCode)
}

func TestBranchesAndTags(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	seed(t, e, "alice", modelID("alice", "br"), model.VisibilityPublic, map[string]string{"a.txt": "a"})
	base := "/api/models/alice/br"
	require.Equal(t, http.StatusOK, e.Request(t, http.MethodPost, base+"/branch/feature%2Fx", "alice", "").StatusCode)
	require.Equal(t, http.StatusOK, e.Request(t, http.MethodPost, base+"/branch/dev", "alice", `{"startingPoint":"feature/x"}`).StatusCode)
	require.Equal(t, http.StatusConflict, e.Request(t, http.MethodPost, base+"/branch/dev", "alice", `{}`).StatusCode)
	requireHubError(t, e.Request(t, http.MethodPost, base+"/branch/z", "alice", `{"startingPoint":"nope"}`), http.StatusNotFound, "RevisionNotFound")
	require.Equal(t, http.StatusForbidden, e.Request(t, http.MethodPost, base+"/branch/bob", "bob", "").StatusCode)
	require.Equal(t, http.StatusUnauthorized, e.Request(t, http.MethodPost, base+"/branch/anon", "", "").StatusCode)
	require.Equal(t, http.StatusForbidden, e.Request(t, http.MethodPost, base+"/branch/ro", "carol", "").StatusCode, "a read Token cannot write")
	require.Equal(t, http.StatusBadRequest, e.Request(t, http.MethodPost, base+"/branch/bad", "alice", `{`).StatusCode)
	require.Equal(t, http.StatusOK, e.Request(t, http.MethodDelete, base+"/branch/dev", "alice", "").StatusCode)
	require.Equal(t, http.StatusForbidden, e.Request(t, http.MethodDelete, base+"/branch/main", "alice", "").StatusCode)

	require.Equal(t, http.StatusOK, e.Request(t, http.MethodPost, base+"/tag/main", "alice", `{"tag":"v1","message":"first"}`).StatusCode)
	require.Equal(t, http.StatusConflict, e.Request(t, http.MethodPost, base+"/tag/main", "alice", `{"tag":"v1"}`).StatusCode)
	require.Equal(t, http.StatusBadRequest, e.Request(t, http.MethodPost, base+"/tag/main", "alice", `{}`).StatusCode)
	requireHubError(t, e.Request(t, http.MethodPost, base+"/tag/nope", "alice", `{"tag":"v2"}`), http.StatusNotFound, "RevisionNotFound")
	refs := decode[map[string]any](t, e.Request(t, http.MethodGet, base+"/refs", "", ""))
	require.Len(t, refs["tags"], 1)
	require.Len(t, refs["branches"], 2)
	require.Equal(t, http.StatusOK, e.Request(t, http.MethodDelete, base+"/tag/v1", "alice", "").StatusCode)
}

func TestListRepos(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	seed(t, e, "alice", modelID("alice", "gpt2-small"), model.VisibilityPublic, map[string]string{"config.json": configJSON})
	seed(t, e, "alice", modelID("alice", "gpt2-large"), model.VisibilityPublic, nil)
	seed(t, e, "alice", modelID("alice", "hidden-gpt2"), model.VisibilityPrivate, nil)
	seed(t, e, "bob", modelID("bob", "other"), model.VisibilityPublic, nil)
	seed(t, e, "bob", datasetID("bob", "rows"), model.VisibilityPublic, nil)

	list := decode[[]map[string]any](t, e.Request(t, http.MethodGet, "/api/models?search=gpt2&sort=lastModified&direction=-1", "", ""))
	require.Len(t, list, 2)
	list = decode[[]map[string]any](t, e.Request(t, http.MethodGet, "/api/models?search=gpt2", "alice", ""))
	require.Len(t, list, 3)
	list = decode[[]map[string]any](t, e.Request(t, http.MethodGet, "/api/models?author=bob&full=true", "", ""))
	require.Len(t, list, 1)
	require.Contains(t, list[0], "siblings")
	require.Contains(t, list[0], "sha")
	list = decode[[]map[string]any](t, e.Request(t, http.MethodGet, "/api/datasets?expand=sha&expand=downloads", "", ""))
	require.Len(t, list, 1)
	require.Equal(t, "bob/rows", list[0]["id"])
	require.Contains(t, list[0], "sha")

	// Pages follow an absolute Link; a limit gives one page.
	r := e.Request(t, http.MethodGet, "/api/models?limit=1", "", "")
	require.Len(t, decode[[]map[string]any](t, r), 1)
	require.Empty(t, r.Header.Get("Link"))
	require.Equal(t, http.StatusBadRequest, e.Request(t, http.MethodGet, "/api/models?sort=nonsense", "", "").StatusCode)
	require.Empty(t, decode[[]map[string]any](t, e.Request(t, http.MethodGet, "/api/models?filter=no-such-tag", "", "")))
}
