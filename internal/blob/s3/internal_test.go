package s3

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/blob"
)

func TestUnknownSizePartSize(t *testing.T) {
	require.Equal(t, blob.DefaultPartSize, unknownSizePartSize(1))
	require.Equal(t, blob.DefaultPartSize, unknownSizePartSize(2000))
	require.Equal(t, 2*blob.DefaultPartSize, unknownSizePartSize(2001))
	require.Equal(t, 8*blob.DefaultPartSize, unknownSizePartSize(10000))
	var total int64
	for n := 1; n <= blob.MaxParts; n++ {
		total += unknownSizePartSize(n)
	}
	require.Greater(t, total, int64(700)<<30, "unknown-size uploads reach over 700 GiB")
}

func TestTotalFromContentRange(t *testing.T) {
	n, ok := totalFromContentRange("bytes 3-4/10")
	require.True(t, ok)
	require.Equal(t, int64(10), n)
	_, ok = totalFromContentRange("")
	require.False(t, ok)
	_, ok = totalFromContentRange("bytes 3-4/*")
	require.False(t, ok)
}
