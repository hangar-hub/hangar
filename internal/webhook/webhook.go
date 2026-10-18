// Package webhook registers webhooks and delivers events to them, signed
// with HMAC-SHA256 and retried with backoff (EVT-1,
// docs/spec/10-native-api.md "Webhook events"). Other packages emit events
// through Emitter. webhooktest has a recording fake.
package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/google/uuid"

	"github.com/hangar-hub/hangar/internal/model"
)

// EventName is a webhook event.
type EventName string

// Events from 10-native-api.md.
const (
	RepoCreated             EventName = "repo.created"
	RepoUpdated             EventName = "repo.updated"
	RepoDeleted             EventName = "repo.deleted"
	RepoRestored            EventName = "repo.restored"
	RepoRenamed             EventName = "repo.renamed"
	RepoVisibilityChanged   EventName = "repo.visibility_changed"
	PullStarted             EventName = "pull.started"
	PullCompleted           EventName = "pull.completed"
	PullFailed              EventName = "pull.failed"
	MirrorSynced            EventName = "mirror.synced"
	IndexCompleted          EventName = "index.completed"
	ScanCompleted           EventName = "scan.completed"
	DiscussionCreated       EventName = "discussion.created"
	DiscussionCommented     EventName = "discussion.commented"
	DiscussionStatusChanged EventName = "discussion.status_changed"
	PRMerged                EventName = "pr.merged"
)

// AllEvents lists every event name.
func AllEvents() []EventName {
	return []EventName{RepoCreated, RepoUpdated, RepoDeleted, RepoRestored, RepoRenamed, RepoVisibilityChanged,
		PullStarted, PullCompleted, PullFailed, MirrorSynced, IndexCompleted, ScanCompleted,
		DiscussionCreated, DiscussionCommented, DiscussionStatusChanged, PRMerged}
}

// Delivery headers.
const (
	HeaderEvent     = "X-Hangar-Event"
	HeaderDelivery  = "X-Hangar-Delivery"
	HeaderSignature = "X-Hangar-Signature"
)

// RetrySchedule is the delay before each retry; after the last the
// delivery is marked failed.
var RetrySchedule = []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 12 * time.Hour}

// DeliveryTimeout bounds one delivery attempt.
const DeliveryTimeout = 10 * time.Second

// Actor is the payload's `actor`.
type Actor struct {
	ID       string `json:"id,omitempty"`
	Username string `json:"username,omitempty"`
}

// RepoRef is the payload's `repo`.
type RepoRef struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	UUID string `json:"uuid"`
}

// Event is one occurrence to deliver. Its JSON is the payload.
type Event struct {
	Event      EventName      `json:"event"`
	ID         uuid.UUID      `json:"id"`
	OccurredAt time.Time      `json:"occurred_at"`
	Actor      Actor          `json:"actor"`
	Repo       *RepoRef       `json:"repo,omitempty"`
	Data       map[string]any `json:"data"`
	// NamespaceID scopes delivery to Namespace webhooks; it is not sent.
	NamespaceID uuid.UUID `json:"-"`
}

// NewEvent fills in the ID, time and actor of an event about a Repo.
func NewEvent(name EventName, actor model.Principal, repo *model.RepoID, repoUUID, namespaceID uuid.UUID, data map[string]any) Event {
	e := Event{Event: name, ID: uuid.Must(uuid.NewV7()), OccurredAt: time.Now().UTC(), Data: data, NamespaceID: namespaceID}
	if e.Data == nil {
		e.Data = map[string]any{}
	}
	if !actor.IsAnonymous() {
		e.Actor = Actor{ID: actor.EndUserID(), Username: actor.Username}
	}
	if repo != nil {
		e.Repo = &RepoRef{ID: repo.String(), Type: string(repo.Type), UUID: repoUUID.String()}
	}
	return e
}

// Emitter queues an event for delivery to every matching webhook. It never
// blocks on delivery.
type Emitter interface {
	Emit(ctx context.Context, e Event) error
}

// Sign returns the X-Hangar-Signature value for a body: `sha256=<hex HMAC>`.
func Sign(secret, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// Verify checks a signature in constant time.
func Verify(secret, body []byte, signature string) bool {
	return hmac.Equal([]byte(Sign(secret, body)), []byte(signature))
}
