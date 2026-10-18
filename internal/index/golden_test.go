package index_test

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/db/dbtest"
	"github.com/hangar-hub/hangar/internal/index"
	"github.com/hangar-hub/hangar/internal/index/gguf"
	"github.com/hangar-hub/hangar/internal/index/indexdb"
	"github.com/hangar-hub/hangar/internal/index/indextest"
	"github.com/hangar-hub/hangar/internal/index/safetensors"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/repo/repotest"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata/golden")

// fixture is a test Repo: its files, and LFS files that are not present.
type fixture struct {
	name       string
	typ        model.RepoType
	files      func(t testing.TB) map[string][]byte
	notPresent []string
}

const mitText = `MIT License

Copyright (c) 2026 Example

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND.
`

func st(name, dtype string, shape ...int64) safetensors.Tensor {
	return safetensors.Tensor{Name: name, DType: dtype, Shape: shape}
}

func fixtures() []fixture {
	return []fixture{
		{name: "bert", typ: model.RepoTypeModel, files: func(t testing.TB) map[string][]byte {
			return map[string][]byte{
				"README.md":             []byte("---\nlicense: apache-2.0\ntags:\n- test\ndatasets:\n- fixtures/dataset\n- wikipedia\n---\n# Tiny BERT\n"),
				"config.json":           []byte(`{"architectures":["BertModel"],"model_type":"bert","max_position_embeddings":512,"vocab_size":1124}`),
				"tokenizer_config.json": []byte(`{"cls_token":"[CLS]","sep_token":"[SEP]","mask_token":"[MASK]","pad_token":"[PAD]","unk_token":"[UNK]"}`),
				"model.safetensors":     indextest.Safetensors(t, map[string]string{"format": "pt"}, st("embeddings.word_embeddings.weight", "F32", 1124, 32), st("embeddings.position_ids", "I64", 1, 512)),
			}
		}},
		{name: "gpt2-sharded", typ: model.RepoTypeModel, files: func(t testing.TB) map[string][]byte {
			return map[string][]byte{
				"config.json":                         []byte(`{"architectures":["GPT2LMHeadModel"],"model_type":"gpt2","n_positions":1024}`),
				"model-00001-of-00002.safetensors":    indextest.Safetensors(t, nil, st("wte.weight", "F16", 100, 16)),
				"model-00002-of-00002.safetensors":    indextest.Safetensors(t, nil, st("wpe.weight", "F16", 64, 16), st("ln_f.bias", "F32", 16)),
				"model.safetensors.index.json":        []byte(`{"metadata":{"total_size":3584},"weight_map":{"wte.weight":"model-00001-of-00002.safetensors","wpe.weight":"model-00002-of-00002.safetensors","ln_f.bias":"model-00002-of-00002.safetensors"}}`),
				"pytorch_model.bin":                   []byte("PK\x03\x04 a zip archive"),
				"tokenizer_config.json":               []byte(`{"bos_token":"<|endoftext|>","eos_token":"<|endoftext|>"}`),
				"onnx/decoder_model.onnx":             []byte("onnx bytes"),
				"subfolder/variant/model.safetensors": indextest.Safetensors(t, nil, st("x", "F32", 4)),
			}
		}},
		{name: "llama-chat", typ: model.RepoTypeModel, files: func(t testing.TB) map[string][]byte {
			return map[string][]byte{
				"config.json":           []byte(`{"architectures":["LlamaForCausalLM"],"model_type":"llama","max_position_embeddings":4096}`),
				"tokenizer_config.json": []byte(`{"bos_token":{"content":"<s>","lstrip":false},"eos_token":{"content":"</s>"},"chat_template":[{"name":"tool_use","template":"tools"},{"name":"default","template":"{{ messages }}"}]}`),
				"model.safetensors":     indextest.Safetensors(t, nil, st("model.embed_tokens.weight", "BF16", 32, 8), st("lm_head.weight", "BF16", 32, 8)),
				"LICENSE":               []byte(mitText),
			}
		}},
		{name: "gguf-quants", typ: model.RepoTypeModel, files: func(t testing.TB) map[string][]byte {
			split := func(no int) []byte {
				return indextest.LlamaGGUF(t, 1, "F16", gguf.KV{Key: "split.no", Value: uint32(no)}, gguf.KV{Key: "split.count", Value: uint32(2)})
			}
			return map[string][]byte{
				"README.md":                    []byte("---\nbase_model: example/Tiny-Llama\nlicense: llama3.1\npipeline_tag: text-generation\n---\n"),
				"tiny-Q4_K_M.gguf":             indextest.LlamaGGUF(t, 15, "Q4_K"),
				"tiny-Q8_0.gguf":               indextest.LlamaGGUF(t, 7, "Q8_0"),
				"tiny-F16-00001-of-00002.gguf": split(0),
				"tiny-F16-00002-of-00002.gguf": split(1),
				"mmproj-tiny-f16.gguf": indextest.GGUF(t, []gguf.KV{{Key: "general.architecture", Value: "clip"}, {Key: "general.file_type", Value: uint32(1)}},
					gguf.EncodeTensor{Name: "v.patch_embd.weight", Type: "F16", Dims: []uint64{16, 4}}),
			}
		}},
		{name: "partial-mirror", typ: model.RepoTypeModel, notPresent: []string{"tiny-Q8_0.gguf"}, files: func(t testing.TB) map[string][]byte {
			return map[string][]byte{
				"tiny-Q4_0.gguf": indextest.LlamaGGUF(t, 2, "Q4_0"),
				"tiny-Q8_0.gguf": indextest.LlamaGGUF(t, 7, "Q8_0"),
			}
		}},
		{name: "mlx", typ: model.RepoTypeModel, files: func(t testing.TB) map[string][]byte {
			return map[string][]byte{
				"README.md":         []byte("---\nlibrary_name: mlx\ntags:\n- mlx\nlicense: mit\n---\n"),
				"config.json":       []byte(`{"architectures":["LlamaForCausalLM"],"model_type":"llama","max_position_embeddings":2048,"quantization":{"group_size":64,"bits":4}}`),
				"model.safetensors": indextest.Safetensors(t, nil, st("model.embed_tokens.weight", "U32", 32, 8), st("model.embed_tokens.scales", "BF16", 32, 1)),
			}
		}},
		{name: "sentence-transformers", typ: model.RepoTypeModel, files: func(t testing.TB) map[string][]byte {
			return map[string][]byte{
				"modules.json":                      []byte(`[{"idx":0,"name":"0","path":"","type":"sentence_transformers.models.Transformer"}]`),
				"config_sentence_transformers.json": []byte(`{"__version__":{"sentence_transformers":"5.0.0"}}`),
				"config.json":                       []byte(`{"architectures":["BertModel"],"model_type":"bert","max_position_embeddings":512}`),
				"model.safetensors":                 indextest.Safetensors(t, nil, st("embeddings.weight", "F32", 16, 4)),
			}
		}},
		{name: "diffusers", typ: model.RepoTypeModel, files: func(t testing.TB) map[string][]byte {
			return map[string][]byte{
				"model_index.json":                         []byte(`{"_class_name":"StableDiffusionPipeline","_diffusers_version":"0.35.0"}`),
				"unet/diffusion_pytorch_model.safetensors": indextest.Safetensors(t, nil, st("conv_in.weight", "F16", 8, 4, 3, 3)),
				"text_encoder/model.safetensors":           indextest.Safetensors(t, nil, st("embeddings.weight", "F16", 16, 8)),
				"text_encoder/config.json":                 []byte(`{"architectures":["CLIPTextModel"]}`),
			}
		}},
		{name: "malformed", typ: model.RepoTypeModel, files: func(t testing.TB) map[string][]byte {
			truncated := indextest.LlamaGGUF(t, 2, "Q4_0")
			return map[string][]byte{
				"README.md":         []byte("---\nbase: &anchor x\nother: *anchor\n---\n"),
				"config.json":       []byte(`{"architectures": [`),
				"bad.safetensors":   []byte("\xff\xff\xff\xff\xff\xff\xff\xffnot json"),
				"short.safetensors": []byte("\x10\x00\x00\x00\x00\x00\x00\x00{}"),
				"broken.gguf":       truncated[:40],
				"notgguf.gguf":      []byte("GGML this is not gguf"),
			}
		}},
		{name: "other-formats", typ: model.RepoTypeModel, files: func(t testing.TB) map[string][]byte {
			return map[string][]byte{
				"model.onnx":          []byte("onnx"),
				"tf_model.h5":         []byte("h5"),
				"flax_model.msgpack":  []byte("msgpack"),
				"model.tflite":        []byte("tflite"),
				"openvino_model.xml":  []byte("<net/>"),
				"openvino_model.bin":  []byte("ov"),
				"Model.mlmodel":       []byte("coreml"),
				"adapter_config.json": []byte(`{"base_model_name_or_path":"example/base","peft_type":"LORA"}`),
				"LICENCE.txt":         []byte("                                 Apache License\n                           Version 2.0, January 2004\n"),
			}
		}},
		{name: "dataset", typ: model.RepoTypeDataset, files: func(t testing.TB) map[string][]byte {
			return map[string][]byte{
				"README.md":          []byte("---\npretty_name: Tiny Demo\nlicense: cc-by-4.0\ntask_categories:\n- text-classification\nlanguage:\n- en\n- fr\nsize_categories:\n- n<1K\n---\n# Demo\n"),
				"data/train.csv":     []byte("text,label\nhello,1\n"),
				"data/test.jsonl":    []byte(`{"text":"bye","label":0}` + "\n"),
				"images/cat.png":     []byte("\x89PNG"),
				"data/extra.parquet": []byte("PAR1"),
			}
		}},
	}
}

func indexFixture(t *testing.T, d *db.DB, f fixture) *index.Summary {
	t.Helper()
	ctx := context.Background()
	fake := repotest.NewFake(t)
	id := model.RepoID{Type: f.typ, Namespace: "fixtures", Name: f.name}
	r := indextest.CreateRepo(t, fake, id, model.VisibilityPublic)
	indextest.InsertRepoRow(t, d, r)
	files := f.files(t)
	sha := indextest.Commit(t, fake, id, files)
	if len(f.notPresent) > 0 {
		var refs []repo.LFSRef
		for _, p := range f.notPresent {
			e, err := fake.Entry(ctx, id, sha, p)
			require.NoError(t, err)
			refs = append(refs, repo.LFSRef{OID: e.LFS.OID, Size: e.Size, Present: false})
		}
		require.NoError(t, fake.SetLFSPresence(ctx, id, refs))
	}
	layout := func(context.Context, model.RepoID, model.CommitSHA) ([]string, map[string]int, error) {
		return []string{"default"}, map[string]int{"default": 2}, nil
	}
	// The dataset fixture's Parquet export: two Splits with their sizes.
	splits := func(context.Context, uuid.UUID, model.CommitSHA) ([]indexdb.DatasetSplit, error) {
		cols := []index.FeatureColumn{{Name: "text", Type: json.RawMessage(`{"dtype":"string","_type":"Value"}`)},
			{Name: "label", Type: json.RawMessage(`{"dtype":"int64","_type":"Value"}`)}}
		return []indexdb.DatasetSplit{
			{Config: "default", Split: "train", Converted: true, Rows: 1, Bytes: 20, ParquetBytes: 600, Features: cols},
			{Config: "default", Split: "test", Converted: true, Rows: 1, Bytes: 25, ParquetBytes: 610, Features: cols},
		}, nil
	}
	svc := indexdb.New(indexdb.Options{DB: d, Repos: fake, Layout: layout, DatasetSplits: splits, Window: 64})
	sum, err := svc.Index(ctx, r.UUID, id, sha)
	require.NoError(t, err)
	got, err := svc.Get(ctx, r.UUID, sha)
	require.NoError(t, err)
	want, _ := json.Marshal(sum)
	stored, _ := json.Marshal(got)
	require.JSONEq(t, string(want), string(stored), "stored results must equal what Index returned")
	return got
}

func TestIndexMatchesGoldenFiles(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		for _, f := range fixtures() {
			t.Run(f.name, func(t *testing.T) {
				sum := indexFixture(t, d, f)
				got, err := json.MarshalIndent(sum, "", "  ")
				require.NoError(t, err)
				got = append(got, '\n')
				golden := filepath.Join("testdata", "golden", f.name+".json")
				if *update && d.IsSQLite() {
					require.NoError(t, os.MkdirAll(filepath.Dir(golden), 0o755))
					require.NoError(t, os.WriteFile(golden, got, 0o644))
				}
				want, err := os.ReadFile(golden)
				require.NoError(t, err, "run go test ./internal/index -run TestIndexMatchesGoldenFiles -update to create it")
				require.JSONEq(t, string(want), string(got))
			})
		}
	})
}
