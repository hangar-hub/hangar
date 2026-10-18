package indexdb

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/index/gguf"
)

func TestPlainValue(t *testing.T) {
	require.Equal(t, "llama", plainValue(gguf.Value{Type: gguf.TypeString, Scalar: "llama"}))
	require.Equal(t, []any{int64(1), int64(2)}, plainValue(gguf.Value{Type: gguf.TypeArray, Len: 2, Items: []any{int64(1), int64(2)}}))
	require.Equal(t, []any{}, plainValue(gguf.Value{Type: gguf.TypeArray, Len: 0}))
	require.Equal(t, map[string]any{"type": "array", "length": int64(32000)},
		plainValue(gguf.Value{Type: gguf.TypeArray, Len: 32000, Items: []any{"a", "b"}}), "a shortened array is summarised")
}
