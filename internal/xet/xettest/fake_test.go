package xettest_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/xet"
	"github.com/hangar-hub/hangar/internal/xet/xettest"
)

func TestFakeTokensXorbsAndShards(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	f := xettest.NewFake("https://hangar.example.com/xet-cas", time.Hour)
	f.SetClock(func() time.Time { return now })
	user := model.Principal{Kind: model.PrincipalUser, UserID: uuid.New(), Username: "alice"}
	repo := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "m"}

	tok, err := f.IssueWriteToken(ctx, xet.TokenRequest{Principal: user, Repo: repo, Revision: "None"})
	require.NoError(t, err)
	require.Equal(t, "https://hangar.example.com/xet-cas", tok.CASURL)
	require.Equal(t, now.Add(time.Hour).Unix(), tok.Exp)
	_, err = f.IssueWriteToken(ctx, xet.TokenRequest{Principal: model.Anonymous(), Repo: repo})
	require.ErrorIs(t, err, xet.ErrScope)

	claims, err := f.VerifyToken(ctx, tok.AccessToken)
	require.NoError(t, err)
	require.Equal(t, repo, claims.Repo)
	require.Equal(t, "None", claims.Revision)
	_, err = f.VerifyToken(ctx, "nope")
	require.ErrorIs(t, err, xet.ErrInvalidToken)

	hash := strings.Repeat("a", 64)
	inserted, err := f.PutXorb(ctx, claims, hash, bytes.NewReader([]byte("xorb")), 4)
	require.NoError(t, err)
	require.True(t, inserted)
	inserted, err = f.PutXorb(ctx, claims, hash, bytes.NewReader([]byte("xorb")), 4)
	require.NoError(t, err)
	require.False(t, inserted, "uploads are idempotent")
	b, ok := f.Xorb(hash)
	require.True(t, ok)
	require.Equal(t, "xorb", string(b))
	_, err = f.PutXorb(ctx, claims, "NOTHEX", bytes.NewReader(nil), 0)
	require.ErrorIs(t, err, xet.ErrBadXorb)
	_, err = f.PutXorb(ctx, claims, hash, bytes.NewReader(nil), xet.MaxXorbSize+1)
	require.ErrorIs(t, err, xet.ErrTooLarge)
	_, err = f.PutXorb(ctx, &xet.Claims{Scope: xet.ScopeRead}, hash, bytes.NewReader(nil), 0)
	require.ErrorIs(t, err, xet.ErrScope)

	f.SetShardFiles(xet.ReassembledFile{XetHash: hash, OID: strings.Repeat("b", 64), Size: 4})
	var events []string
	res, err := f.AcceptShard(ctx, claims, strings.NewReader("shard"), 5, func(e xet.ShardEvent) { events = append(events, e.Type) })
	require.NoError(t, err)
	require.Equal(t, 1, res.Result)
	require.Len(t, res.Files, 1)
	require.Equal(t, []string{xet.EventValidating, xet.EventCommitting, xet.EventResult}, events)
	require.Equal(t, 1, f.Shards())
	_, err = f.AcceptShard(ctx, claims, strings.NewReader(""), xet.MaxShardSize+1, nil)
	require.ErrorIs(t, err, xet.ErrTooLarge)

	// The token is valid up to its expiry, then rejected.
	now = now.Add(time.Hour)
	_, err = f.VerifyToken(ctx, tok.AccessToken)
	require.NoError(t, err)
	now = now.Add(time.Second)
	_, err = f.VerifyToken(ctx, tok.AccessToken)
	require.ErrorIs(t, err, xet.ErrInvalidToken)

	f.Reject = true
	_, err = f.IssueWriteToken(ctx, xet.TokenRequest{Principal: user, Repo: repo})
	require.ErrorIs(t, err, xet.ErrRejected)
}
