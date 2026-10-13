package community_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/community"
)

func TestIsPullRequest(t *testing.T) {
	require.True(t, (&community.Discussion{Kind: community.KindPullRequest}).IsPullRequest())
	require.False(t, (&community.Discussion{Kind: community.KindDiscussion}).IsPullRequest())
}
