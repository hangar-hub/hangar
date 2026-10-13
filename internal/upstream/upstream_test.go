package upstream_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/upstream"
)

func TestDatasetViewerURL(t *testing.T) {
	require.Equal(t, "https://datasets-server.huggingface.co", upstream.Upstream{BaseURL: "https://huggingface.co"}.DatasetViewerURL())
	require.Equal(t, "http://datasets-server.hangar.lan:8080", upstream.Upstream{BaseURL: "http://hangar.lan:8080/"}.DatasetViewerURL())
	require.Equal(t, "https://viewer.example.com", upstream.Upstream{BaseURL: "https://x", ViewerURL: "https://viewer.example.com/"}.DatasetViewerURL())
	require.Empty(t, upstream.Upstream{BaseURL: "not a url"}.DatasetViewerURL())
	require.Contains(t, (&upstream.RateLimitError{RetryAfter: 30 * time.Second}).Error(), "30s")
}
