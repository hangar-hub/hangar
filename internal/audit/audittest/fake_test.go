package audittest_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/audit"
	"github.com/hangar-hub/hangar/internal/audit/audittest"
	"github.com/hangar-hub/hangar/internal/model"
)

func TestFakeRecordsAndLists(t *testing.T) {
	ctx := context.Background()
	f := audittest.NewFake()
	alice := model.Principal{Kind: model.PrincipalUser, Username: "alice"}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, f.Record(ctx, audit.Event{Action: audit.LoginFailed, Actor: model.Anonymous(), IP: "1.2.3.4", Time: t0}))
	require.NoError(t, f.Record(ctx, audit.Event{Action: audit.LoginSucceeded, Actor: alice, Time: t0.Add(time.Hour)}))
	require.NoError(t, f.Record(ctx, audit.Event{Action: audit.RepoCreated, Actor: alice, TargetType: "repo", TargetID: "model:alice/x"}))
	require.Error(t, f.Record(ctx, audit.Event{}))
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	require.Error(t, f.Record(cancelled, audit.Event{Action: audit.Logout}))

	require.Equal(t, []audit.Action{audit.LoginFailed, audit.LoginSucceeded, audit.RepoCreated}, f.Actions())
	require.False(t, f.Events()[2].Time.IsZero(), "time defaults to now")

	byAlice, _, err := f.List(ctx, audit.Filter{ActorUsername: "alice"})
	require.NoError(t, err)
	require.Len(t, byAlice, 2)
	require.Equal(t, audit.RepoCreated, byAlice[0].Action, "newest first")
	logins, _, err := f.List(ctx, audit.Filter{Actions: []audit.Action{audit.LoginFailed, audit.LoginSucceeded}, Until: t0.Add(time.Minute)})
	require.NoError(t, err)
	require.Len(t, logins, 1)
	page, next, err := f.List(ctx, audit.Filter{Limit: 2, Since: t0})
	require.NoError(t, err)
	require.Len(t, page, 2)
	rest, _, err := f.List(ctx, audit.Filter{Cursor: next})
	require.NoError(t, err)
	require.Len(t, rest, 1)
	_, _, err = f.List(ctx, audit.Filter{Cursor: "-1"})
	require.Error(t, err)
}
