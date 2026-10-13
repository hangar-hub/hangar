// Package audit records security-relevant actions (EVT-2): to the
// audit_events table and as OpenTelemetry log records named
// `hangar.audit.<action>`. audittest has an in-memory fake.
package audit

import (
	"context"
	"time"

	"github.com/jamesagarside/hangar/internal/model"
)

// Action names an audited action. The list covers docs/spec/05-auth.md
// "Audit events".
type Action string

// Audited actions.
const (
	LoginSucceeded        Action = "login.succeeded"
	LoginFailed           Action = "login.failed"
	Logout                Action = "logout"
	TokenCreated          Action = "token.created"
	TokenRevoked          Action = "token.revoked"
	UserCreated           Action = "user.created"
	UserDisabled          Action = "user.disabled"
	UserEnabled           Action = "user.enabled"
	UserAdminChanged      Action = "user.admin_changed"
	UserPasswordReset     Action = "user.password_reset"
	InviteCreated         Action = "invite.created"
	MembershipChanged     Action = "membership.changed"
	MembershipRemoved     Action = "membership.removed"
	OrgCreated            Action = "org.created"
	OrgDeleted            Action = "org.deleted"
	RepoCreated           Action = "repo.created"
	RepoVisibilityChanged Action = "repo.visibility_changed"
	RepoRenamed           Action = "repo.renamed"
	RepoTransferred       Action = "repo.transferred"
	RepoDeleted           Action = "repo.deleted"
	RepoRestored          Action = "repo.restored"
	RepoPurged            Action = "repo.purged"
	CollaboratorsChanged  Action = "repo.collaborators_changed"
	AccessRequestDecided  Action = "repo.access_request_decided"
	UpstreamChanged       Action = "upstream.changed"
	UpstreamTokenChanged  Action = "upstream_token.changed"
	SettingsChanged       Action = "settings.changed"
	QuotaChanged          Action = "quota.changed"
	WebhookChanged        Action = "webhook.changed"
	OllamaKeyChanged      Action = "ollama_key.changed"
	InstanceKeyRotated    Action = "instance_key.rotated"
)

// Event is one audited action.
type Event struct {
	Action Action
	// Actor is who did it; anonymous for failed logins.
	Actor model.Principal
	// TargetType and TargetID name what it was done to, such as "repo"
	// and "model:ns/name".
	TargetType string
	TargetID   string
	IP         string
	UserAgent  string
	// Data holds action details. Never put secrets in it.
	Data map[string]any
	// Time is when it happened; zero means now.
	Time time.Time
}

// Recorder records audit events.
type Recorder interface {
	Record(ctx context.Context, e Event) error
}

// Filter selects audit events for the admin audit log.
type Filter struct {
	ActorUsername string
	Actions       []Action
	Since, Until  time.Time
	Cursor        string
	Limit         int
}

// Entry is a stored audit event.
type Entry struct {
	Event
	ID            string
	ActorUsername string
}

// Log lists recorded events for the admin UI and CSV export.
type Log interface {
	List(ctx context.Context, f Filter) ([]Entry, string, error)
}
