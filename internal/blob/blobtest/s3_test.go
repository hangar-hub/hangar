package blobtest_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/blob/blobtest"
)

// TestWrapperStartsMinIOImage checks that scripts/with-test-minio.sh, which
// `make test` runs the Go tests through, starts the same MinIO image as
// S3TestServer does with testcontainers, so both ways of running the S3
// tests test the same server.
func TestWrapperStartsMinIOImage(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "scripts", "with-test-minio.sh"))
	require.NoError(t, err)
	want := `image="${` + blobtest.EnvMinIOImage + `:-` + blobtest.MinIOImage + `}"`
	require.True(t, strings.Contains(string(b), want), "scripts/with-test-minio.sh sets %s", want)
}
