package deps_test

import (
	"context"
	"fmt"
	"io/fs"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/db/dbtest"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/upstream/deps"
)

func reader(files map[string]string) (deps.Reader, []string) {
	var paths []string
	for p := range files {
		paths = append(paths, p)
	}
	return func(_ context.Context, p string) ([]byte, error) {
		c, ok := files[p]
		if !ok {
			return nil, fmt.Errorf("%s: %w", p, fs.ErrNotExist)
		}
		return []byte(c), nil
	}, paths
}

func mid(s string) model.RepoID {
	id, err := model.ParseRepoID(model.RepoTypeModel, s)
	if err != nil {
		panic(err)
	}
	return id
}

var self = mid("jinaai/jina-embeddings-v3")

func TestParseJinaAutoMap(t *testing.T) {
	// The real config.json of jinaai/jina-embeddings-v3 maps its classes to
	// another Repo, jinaai/xlm-roberta-flash-implementation.
	read, files := reader(map[string]string{
		"config.json": `{"architectures":["XLMRobertaLoRA"],"auto_map":{
			"AutoConfig":"jinaai/xlm-roberta-flash-implementation--configuration_xlm_roberta.XLMRobertaFlashConfig",
			"AutoModel":"jinaai/xlm-roberta-flash-implementation--modeling_lora.XLMRobertaLoRA",
			"AutoModelForMaskedLM":"jinaai/xlm-roberta-flash-implementation--modeling_xlm_roberta.XLMRobertaForMaskedLM",
			"AutoModelForPreTraining":"jinaai/xlm-roberta-flash-implementation--modeling_xlm_roberta.XLMRobertaForPreTraining"}}`,
		"tokenizer_config.json":  `{"auto_map":{"AutoTokenizer":["acme/tok@v2--tokenization.Slow", null, "local_tokenization.Fast"]}}`,
		"generation_config.json": `{"auto_map":{"AutoModel":"modeling.Local"}}`,
		"model.safetensors":      "binary",
	})
	got, err := deps.Parse(context.Background(), self, files, read)
	require.NoError(t, err)
	require.Equal(t, []deps.Dependency{
		{Target: mid("jinaai/xlm-roberta-flash-implementation"), Kind: deps.KindCode, Required: true, SourcePath: "config.json", SourceKey: "auto_map.AutoConfig"},
		{Target: mid("acme/tok"), Revision: "v2", Kind: deps.KindCode, Required: true, SourcePath: "tokenizer_config.json", SourceKey: "auto_map.AutoTokenizer"},
	}, got, "one row per target, values with no -- are in the same Repo")
}

func TestParseOtherSources(t *testing.T) {
	read, files := reader(map[string]string{
		"adapter_config.json":   `{"base_model_name_or_path":"meta-llama/Llama-3.2-1B","peft_type":"LORA"}`,
		"README.md":             "---\nbase_model:\n- Qwen/Qwen2.5-0.5B\n- not a repo\nbase_model_relation: finetune\ndatasets: [HuggingFaceFW/fineweb, single]\n---\n# Card\n",
		"modules.json":          `[{"idx":0,"name":"0","path":"","type":"sentence_transformers.models.Transformer"},{"idx":1,"name":"1","path":"1_Pooling","type":"x"},{"idx":2,"name":"2","path":"acme/dense-head","type":"x"}]`,
		"1_Pooling/config.json": `{}`,
		"model_index.json":      `{"_class_name":"StableDiffusionPipeline","unet":["diffusers","UNet2DConditionModel"],"text_encoder":["acme/custom-encoder","Encoder"],"scheduler":["diffusers",null]}`,
		"config.json":           `{"auto_map":"not an object"}`,
	})
	got, err := deps.Parse(context.Background(), self, files, read)
	require.NoError(t, err)
	want := []deps.Dependency{
		{Target: mid("Qwen/Qwen2.5-0.5B"), Kind: deps.KindCardBaseModel, SourcePath: "README.md", SourceKey: "base_model"},
		{Target: model.RepoID{Type: model.RepoTypeDataset, Namespace: "HuggingFaceFW", Name: "fineweb"}, Kind: deps.KindDataset, SourcePath: "README.md", SourceKey: "datasets"},
		{Target: mid("meta-llama/Llama-3.2-1B"), Kind: deps.KindAdapterBase, SourcePath: "adapter_config.json", SourceKey: "base_model_name_or_path"},
		{Target: mid("acme/custom-encoder"), Kind: deps.KindCode, Required: true, SourcePath: "model_index.json", SourceKey: "text_encoder"},
		{Target: mid("acme/dense-head"), Kind: deps.KindCode, Required: true, SourcePath: "modules.json", SourceKey: "[2].path"},
	}
	require.Equal(t, want, got)
}

func TestParseIgnoresLocalPathsAndBrokenFiles(t *testing.T) {
	read, files := reader(map[string]string{
		"adapter_config.json": `{"base_model_name_or_path":"/home/me/models/llama"}`,
		"README.md":           "no front matter\nbase_model: acme/x\n",
		"config.json":         `{not json`,
		"modules.json":        `[{"path":"./local"}]`,
	})
	got, err := deps.Parse(context.Background(), self, files, read)
	require.NoError(t, err)
	require.Empty(t, got)
	// A file listed in the Commit but unreadable (not in the arsenal) is skipped.
	got, err = deps.Parse(context.Background(), self, []string{"config.json"}, func(context.Context, string) ([]byte, error) { return nil, fs.ErrNotExist })
	require.NoError(t, err)
	require.Empty(t, got)
	_, err = deps.Parse(context.Background(), self, []string{"config.json"}, func(context.Context, string) ([]byte, error) { return nil, fmt.Errorf("storage down") })
	require.Error(t, err)
}

func TestFrontMatter(t *testing.T) {
	require.Equal(t, map[string]any{"license": "mit"}, deps.FrontMatter([]byte("\ufeff---\r\nlicense: mit\r\n---\r\nbody")))
	require.Nil(t, deps.FrontMatter([]byte("---\nunterminated: true\n")))
	require.Nil(t, deps.FrontMatter([]byte("---\n: : bad yaml [\n---\n")))
	require.Contains(t, deps.SourceFiles(), "model_index.json")
}

func FuzzParse(f *testing.F) {
	f.Add(`{"auto_map":{"AutoModel":"a/b--m.C","AutoTokenizer":["c/d@e--t.T",null]}}`, "---\nbase_model: [x/y]\ndatasets: z/w\n---\n")
	f.Add(`{"auto_map":[]}`, "---\n---\n")
	f.Fuzz(func(t *testing.T, cfg, card string) {
		read, files := reader(map[string]string{"config.json": cfg, "README.md": card, "modules.json": cfg, "model_index.json": cfg, "adapter_config.json": cfg})
		got, err := deps.Parse(context.Background(), self, files, read)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range got {
			if err := d.Target.Validate(); err != nil {
				t.Fatalf("invalid target %v: %v", d.Target, err)
			}
		}
	})
}

func TestStore(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		now := db.Now()
		ns := db.Namespace{ID: db.NewID(), Name: "jinaai", Kind: "org", Timestamps: db.Timestamps{CreatedAt: now, UpdatedAt: now}}
		_, err := d.NewInsert().Model(&ns).Exec(ctx)
		require.NoError(t, err)
		r := db.Repo{ID: db.NewID(), NamespaceID: ns.ID, Name: "v3", Type: "model", Visibility: "public", DefaultBranch: "main", HeadRef: "refs/heads/main",
			Tags: []string{}, Settings: map[string]any{}, Gating: map[string]any{}, Timestamps: db.Timestamps{CreatedAt: now, UpdatedAt: now}}
		_, err = d.NewInsert().Model(&r).Exec(ctx)
		require.NoError(t, err)
		s := deps.NewStore(d)
		c1 := model.CommitSHA("1111111111111111111111111111111111111111")
		ds := []deps.Dependency{{Target: mid("jinaai/xlm-roberta-flash-implementation"), Kind: deps.KindCode, Required: true, SourcePath: "config.json", SourceKey: "auto_map.AutoModel"}}
		require.NoError(t, s.Replace(ctx, r.ID, c1, ds))
		require.NoError(t, s.Replace(ctx, r.ID, c1, ds), "replacing is idempotent")
		got, err := s.Needs(ctx, r.ID, c1)
		require.NoError(t, err)
		require.Equal(t, ds, got)
		by, err := s.NeededBy(ctx, mid("JinaAI/XLM-Roberta-Flash-Implementation"))
		require.NoError(t, err)
		require.Len(t, by, 1, "targets match case-insensitively")
		require.Equal(t, r.ID, by[0].RepoUUID)
		require.Equal(t, c1, by[0].Commit)
		require.NoError(t, s.Replace(ctx, r.ID, c1, nil))
		got, err = s.Needs(ctx, r.ID, c1)
		require.NoError(t, err)
		require.Empty(t, got)
	})
}
