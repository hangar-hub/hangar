package audit_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/jamesagarside/hangar/internal/audit"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/model"
)

// memExporter keeps exported log records.
type memExporter struct {
	mu      sync.Mutex
	records []sdklog.Record
}

func (m *memExporter) Export(_ context.Context, rs []sdklog.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range rs {
		m.records = append(m.records, r.Clone())
	}
	return nil
}
func (m *memExporter) Shutdown(context.Context) error   { return nil }
func (m *memExporter) ForceFlush(context.Context) error { return nil }

func insertUser(t *testing.T, d *db.DB, name string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	ns := &db.Namespace{ID: db.NewID(), Name: name, Kind: "user", DisplayName: name}
	_, err := d.NewInsert().Model(ns).Exec(ctx)
	require.NoError(t, err)
	u := &db.User{ID: db.NewID(), NamespaceID: ns.ID}
	_, err = d.NewInsert().Model(u).Exec(ctx)
	require.NoError(t, err)
	return u.ID
}

func TestDBRecorder(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		exp := &memExporter{}
		lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exp)))
		r := audit.NewDB(d, lp)
		base := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
		now := base
		r.SetClock(func() time.Time { return now })

		aliceID := insertUser(t, d, "alice")
		tokID := uuid.New()
		alice := model.Principal{Kind: model.PrincipalUser, UserID: aliceID, Username: "alice",
			Token: &model.TokenInfo{ID: tokID}}

		require.NoError(t, r.Record(ctx, audit.Event{Action: audit.LoginFailed, Actor: model.Anonymous(), TargetType: "user", TargetID: "alice", IP: "10.0.0.1"}))
		for i := range 5 {
			now = base.Add(time.Duration(i+1) * time.Minute)
			require.NoError(t, r.Record(ctx, audit.Event{Action: audit.TokenCreated, Actor: alice, TargetType: "token", TargetID: "t",
				UserAgent: "ua", Data: map[string]any{"name": "laptop", "n": i}}))
		}
		require.NoError(t, r.Record(ctx, audit.Event{Action: audit.SettingsChanged, Actor: model.System(), Time: base.Add(time.Hour)}))
		require.Error(t, r.Record(ctx, audit.Event{}), "an action is required")
		require.ErrorContains(t, r.Record(ctx, audit.Event{Action: audit.UserCreated, Data: map[string]any{"password": "x"}}), "looks like a secret")
		require.ErrorContains(t, r.Record(ctx, audit.Event{Action: audit.UserCreated, Data: map[string]any{"Token": "x"}}), "looks like a secret")
		require.NoError(t, r.Record(ctx, audit.Event{Action: audit.TokenRevoked, Data: map[string]any{"token_name": "laptop"}}), "names are fine")

		all, next, err := r.List(ctx, audit.Filter{})
		require.NoError(t, err)
		require.Empty(t, next)
		require.Len(t, all, 8)
		require.Equal(t, audit.SettingsChanged, all[0].Action, "newest first")
		require.Equal(t, audit.TokenRevoked, all[1].Action, "ties on time are ordered by ID")
		require.Equal(t, audit.LoginFailed, all[7].Action)
		require.True(t, all[7].Actor.IsAnonymous())
		require.Equal(t, "10.0.0.1", all[7].IP)
		tc := all[2]
		require.Equal(t, "alice", tc.ActorUsername)
		require.Equal(t, aliceID, tc.Actor.UserID)
		require.Equal(t, tokID, tc.Actor.Token.ID)
		require.Equal(t, "laptop", tc.Data["name"])
		require.Equal(t, base.Add(5*time.Minute), tc.Time)

		page, next, err := r.List(ctx, audit.Filter{ActorUsername: "ALICE", Limit: 2})
		require.NoError(t, err)
		require.Len(t, page, 2)
		require.NotEmpty(t, next)
		seen := map[string]bool{page[0].ID: true, page[1].ID: true}
		for next != "" {
			page, next, err = r.List(ctx, audit.Filter{ActorUsername: "alice", Limit: 2, Cursor: next})
			require.NoError(t, err)
			for _, e := range page {
				require.False(t, seen[e.ID], "pages do not overlap")
				seen[e.ID] = true
			}
		}
		require.Len(t, seen, 5)

		rng, _, err := r.List(ctx, audit.Filter{Actions: []audit.Action{audit.TokenCreated}, Since: base.Add(2 * time.Minute), Until: base.Add(4 * time.Minute)})
		require.NoError(t, err)
		require.Len(t, rng, 2, "since is inclusive, until exclusive")
		_, _, err = r.List(ctx, audit.Filter{Cursor: "garbage"})
		require.ErrorIs(t, err, audit.ErrBadCursor)

		// Every event is also an OpenTelemetry log record named
		// hangar.audit.<action>.
		exp.mu.Lock()
		defer exp.mu.Unlock()
		require.Len(t, exp.records, 8)
		rec := exp.records[1]
		require.Equal(t, "hangar.audit.token.created", rec.EventName())
		attrs := map[string]string{}
		rec.WalkAttributes(func(kv attribute.KeyValue) bool {
			attrs[string(kv.Key)] = kv.Value.String()
			return true
		})
		require.Equal(t, "token.created", attrs["hangar.audit.action"])
		require.Equal(t, "alice", attrs["hangar.audit.actor"])
		require.Equal(t, aliceID.String(), attrs["enduser.id"])
		require.Equal(t, tokID.String(), attrs["hangar.audit.actor_token_id"])
		require.Equal(t, "token", attrs["hangar.audit.target_type"])
		require.Equal(t, "laptop", attrs["hangar.audit.data.name"])
		require.Equal(t, "ua", attrs["user_agent.original"])
		require.Equal(t, base.Add(time.Minute), rec.Timestamp())
		require.Equal(t, "hangar.audit.login.failed", exp.records[0].EventName())
	})
}

func TestDBRecorderWithoutLogs(t *testing.T) {
	d := dbtest.SQLite(t)
	r := audit.NewDB(d, nil)
	require.NoError(t, r.Record(context.Background(), audit.Event{Action: audit.Logout}))
	es, _, err := r.List(context.Background(), audit.Filter{Limit: 1000})
	require.NoError(t, err)
	require.Len(t, es, 1)
}
