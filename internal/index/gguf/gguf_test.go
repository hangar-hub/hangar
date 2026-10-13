package gguf_test

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/index/gguf"
	"github.com/jamesagarside/hangar/internal/index/rangeio"
)

func sample(t testing.TB, extra ...gguf.KV) []byte {
	t.Helper()
	tokens := make([]string, 100)
	for i := range tokens {
		tokens[i] = "tok" + strings.Repeat("x", i%7)
	}
	tokens[1], tokens[2] = "<s>", "</s>"
	kvs := append([]gguf.KV{
		{Key: "general.architecture", Value: "llama"},
		{Key: "general.file_type", Value: uint32(15)},
		{Key: "general.alignment", Value: uint32(64)},
		{Key: "llama.context_length", Value: uint32(8192)},
		{Key: "llama.rope.freq_base", Value: float32(10000)},
		{Key: "general.flag", Value: true},
		{Key: "general.count", Value: uint64(7)},
		{Key: "general.signed", Value: int64(-3)},
		{Key: "general.small", Value: uint8(2)},
		{Key: "general.i32", Value: int32(-4)},
		{Key: "general.ints", Value: []int32{1, 2, 3}},
		{Key: "tokenizer.ggml.tokens", Value: tokens},
		{Key: "tokenizer.ggml.merges", Value: tokens},
		{Key: "tokenizer.ggml.scores", Value: make([]float32, 100)},
		{Key: "tokenizer.ggml.bos_token_id", Value: uint32(1)},
		{Key: "tokenizer.ggml.eos_token_id", Value: uint32(2)},
		{Key: "tokenizer.chat_template", Value: "{{ messages }}"},
	}, extra...)
	b, err := gguf.Encode(kvs, []gguf.EncodeTensor{
		{Name: "token_embd.weight", Type: "Q4_K", Dims: []uint64{256, 4}},
		{Name: "output_norm.weight", Type: "F32", Dims: []uint64{256}},
	})
	require.NoError(t, err)
	return b
}

func TestReadHeader(t *testing.T) {
	file := sample(t)
	f, err := gguf.Read(bytes.NewReader(file), int64(len(file)))
	require.NoError(t, err)
	require.Equal(t, uint32(3), f.Version)
	require.Equal(t, "llama", f.Architecture())
	require.Equal(t, int64(8192), f.ContextLength())
	require.Equal(t, "{{ messages }}", f.ChatTemplate())
	require.Equal(t, "<s>", f.BOSToken())
	require.Equal(t, "</s>", f.EOSToken())
	n, name := f.FileType()
	require.Equal(t, 15, n)
	require.Equal(t, "Q4_K_M", name)
	require.Equal(t, uint64(64), f.Alignment)
	require.Equal(t, int64(256*4+256), f.Parameters())
	require.Equal(t, map[string]int64{"Q4_K": 1024, "F32": 256}, f.ParametersByType())
	require.Len(t, f.Tensors, 2)
	require.Equal(t, "Q4_K", f.Tensors[0].TypeName())
	require.Zero(t, f.DataOffset%64)
	tokens, ok := f.Get("tokenizer.ggml.tokens")
	require.True(t, ok)
	require.Equal(t, int64(100), tokens.Len)
	require.Len(t, tokens.Items, gguf.KeepArray)
	scores, _ := f.Get("tokenizer.ggml.scores")
	require.Len(t, scores.Items, gguf.KeepArray)
	require.Equal(t, int64(100), scores.Len)
	count, ok := f.Int("general.count")
	require.True(t, ok)
	require.Equal(t, int64(7), count)
	signed, _ := f.Int("general.signed")
	require.Equal(t, int64(-3), signed)
	_, ok = f.Int("general.flag")
	require.False(t, ok)
	_, ok = f.Int("missing")
	require.False(t, ok)
	no, total := f.Split()
	require.Zero(t, no+total)
	require.Contains(t, f.SortedKeys(), "general.architecture")
	require.NoError(t, f.Validate(int64(len(file))))
}

func TestReadFetchesOnlyHeaderWindows(t *testing.T) {
	head := sample(t)
	file := append(head, make([]byte, 8<<20)...)
	ranged := rangeio.FromBytes(file)
	f, err := gguf.Read(ranged, int64(len(file)))
	require.NoError(t, err)
	require.Equal(t, "llama", f.Architecture())
	require.LessOrEqual(t, ranged.Fetched, int64(rangeio.DefaultWindow), "the header fits in one window")
}

func TestValidateFindsBadTensors(t *testing.T) {
	file := sample(t)
	f, err := gguf.Read(bytes.NewReader(file), int64(len(file)))
	require.NoError(t, err)
	require.ErrorContains(t, f.Validate(int64(len(file))-1), "runs past the end")
	require.ErrorContains(t, f.Validate(f.DataOffset-1), "header runs past")
	f.Tensors[1].Offset++
	require.ErrorContains(t, f.Validate(int64(len(file))), "not aligned")
	f.Tensors[1].Offset--
	f.Tensors[0].Type = 999
	require.ErrorContains(t, f.Validate(int64(len(file))), "unknown type")
	f.Tensors[0].Type = 12 // Q4_K needs 256-element blocks.
	f.Tensors[0].Dims = []uint64{3}
	require.ErrorContains(t, f.Validate(int64(len(file))), "does not fit")
}

func le(parts ...any) []byte {
	var b bytes.Buffer
	for _, p := range parts {
		if s, ok := p.(string); ok {
			b.WriteString(s)
			continue
		}
		_ = binary.Write(&b, binary.LittleEndian, p)
	}
	return b.Bytes()
}

func TestReadRejectsMalformedFiles(t *testing.T) {
	cases := map[string][]byte{
		"empty":           nil,
		"bad magic":       []byte("GGML\x03\x00\x00\x00"),
		"bad version":     le("GGUF", uint32(9), uint64(0), uint64(0)),
		"truncated":       le("GGUF", uint32(3)),
		"too many counts": le("GGUF", uint32(3), uint64(1<<40), uint64(0)),
		"long string":     le("GGUF", uint32(3), uint64(0), uint64(1), uint64(1<<40)),
		"bad value type":  le("GGUF", uint32(3), uint64(0), uint64(1), uint64(1), "k", uint32(77), uint64(0), uint64(0)),
		"nested array":    le("GGUF", uint32(3), uint64(0), uint64(1), uint64(1), "k", uint32(9), uint32(9), uint64(1), uint64(0)),
		"huge array":      le("GGUF", uint32(3), uint64(0), uint64(1), uint64(1), "k", uint32(9), uint32(4), uint64(1<<50), uint64(0)),
		"bad array type":  le("GGUF", uint32(3), uint64(0), uint64(1), uint64(1), "k", uint32(9), uint32(77), uint64(1), uint64(0)),
		"duplicate key":   le("GGUF", uint32(3), uint64(0), uint64(2), uint64(1), "k", uint32(4), uint32(1), uint64(1), "k", uint32(4), uint32(2), uint64(0)),
		"bad alignment":   le("GGUF", uint32(3), uint64(0), uint64(1), uint64(17), "general.alignment", uint32(4), uint32(3), uint64(0), uint64(0)),
		"zero dims":       le("GGUF", uint32(3), uint64(1), uint64(0), uint64(1), "t", uint32(0), uint32(0), uint64(0), uint64(0), uint64(0)),
		"array past end":  le("GGUF", uint32(3), uint64(0), uint64(1), uint64(1), "k", uint32(9), uint32(4), uint64(6), uint32(1), uint32(2), uint32(3), uint32(4), uint32(5)),
	}
	for name, file := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := gguf.Read(bytes.NewReader(file), int64(len(file)))
			require.ErrorIs(t, err, gguf.ErrInvalid)
		})
	}
}

func TestReadVersionOneAndBigEndian(t *testing.T) {
	v1 := le("GGUF", uint32(1), uint32(1), uint32(1), uint32(20), "general.architecture", uint32(8), uint32(4), "test",
		uint32(1), "t", uint32(1), uint32(32), uint32(0), uint64(0))
	v1 = append(v1, make([]byte, 256)...)
	f, err := gguf.Read(bytes.NewReader(v1), int64(len(v1)))
	require.NoError(t, err)
	require.Equal(t, uint32(1), f.Version)
	require.Equal(t, "test", f.Architecture())
	require.Equal(t, []uint64{32}, f.Tensors[0].Dims)

	var b bytes.Buffer
	b.WriteString("FUGG")
	_ = binary.Write(&b, binary.BigEndian, uint32(3))
	_ = binary.Write(&b, binary.BigEndian, uint64(0))
	_ = binary.Write(&b, binary.BigEndian, uint64(0))
	f, err = gguf.Read(bytes.NewReader(b.Bytes()), int64(b.Len()))
	require.NoError(t, err)
	require.True(t, f.BigEndian)
}

func TestEncodeRejectsUnknownTypes(t *testing.T) {
	_, err := gguf.Encode([]gguf.KV{{Key: "k", Value: struct{}{}}}, nil)
	require.Error(t, err)
	_, err = gguf.Encode(nil, []gguf.EncodeTensor{{Name: "t", Type: "NOPE", Dims: []uint64{1}}})
	require.Error(t, err)
	_, err = gguf.Encode(nil, []gguf.EncodeTensor{{Name: "t", Type: "Q4_0", Dims: []uint64{3}}})
	require.Error(t, err)
	typ, ok := gguf.GGMLTypeByName("Q8_0")
	require.True(t, ok)
	require.Equal(t, uint32(8), typ)
}

func FuzzRead(f *testing.F) {
	f.Add(sample(f))
	f.Add(le("GGUF", uint32(3), uint64(0), uint64(1), uint64(1), "k", uint32(8), uint64(1), "v"))
	f.Add(le("GGUF", uint32(1), uint32(0), uint32(0)))
	f.Fuzz(func(t *testing.T, data []byte) {
		g, err := gguf.Read(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			require.ErrorIs(t, err, gguf.ErrInvalid)
			return
		}
		_ = g.Parameters()
		_, _ = g.FileType()
		_ = g.BOSToken()
		if err := g.Validate(int64(len(data))); err != nil {
			require.ErrorIs(t, err, gguf.ErrInvalid)
		}
	})
}
