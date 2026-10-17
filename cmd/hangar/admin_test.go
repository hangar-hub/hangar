package main

import (
	"context"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/config"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
)

// closedPort returns a local address nothing listens on.
func closedPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return addr
}

func postgresConfigAt(t *testing.T, dsn string) *config.Config {
	t.Helper()
	cfg := testConfig(t)
	cfg.Database.Driver = db.Postgres
	cfg.Database.DSN = dsn
	return cfg
}

// TestMigrateFailsAtOnceWithoutWaitForDatabase keeps the default: a
// database that does not answer fails hangar admin migrate straight away.
func TestMigrateFailsAtOnceWithoutWaitForDatabase(t *testing.T) {
	cfg := postgresConfigAt(t, "postgres://hangar:hangar@"+closedPort(t)+"/hangar?sslmode=disable")
	var status strings.Builder
	start := time.Now()
	_, err := migrateDatabase(context.Background(), cfg, 0, &status)
	require.Error(t, err)
	require.Less(t, time.Since(start), 5*time.Second)
	require.Empty(t, status.String())
}

// TestMigrateGivesUpWaitingForTheDatabase checks that --wait-for-database
// is a bound: a database that never answers fails the migration once it
// has passed, with the last connection error.
func TestMigrateGivesUpWaitingForTheDatabase(t *testing.T) {
	cfg := postgresConfigAt(t, "postgres://hangar:hangar@"+closedPort(t)+"/hangar?sslmode=disable")
	var status strings.Builder
	start := time.Now()
	_, err := migrateDatabase(context.Background(), cfg, 2*time.Second, &status)
	require.ErrorContains(t, err, "did not answer within 2s")
	require.ErrorContains(t, err, "connection refused")
	require.GreaterOrEqual(t, time.Since(start), 2*time.Second, "gave up before the wait was over")
	require.Less(t, time.Since(start), 10*time.Second)
	require.Contains(t, status.String(), "Waiting for the database")
}

// TestMigrateWaitsForTheDatabase starts hangar admin migrate
// --wait-for-database before its Postgres answers, as a migrate init
// container does while the database Pod starts, and opens the database's
// port a few seconds later. The migration must wait and then succeed,
// instead of exiting into the kubelet's restart back-off.
func TestMigrateWaitsForTheDatabase(t *testing.T) {
	dbtest.RequirePostgres(t)
	server, err := url.Parse(dbtest.PostgresDSN(t))
	require.NoError(t, err)
	addr := closedPort(t)
	late := *server
	late.Host = addr
	cfg := postgresConfigAt(t, late.String())

	// Forward addr to the real server once the migrator has had to wait.
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); wg.Wait() }()
	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case <-ctx.Done():
			return
		case <-time.After(2500 * time.Millisecond):
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Errorf("listen on %s: %v", addr, err)
			return
		}
		go func() { <-ctx.Done(); _ = ln.Close() }()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer c.Close()
				up, err := net.Dial("tcp", server.Host)
				if err != nil {
					return
				}
				defer up.Close()
				go func() { <-ctx.Done(); _ = c.Close(); _ = up.Close() }()
				go func() { _, _ = io.Copy(up, c) }()
				_, _ = io.Copy(c, up)
			}()
		}
	}()

	var status strings.Builder
	mctx, mcancel := context.WithTimeout(context.Background(), time.Minute)
	defer mcancel()
	st, err := migrateDatabase(mctx, cfg, 30*time.Second, &status)
	require.NoError(t, err)
	require.True(t, st.UpToDate(), "pending: %v", st.Pending)
	require.Contains(t, status.String(), "Waiting for the database")
	require.NotContains(t, status.String(), server.User.String(), "the status lines must not print credentials")
}
