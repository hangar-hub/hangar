package blob_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/blob"
)

func TestRangeHelpers(t *testing.T) {
	for _, tc := range []struct {
		header string
		size   int64
		want   *blob.Range
		err    bool
	}{
		{"", 10, nil, false},
		{"bytes=0-0", 10, &blob.Range{Start: 0, End: 0}, false},
		{"bytes=2-", 10, &blob.Range{Start: 2, End: -1}, false},
		{"bytes=-3", 10, &blob.Range{Start: 7, End: 9}, false},
		{"bytes=-30", 10, &blob.Range{Start: 0, End: 9}, false},
		{"bytes=5-100", 10, &blob.Range{Start: 5, End: 100}, false},
		{"bytes=10-", 10, nil, true},
		{"bytes=5-2", 10, nil, true},
		{"bytes=0-1,3-4", 10, nil, true},
		{"items=0-1", 10, nil, true},
		{"bytes=-0", 10, nil, true},
		{"bytes=-1", 0, nil, true},
		{"bytes=x-", 10, nil, true},
		{"bytes=1", 10, nil, true},
	} {
		got, err := blob.ParseRange(tc.header, tc.size)
		if tc.err {
			require.ErrorIs(t, err, blob.ErrInvalidRange, tc.header)
			continue
		}
		require.NoError(t, err, tc.header)
		require.Equal(t, tc.want, got, tc.header)
	}
	r := blob.Range{Start: 5, End: -1}
	require.Equal(t, int64(5), r.Length(10))
	require.Equal(t, "bytes 5-9/10", r.ContentRange(10))
	require.Equal(t, "bytes */3", r.ContentRange(3))
	require.Equal(t, int64(0), blob.Range{Start: 9, End: 3}.Length(10))
	require.NoError(t, blob.ValidateKey("lfs/sha256/aa/bb/cc"))
	require.ErrorIs(t, blob.ValidateKey("a/./b"), blob.ErrInvalidKey)
}

func TestContentDisposition(t *testing.T) {
	require.Equal(t, "attachment; filename=model.gguf", blob.ContentDisposition("model.gguf"))
	require.Contains(t, blob.ContentDisposition("naïve.gguf"), "filename*=utf-8''na%C3%AFve.gguf")
	require.Equal(t, "attachment", blob.ContentDisposition(""), "a download without a name is still an attachment")
	require.Equal(t, `attachment; filename="a \"b\".html"`, blob.ContentDisposition(`a "b".html`))
}

func TestSetDownloadHeadersOverridesCallerValues(t *testing.T) {
	h := http.Header{}
	h.Set("Content-Type", "image/svg+xml")
	h.Set("Content-Disposition", "inline")
	blob.SetDownloadHeaders(h, "logo.svg")
	require.Equal(t, blob.SafeContentType, h.Get("Content-Type"))
	require.Equal(t, "attachment; filename=logo.svg", h.Get("Content-Disposition"))
	require.Equal(t, "nosniff", h.Get("X-Content-Type-Options"))
}
