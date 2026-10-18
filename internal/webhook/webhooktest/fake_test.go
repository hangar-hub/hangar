package webhooktest_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/webhook"
	"github.com/hangar-hub/hangar/internal/webhook/webhooktest"
)

func TestFakeRecords(t *testing.T) {
	ctx := context.Background()
	f := &webhooktest.Fake{}
	repo := model.RepoID{Type: model.RepoTypeModel, Namespace: "ns", Name: "n"}
	actor := model.Principal{Kind: model.PrincipalUser, UserID: uuid.New(), Username: "alice"}
	require.NoError(t, f.Emit(ctx, webhook.NewEvent(webhook.RepoCreated, actor, &repo, uuid.New(), uuid.New(), nil)))
	require.NoError(t, f.Emit(ctx, webhook.NewEvent(webhook.PullStarted, model.Anonymous(), nil, uuid.Nil, uuid.Nil, map[string]any{"files": 2})))
	require.Error(t, f.Emit(ctx, webhook.Event{}))
	require.Equal(t, []webhook.EventName{webhook.RepoCreated, webhook.PullStarted}, f.Names())

	b, err := json.Marshal(f.Events()[0])
	require.NoError(t, err)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(b, &payload))
	require.Equal(t, "repo.created", payload["event"])
	require.Equal(t, "ns/n", payload["repo"].(map[string]any)["id"])
	require.Equal(t, "alice", payload["actor"].(map[string]any)["username"])
	require.NotContains(t, payload, "NamespaceID")
	require.Empty(t, f.Events()[1].Actor.Username)

	f.Err = errors.New("down")
	require.Error(t, f.Emit(ctx, webhook.NewEvent(webhook.PRMerged, actor, nil, uuid.Nil, uuid.Nil, nil)))
}
