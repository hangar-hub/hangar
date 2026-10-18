package upstream_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/upstream"
)

func TestSelectionMatching(t *testing.T) {
	files := map[string]int64{
		"README.md": 10, "config.json": 10, "model.safetensors": 100, "onnx/model.onnx": 50, "onnx/model_fp16.onnx": 25,
		"Llama-Q4_K_M.gguf": 40, "Llama-IQ3_XXS-00001-of-00002.gguf": 20, "Llama-IQ3_XXS-00002-of-00002.gguf": 20,
		"mmproj-Llama-F16.gguf": 5, "data/train-0.parquet": 1, "data/test-0.parquet": 1,
	}
	match := func(s upstream.Selection, pats []string) []string {
		t.Helper()
		n, err := s.Normalise()
		require.NoError(t, err)
		m, err := upstream.NewMatcher(n, files, pats)
		require.NoError(t, err)
		var out []string
		for _, p := range []string{"README.md", "config.json", "model.safetensors", "onnx/model.onnx", "onnx/model_fp16.onnx", "Llama-Q4_K_M.gguf",
			"Llama-IQ3_XXS-00001-of-00002.gguf", "Llama-IQ3_XXS-00002-of-00002.gguf", "mmproj-Llama-F16.gguf", "data/train-0.parquet", "data/test-0.parquet"} {
			if m.Match(p) {
				out = append(out, p)
			}
		}
		return out
	}
	require.Len(t, match(upstream.Selection{}, nil), len(files), "empty means every file")
	require.Empty(t, match(upstream.Selection{None: true}, nil))
	require.Equal(t, []string{"onnx/model.onnx", "onnx/model_fp16.onnx"}, match(upstream.Selection{Include: []string{"onnx/"}}, nil), "a folder pattern takes everything under it")
	require.Equal(t, []string{"onnx/model.onnx"}, match(upstream.Selection{Include: []string{"*.onnx"}, Exclude: []string{"*fp16*"}}, nil), "* crosses folders, as in allow_patterns")
	require.Equal(t, []string{"config.json", "model.safetensors"}, match(upstream.Selection{Include: []string{"model.[!o]afetensor?"}, Paths: []string{"config.json"}}, nil))
	require.Equal(t, []string{"Llama-IQ3_XXS-00001-of-00002.gguf", "Llama-IQ3_XXS-00002-of-00002.gguf", "mmproj-Llama-F16.gguf"},
		match(upstream.Selection{Quants: []string{"iq3_xxs"}}, nil), "split files form one quant, and projectors come with it")
	require.Equal(t, []string{"data/train-0.parquet"}, match(upstream.Selection{Configs: []string{"default"}}, []string{"data/train-*"}))
	require.Equal(t, []string{"README.md", "config.json", "model.safetensors", "onnx/model.onnx", "onnx/model_fp16.onnx"},
		match(upstream.Selection{Exclude: []string{"*.gguf", "data/**"}}, nil))
}

func TestSelectionNormaliseAndUnion(t *testing.T) {
	s, err := upstream.Selection{Quants: []string{" q4_k_m", "Q4_K_M"}, Paths: []string{"b", "a", "a"}}.Normalise()
	require.NoError(t, err)
	require.Equal(t, []string{"Q4_K_M"}, s.Quants)
	require.Equal(t, []string{"a", "b"}, s.Paths)
	for _, bad := range []upstream.Selection{{Paths: []string{"/abs"}}, {Paths: []string{"a/../b"}}, {Include: []string{strings.Repeat("a", 1100)}},
		{None: true, Paths: []string{"a"}}} {
		_, err := bad.Normalise()
		require.ErrorIs(t, err, upstream.ErrInvalidSelection, "%+v", bad)
	}
	q4 := upstream.Selection{Quants: []string{"Q4_K_M"}, Exclude: []string{"x"}}
	q8 := upstream.Selection{Quants: []string{"Q8_0"}, Exclude: []string{"x", "y"}}
	u := q4.Union(q8)
	require.Equal(t, []string{"Q4_K_M", "Q8_0"}, u.Quants)
	require.Equal(t, []string{"x"}, u.Exclude, "only excludes both sides share survive")
	require.True(t, q4.Union(upstream.Selection{}).All())
	require.Equal(t, q4, upstream.Selection{None: true}.Union(q4))
	require.Equal(t, q4, q4.Union(upstream.Selection{None: true}))
	pq := upstream.Selection{Configs: []string{"a"}, ParquetOnly: true}
	require.False(t, pq.Union(upstream.Selection{Configs: []string{"b"}}).ParquetOnly)
	require.True(t, upstream.MatchGlob("jinaai/*", "jinaai/jina-embeddings-v3"))
	require.False(t, upstream.MatchGlob("jinaai/*", "other/x"))
	require.True(t, upstream.MatchGlob("[", "["), "an unclosed class is a literal, as in fnmatch")
}
