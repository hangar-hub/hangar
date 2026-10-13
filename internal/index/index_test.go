package index_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/index"
)

func TestQuantName(t *testing.T) {
	for in, want := range map[string]string{
		"Llama-3.2-3B-Instruct-Q4_K_M.gguf":                "Q4_K_M",
		"llama-3.2-3b-instruct-q8_0.gguf":                  "Q8_0",
		"model.IQ3_XXS.gguf":                               "IQ3_XXS",
		"sub/Qwen2.5-7B-Instruct-BF16-00001-of-00003.gguf": "BF16",
		"gpt-oss-20b-MXFP4.gguf":                           "MXFP4",
		"tinyllama-f16.gguf":                               "F16",
		"model.gguf":                                       "",
		"Q4_K_M-model-Q5_K_S.gguf":                         "Q5_K_S",
	} {
		require.Equal(t, want, index.QuantName(in), in)
	}
}

func TestGroupQuants(t *testing.T) {
	groups := index.GroupQuants(map[string]int64{
		"m-Q4_K_M.gguf":              100,
		"m-Q8_0-00001-of-00002.gguf": 300,
		"m-Q8_0-00002-of-00002.gguf": 200,
		"mmproj-m-f16.gguf":          10,
		"README.md":                  1,
		"plain.gguf":                 7,
	})
	require.Len(t, groups, 3)
	byName := map[string]index.QuantGroup{}
	for _, g := range groups {
		byName[g.Name] = g
	}
	require.Equal(t, []string{"m-Q8_0-00001-of-00002.gguf", "m-Q8_0-00002-of-00002.gguf"}, byName["Q8_0"].Files)
	require.Equal(t, int64(510), byName["Q8_0"].Size, "split files plus the projector")
	require.Equal(t, []string{"mmproj-m-f16.gguf"}, byName["Q4_K_M"].Projectors)
	require.Equal(t, []string{"plain.gguf"}, byName["plain"].Files)
	require.Empty(t, index.GroupQuants(map[string]int64{"a.safetensors": 1}))
}
