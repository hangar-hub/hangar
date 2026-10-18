package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/api/gen"
	"github.com/hangar-hub/hangar/internal/index/indextest"
	"github.com/hangar-hub/hangar/internal/scan"
	"github.com/hangar-hub/hangar/internal/scan/scantest"
)

// TestFileScan checks GET .../files/{rev}/{path}/scan, which lists the
// imports the pickle scanner found in one file for the file page.
func TestFileScan(t *testing.T) {
	t.Parallel()
	e := newMetaEnv(t)
	ctx := context.Background()
	sha := indextest.Commit(t, e.repos, e.model.ID, map[string][]byte{"evil.pkl": scantest.OSSystemProtocol0()})
	base := "/repos/models/alice/tiny/files/" + string(sha)

	var fs gen.FileScan
	e.json("GET", base+"/evil.pkl/scan", "bob-token", nil, http.StatusOK, &fs)
	require.Equal(t, gen.FileScanStatus("pending"), fs.Status, "a Commit that is not scanned yet is pending")
	require.Empty(t, fs.Findings)

	_, err := scan.New(scan.Options{DB: e.db, Repos: e.repos}).ScanCommit(ctx, e.model.UUID, e.model.ID, sha)
	require.NoError(t, err)
	e.json("GET", base+"/evil.pkl/scan", "bob-token", nil, http.StatusOK, &fs)
	require.Equal(t, gen.FileScanStatus("unsafe"), fs.Status)
	require.Equal(t, "evil.pkl", fs.Path)
	require.NotEmpty(t, fs.Findings)
	f := fs.Findings[0]
	require.Equal(t, "os", *f.Module)
	require.Equal(t, "system", *f.Name)
	require.Equal(t, "GLOBAL", *f.Opcode)
	require.Equal(t, gen.ScanFindingStatus("unsafe"), f.Status)

	// A later Commit whose scan is pending keeps the findings of the
	// unchanged file, so the file page lists them straight away.
	next := indextest.Commit(t, e.repos, e.model.ID, map[string][]byte{"other.txt": []byte("other")})
	fs = gen.FileScan{}
	e.json("GET", "/repos/models/alice/tiny/files/"+string(next)+"/evil.pkl/scan", "bob-token", nil, http.StatusOK, &fs)
	require.Equal(t, gen.FileScanStatus("unsafe"), fs.Status)
	require.NotEmpty(t, fs.Findings)
	require.Equal(t, "os", *fs.Findings[0].Module)
	fs = gen.FileScan{}
	e.json("GET", "/repos/models/alice/tiny/files/"+string(next)+"/other.txt/scan", "bob-token", nil, http.StatusOK, &fs)
	require.Equal(t, gen.FileScanStatus("pending"), fs.Status, "a file with no carried result is pending until the scan runs")

	e.json("GET", base+"/notes.txt/scan", "bob-token", nil, http.StatusOK, &fs)
	require.Equal(t, gen.FileScanStatus("safe"), fs.Status, "a file the scan does not check is safe")
	require.Empty(t, fs.Findings)

	status, _ := e.do("GET", base+"/missing.pkl/scan", "bob-token", nil)
	require.Equal(t, http.StatusNotFound, status)
	status, _ = e.do("GET", "/repos/models/alice/tiny/files/main/%2e%2e/scan", "bob-token", nil)
	require.Contains(t, []int{http.StatusBadRequest, http.StatusNotFound}, status)
	status, _ = e.do("GET", "/repos/models/alice/missing/files/main/evil.pkl/scan", "", nil)
	require.Equal(t, http.StatusUnauthorized, status)
}
