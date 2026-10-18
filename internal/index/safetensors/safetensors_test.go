package safetensors_test

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/index/rangeio"
	"github.com/hangar-hub/hangar/internal/index/safetensors"
)

func tensor(name, dtype string, shape ...int64) safetensors.Tensor {
	return safetensors.Tensor{Name: name, DType: dtype, Shape: shape}
}

func TestReadCountsParametersAndValidates(t *testing.T) {
	file, err := safetensors.Encode(map[string]string{"format": "pt"}, []safetensors.Tensor{
		tensor("a", "F32", 2, 3), tensor("b", "BF16", 4), tensor("c", "I64", 1), tensor("d", "F4", 2, 2),
	})
	require.NoError(t, err)
	h, err := safetensors.Read(bytes.NewReader(file), int64(len(file)))
	require.NoError(t, err)
	require.Equal(t, map[string]string{"format": "pt"}, h.Metadata)
	require.Len(t, h.Tensors, 4)
	by, total := h.Parameters()
	require.Equal(t, map[string]int64{"F32": 6, "BF16": 4, "I64": 1, "F4": 4}, by)
	require.Equal(t, int64(15), total)
	require.Equal(t, "F32", h.DominantDType())
	require.NoError(t, h.Validate(int64(len(file))))
	require.Equal(t, int64(8)+h.HeaderSize, h.DataStart())
	require.True(t, safetensors.KnownDType("F8_E4M3"))
	require.False(t, safetensors.KnownDType("Q4"))
}

func TestReadThroughRangedReader(t *testing.T) {
	file, err := safetensors.Encode(nil, []safetensors.Tensor{tensor("w", "F16", 1024, 1024)})
	require.NoError(t, err)
	r := rangeio.FromBytes(file)
	h, err := safetensors.Read(r, int64(len(file)))
	require.NoError(t, err)
	require.Equal(t, int64(1024*1024), h.Tensors[0].Elements())
	require.Less(t, r.Fetched, int64(len(file)), "only the header windows are read")
}

func header(t *testing.T, json string, dataSize int) []byte {
	t.Helper()
	out := make([]byte, 8)
	binary.LittleEndian.PutUint64(out, uint64(len(json)))
	out = append(out, json...)
	return append(out, make([]byte, dataSize)...)
}

func TestValidateFindsIntegrityProblems(t *testing.T) {
	cases := map[string]struct {
		json string
		data int
		want string
	}{
		"unknown dtype":  {`{"a":{"dtype":"Q9","shape":[1],"data_offsets":[0,1]}}`, 1, "unknown dtype"},
		"wrong length":   {`{"a":{"dtype":"F32","shape":[2],"data_offsets":[0,4]}}`, 4, "need 8"},
		"past the end":   {`{"a":{"dtype":"F32","shape":[2],"data_offsets":[0,8]}}`, 4, "past the data section"},
		"overlap":        {`{"a":{"dtype":"F32","shape":[1],"data_offsets":[0,4]},"b":{"dtype":"F32","shape":[1],"data_offsets":[2,6]}}`, 6, "overlaps"},
		"gap":            {`{"a":{"dtype":"F32","shape":[1],"data_offsets":[4,8]}}`, 8, "belong to no tensor"},
		"trailing bytes": {`{"a":{"dtype":"F32","shape":[1],"data_offsets":[0,4]}}`, 9, "data section has 9"},
		"reversed":       {`{"a":{"dtype":"F32","shape":[0],"data_offsets":[4,0]}}`, 4, "data_offsets"},
		"partial bytes":  {`{"a":{"dtype":"F4","shape":[1],"data_offsets":[0,1]}}`, 1, "whole bytes"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			file := header(t, c.json, c.data)
			h, err := safetensors.Read(bytes.NewReader(file), int64(len(file)))
			require.NoError(t, err)
			err = h.Validate(int64(len(file)))
			require.ErrorIs(t, err, safetensors.ErrInvalid)
			require.ErrorContains(t, err, c.want)
		})
	}
	h, err := safetensors.Parse([]byte(`{}`))
	require.NoError(t, err)
	require.ErrorContains(t, h.Validate(1), "shorter than its header")
}

func TestReadRejectsMalformedHeaders(t *testing.T) {
	cases := map[string][]byte{
		"too short":      {1, 2},
		"zero length":    make([]byte, 16),
		"huge length":    {0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f},
		"not an object":  header(t, `[1]`, 0),
		"not json":       header(t, `{"a":`, 0),
		"bad metadata":   header(t, `{"__metadata__":{"a":1}}`, 0),
		"missing fields": header(t, `{"a":{"dtype":"F32"}}`, 0),
		"bad tensor":     header(t, `{"a":"x"}`, 0),
		"negative dim":   header(t, `{"a":{"dtype":"F32","shape":[-1],"data_offsets":[0,0]}}`, 0),
		"overflow":       header(t, `{"a":{"dtype":"F32","shape":[4294967296,4294967296],"data_offsets":[0,0]}}`, 0),
		"longer header":  append([]byte{100, 0, 0, 0, 0, 0, 0, 0}, []byte("{}")...),
	}
	for name, file := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := safetensors.Read(bytes.NewReader(file), int64(len(file)))
			require.ErrorIs(t, err, safetensors.ErrInvalid)
		})
	}
	_, err := safetensors.Encode(nil, []safetensors.Tensor{tensor("x", "Q9", 1)})
	require.Error(t, err)
}

func FuzzRead(f *testing.F) {
	seed, _ := safetensors.Encode(map[string]string{"k": "v"}, []safetensors.Tensor{tensor("a", "F32", 2), tensor("b", "U8", 3)})
	f.Add(seed)
	f.Add([]byte{2, 0, 0, 0, 0, 0, 0, 0, '{', '}'})
	f.Add([]byte(`{"a":{"dtype":"F32","shape":[1],"data_offsets":[0,4]}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		h, err := safetensors.Read(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			require.ErrorIs(t, err, safetensors.ErrInvalid)
			return
		}
		_, _ = h.Parameters()
		if err := h.Validate(int64(len(data))); err != nil {
			require.ErrorIs(t, err, safetensors.ErrInvalid)
		}
	})
}
