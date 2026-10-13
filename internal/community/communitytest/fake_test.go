package communitytest_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/community"
	"github.com/jamesagarside/hangar/internal/community/communitytest"
	"github.com/jamesagarside/hangar/internal/model"
)

func TestFakeDiscussionsAndPullRequests(t *testing.T) {
	ctx := context.Background()
	f := communitytest.NewFake()
	repo := uuid.New()
	alice := model.Principal{Kind: model.PrincipalUser, Username: "alice"}

	d, err := f.Create(ctx, community.CreateParams{RepoUUID: repo, Title: "Question", Description: "How?", Author: alice})
	require.NoError(t, err)
	require.Equal(t, 1, d.Num)
	require.Equal(t, community.StatusOpen, d.Status)
	require.Len(t, d.Events, 1)
	pr, err := f.Create(ctx, community.CreateParams{RepoUUID: repo, Title: "Add weights", PullRequest: true, Author: alice})
	require.NoError(t, err)
	require.Equal(t, "refs/pr/2", pr.PRRef)
	require.Equal(t, "main", pr.PRBase)
	require.True(t, pr.IsPullRequest())
	_, err = f.Create(ctx, community.CreateParams{RepoUUID: repo, Title: " ", Author: alice})
	require.ErrorIs(t, err, community.ErrInvalid)
	_, err = f.Create(ctx, community.CreateParams{RepoUUID: repo, Title: "x", Author: model.Anonymous()})
	require.ErrorIs(t, err, community.ErrInvalid)

	_, err = f.Comment(ctx, repo, 1, "Like this.", alice)
	require.NoError(t, err)
	_, err = f.Comment(ctx, repo, 1, "", alice)
	require.ErrorIs(t, err, community.ErrInvalid)
	_, err = f.Rename(ctx, repo, 1, "Better question", alice)
	require.NoError(t, err)
	_, err = f.Rename(ctx, repo, 1, "", alice)
	require.ErrorIs(t, err, community.ErrInvalid)
	got, err := f.Get(ctx, repo, 1)
	require.NoError(t, err)
	require.Equal(t, "Better question", got.Title)
	require.Len(t, got.Events, 3)

	_, err = f.Merge(ctx, repo, 1, "", alice)
	require.ErrorIs(t, err, community.ErrNotPullRequest)
	_, err = f.Merge(ctx, repo, 2, "Thanks", alice)
	require.NoError(t, err)
	_, err = f.Merge(ctx, repo, 2, "", alice)
	require.ErrorIs(t, err, community.ErrNotOpen)
	_, err = f.SetStatus(ctx, repo, 2, community.StatusOpen, "", alice)
	require.ErrorIs(t, err, community.ErrNotOpen)
	_, err = f.SetStatus(ctx, repo, 1, community.StatusClosed, "Answered", alice)
	require.NoError(t, err)
	_, err = f.SetStatus(ctx, repo, 1, "merged", "", alice)
	require.ErrorIs(t, err, community.ErrInvalid)

	all, total, err := f.List(ctx, repo, community.ListOptions{})
	require.NoError(t, err)
	require.Equal(t, 2, total)
	require.Equal(t, 2, all[0].Num, "newest first")
	prs, _, err := f.List(ctx, repo, community.ListOptions{Type: community.KindPullRequest})
	require.NoError(t, err)
	require.Len(t, prs, 1)
	open, _, err := f.List(ctx, repo, community.ListOptions{Status: community.StatusOpen})
	require.NoError(t, err)
	require.Empty(t, open)
	closed, _, err := f.List(ctx, repo, community.ListOptions{Status: community.StatusClosed, Author: "ALICE", Limit: 1, Page: 1})
	require.NoError(t, err)
	require.Len(t, closed, 1)
	_, err = f.Get(ctx, repo, 9)
	require.ErrorIs(t, err, community.ErrNotFound)
}

func TestFakeLikesAndDownloads(t *testing.T) {
	ctx := context.Background()
	f := communitytest.NewFake()
	repo, u1, u2 := uuid.New(), uuid.New(), uuid.New()
	require.NoError(t, f.Like(ctx, u1, repo))
	require.NoError(t, f.Like(ctx, u1, repo))
	require.NoError(t, f.Like(ctx, u2, repo))
	n, err := f.Likes(ctx, repo)
	require.NoError(t, err)
	require.Equal(t, int64(2), n, "a user likes a Repo once")
	require.NoError(t, f.Unlike(ctx, u1, repo))
	n, _ = f.Likes(ctx, repo)
	require.Equal(t, int64(1), n)

	d1 := time.Date(2026, 10, 12, 23, 0, 0, 0, time.UTC)
	require.NoError(t, f.RecordDownload(ctx, repo, d1))
	require.NoError(t, f.RecordDownload(ctx, repo, d1.Add(2*time.Hour)))
	require.NoError(t, f.RecordDownload(ctx, repo, d1.Add(3*time.Hour)))
	days, err := f.Downloads(ctx, repo, d1)
	require.NoError(t, err)
	require.Len(t, days, 2)
	require.Equal(t, int64(1), days[0].Count)
	require.Equal(t, int64(2), days[1].Count)
	recent, err := f.Downloads(ctx, repo, d1.Add(24*time.Hour))
	require.NoError(t, err)
	require.Len(t, recent, 1)
}
