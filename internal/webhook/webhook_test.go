package webhook_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/webhook"
)

func TestSignAndVerify(t *testing.T) {
	secret := []byte("s3cret")
	body := []byte(`{"event":"repo.created"}`)
	sig := webhook.Sign(secret, body)
	require.Regexp(t, `^sha256=[0-9a-f]{64}$`, sig)
	require.True(t, webhook.Verify(secret, body, sig))
	require.False(t, webhook.Verify([]byte("other"), body, sig))
	require.False(t, webhook.Verify(secret, []byte("{}"), sig))
	// Known value from: printf '%s' '{"event":"repo.created"}' | openssl dgst -sha256 -hmac s3cret
	require.Equal(t, "sha256=4ade32eb32bfb30749cba20bcd8b36e15b302dba429c22c25090fa67834c1c6e", sig)
	require.Len(t, webhook.AllEvents(), 16)
	require.Len(t, webhook.RetrySchedule, 5)
}

func TestNewEvent(t *testing.T) {
	repo := model.RepoID{Type: model.RepoTypeDataset, Namespace: "ns", Name: "d"}
	repoUUID, nsID := uuid.New(), uuid.New()
	actor := model.Principal{Kind: model.PrincipalUser, UserID: uuid.New(), Username: "alice"}
	e := webhook.NewEvent(webhook.ScanCompleted, actor, &repo, repoUUID, nsID, map[string]any{"status": "safe"})
	require.Equal(t, webhook.ScanCompleted, e.Event)
	require.Equal(t, uuid.Version(7), e.ID.Version())
	require.Equal(t, "alice", e.Actor.Username)
	require.Equal(t, &webhook.RepoRef{ID: "ns/d", Type: "dataset", UUID: repoUUID.String()}, e.Repo)
	require.Equal(t, nsID, e.NamespaceID)
	require.WithinDuration(t, time.Now(), e.OccurredAt, time.Minute)

	anon := webhook.NewEvent(webhook.PullStarted, model.Anonymous(), nil, uuid.Nil, uuid.Nil, nil)
	require.Empty(t, anon.Actor)
	require.Nil(t, anon.Repo)
	require.NotNil(t, anon.Data)
}
