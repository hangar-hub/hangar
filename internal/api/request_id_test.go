package api_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/api"
	"github.com/jamesagarside/hangar/internal/org"
	"github.com/jamesagarside/hangar/internal/telemetry"
)

type failingNamespaceSearch struct{ err error }

func (f failingNamespaceSearch) SearchNamespaces(context.Context, string, int) ([]org.Namespace, error) {
	return nil, f.err
}

// A 500 names the request ID, and the log line with the cause carries the
// same ID, so a user can quote the one and an operator can find the other.
func TestInternalErrorNamesTheRequestIDTheLogCarries(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	f := newFixture(t, func(d *api.RepoDeps) {
		d.Log = slog.New(telemetry.NewStdoutHandler(&logs, "json", slog.LevelInfo))
		d.NamespaceSearch = failingNamespaceSearch{err: errors.New("namespace index unreachable")}
	})
	f.user("alice")

	r := f.expect(http.StatusInternalServerError, http.MethodGet, "alice", "/search?q=x&types=namespaces", nil)
	id := r.header.Get("X-Request-Id")
	require.NotEmpty(t, id)
	var problem struct {
		Detail    string `json:"detail"`
		RequestID string `json:"request_id"`
	}
	r.json(t, &problem)
	require.Equal(t, "Internal server error. The request ID is "+id+".", problem.Detail)
	require.Equal(t, id, problem.RequestID)
	require.NotContains(t, problem.Detail, "unreachable", "the cause stays in the log")

	var found bool
	sc := bufio.NewScanner(&logs)
	for sc.Scan() {
		var line map[string]any
		require.NoError(t, json.Unmarshal(sc.Bytes(), &line), sc.Text())
		if line["msg"] != "native api request failed" {
			continue
		}
		found = true
		require.Equal(t, id, line["request_id"])
		require.Equal(t, "namespace index unreachable", line["error"])
	}
	require.True(t, found, "no log line for the failure in:\n%s", logs.String())
}
