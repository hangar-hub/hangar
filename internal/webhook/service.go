package webhook

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/jobs"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/secrets"
	"github.com/jamesagarside/hangar/internal/telemetry"
)

// SecretPurpose binds webhook secrets to their use when they are sealed
// with the Instance key (see secrets.Sealer).
const SecretPurpose = "webhook_secret"

// Ping is the event a test delivery sends. Webhooks receive it whatever
// events they subscribe to.
const Ping EventName = "ping"

// Errors.
var (
	ErrNotFound = errors.New("webhook not found")
	ErrInvalid  = errors.New("invalid webhook")
	// ErrPrivateTarget means the URL resolves to a private address and the
	// Instance setting webhooks.allow_private_targets is off.
	ErrPrivateTarget = errors.New("webhook targets a private address, which this Instance does not allow")
)

// MaxPayloadSize bounds a delivery body.
const MaxPayloadSize = 1 << 20

// Webhook is a registered webhook. Exactly one of NamespaceID and RepoUUID
// is set.
type Webhook struct {
	ID          uuid.UUID
	NamespaceID *uuid.UUID
	RepoUUID    *uuid.UUID
	URL         string
	Events      []EventName
	Enabled     bool
	CreatedBy   uuid.UUID
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Wants reports whether the webhook subscribes to an event.
func (w *Webhook) Wants(e EventName) bool { return slices.Contains(w.Events, e) }

// Delivery is one entry in the delivery log: an event sent, or to be
// sent, to a webhook, with the outcome of the latest attempt.
type Delivery struct {
	ID            uuid.UUID
	WebhookID     uuid.UUID
	Event         EventName
	EventID       uuid.UUID
	Payload       string
	StatusCode    *int
	Attempt       int
	NextAttemptAt *time.Time
	DurationMS    *int
	Error         string
	DeliveredAt   *time.Time
	CreatedAt     time.Time
}

// State summarises a delivery: delivered, pending (an attempt is
// scheduled) or failed (no more attempts).
func (d *Delivery) State() string {
	switch {
	case d.DeliveredAt != nil:
		return "delivered"
	case d.NextAttemptAt != nil:
		return "pending"
	default:
		return "failed"
	}
}

// CreateParams registers a webhook.
type CreateParams struct {
	NamespaceID *uuid.UUID
	RepoUUID    *uuid.UUID
	URL         string
	Secret      string
	Events      []string
	Enabled     bool
	CreatedBy   uuid.UUID
}

// Update changes a webhook. Nil fields are unchanged.
type Update struct {
	URL     *string
	Secret  *string
	Events  *[]string
	Enabled *bool
}

// ListOptions selects webhooks by scope. A webhook matches when its
// Namespace is in NamespaceIDs or its Repo is in RepoUUIDs.
type ListOptions struct {
	NamespaceIDs []uuid.UUID
	RepoUUIDs    []uuid.UUID
}

// Options configure the Service.
type Options struct {
	DB     *db.DB
	Queue  jobs.Queue
	Sealer *secrets.Sealer
	// AllowPrivateTargets reads the Instance setting
	// webhooks.allow_private_targets; nil means allowed, the default.
	AllowPrivateTargets func(ctx context.Context) (bool, error)
	// Transport sends deliveries; nil uses a transport that checks every
	// address it connects to against AllowPrivateTargets.
	Transport http.RoundTripper
	// Instruments records hangar.webhook.deliveries; optional.
	Instruments *telemetry.Instruments
	Log         *slog.Logger
	// UserAgent is sent with deliveries.
	UserAgent string
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Service stores webhooks and delivers events to them. It implements
// Emitter.
type Service struct {
	o         Options
	transOnce sync.Once
	trans     http.RoundTripper
}

var _ Emitter = (*Service)(nil)

// New returns a Service.
func New(o Options) *Service {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.UserAgent == "" {
		o.UserAgent = "Hangar-Webhook/1"
	}
	return &Service{o: o}
}

func (s *Service) now() time.Time { return s.o.Now().UTC().Truncate(time.Microsecond) }

// ValidEvent reports whether name is a webhook event.
func ValidEvent(name string) bool { return slices.Contains(AllEvents(), EventName(name)) }

func validate(rawURL string, events []string) ([]string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return nil, fmt.Errorf("%w: the URL must be an absolute http or https URL without credentials", ErrInvalid)
	}
	if len(rawURL) > 2048 {
		return nil, fmt.Errorf("%w: the URL is longer than 2048 characters", ErrInvalid)
	}
	if len(events) == 0 {
		return nil, fmt.Errorf("%w: choose at least one event", ErrInvalid)
	}
	out := []string{}
	for _, e := range events {
		if !ValidEvent(e) {
			return nil, fmt.Errorf("%w: unknown event %q", ErrInvalid, e)
		}
		if !slices.Contains(out, e) {
			out = append(out, e)
		}
	}
	slices.Sort(out)
	return out, nil
}

func validSecret(secret string) error {
	if len(secret) < 8 || len(secret) > 256 {
		return fmt.Errorf("%w: the secret must be 8 to 256 characters", ErrInvalid)
	}
	return nil
}

func toWebhook(row *db.Webhook) *Webhook {
	w := &Webhook{ID: row.ID, NamespaceID: row.ScopeNamespaceID, RepoUUID: row.ScopeRepoID, URL: row.URL, Enabled: row.Enabled,
		CreatedAt: row.CreatedAt.UTC(), UpdatedAt: row.UpdatedAt.UTC()}
	if row.CreatedBy != nil {
		w.CreatedBy = *row.CreatedBy
	}
	for _, e := range row.Events {
		w.Events = append(w.Events, EventName(e))
	}
	return w
}

// Create registers a webhook.
func (s *Service) Create(ctx context.Context, p CreateParams) (*Webhook, error) {
	if (p.NamespaceID == nil) == (p.RepoUUID == nil) {
		return nil, fmt.Errorf("%w: give a Namespace or a Repo", ErrInvalid)
	}
	events, err := validate(p.URL, p.Events)
	if err != nil {
		return nil, err
	}
	if err := validSecret(p.Secret); err != nil {
		return nil, err
	}
	sealed, err := s.o.Sealer.Seal(p.Secret, SecretPurpose)
	if err != nil {
		return nil, err
	}
	row := &db.Webhook{ID: db.NewID(), ScopeNamespaceID: p.NamespaceID, ScopeRepoID: p.RepoUUID, URL: p.URL,
		SecretCiphertext: sealed, Events: events, Enabled: p.Enabled}
	if p.CreatedBy != uuid.Nil {
		row.CreatedBy = &p.CreatedBy
	}
	if _, err := s.o.DB.NewInsert().Model(row).Exec(ctx); err != nil {
		return nil, err
	}
	return toWebhook(row), nil
}

func (s *Service) row(ctx context.Context, id uuid.UUID) (*db.Webhook, error) {
	var row db.Webhook
	err := s.o.DB.NewSelect().Model(&row).Where("id = ?", id).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return &row, err
}

// Get returns a webhook.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (*Webhook, error) {
	row, err := s.row(ctx, id)
	if err != nil {
		return nil, err
	}
	return toWebhook(row), nil
}

// List returns the webhooks in the given scopes, oldest first.
func (s *Service) List(ctx context.Context, o ListOptions) ([]Webhook, error) {
	if len(o.NamespaceIDs) == 0 && len(o.RepoUUIDs) == 0 {
		return []Webhook{}, nil
	}
	var rows []db.Webhook
	q := s.o.DB.NewSelect().Model(&rows).WhereGroup(" AND ", func(q *bun.SelectQuery) *bun.SelectQuery {
		if len(o.NamespaceIDs) > 0 {
			q = q.WhereOr("scope_namespace_id IN (?)", bun.In(o.NamespaceIDs))
		}
		if len(o.RepoUUIDs) > 0 {
			q = q.WhereOr("scope_repo_id IN (?)", bun.In(o.RepoUUIDs))
		}
		return q
	}).Order("created_at ASC", "id ASC")
	if err := q.Scan(ctx); err != nil {
		return nil, err
	}
	out := make([]Webhook, 0, len(rows))
	for i := range rows {
		out = append(out, *toWebhook(&rows[i]))
	}
	return out, nil
}

// Update changes a webhook.
func (s *Service) Update(ctx context.Context, id uuid.UUID, u Update) (*Webhook, error) {
	row, err := s.row(ctx, id)
	if err != nil {
		return nil, err
	}
	if u.URL != nil {
		row.URL = *u.URL
	}
	events := row.Events
	if u.Events != nil {
		events = *u.Events
	}
	if row.Events, err = validate(row.URL, events); err != nil {
		return nil, err
	}
	if u.Secret != nil {
		if err := validSecret(*u.Secret); err != nil {
			return nil, err
		}
		if row.SecretCiphertext, err = s.o.Sealer.Seal(*u.Secret, SecretPurpose); err != nil {
			return nil, err
		}
	}
	if u.Enabled != nil {
		row.Enabled = *u.Enabled
	}
	if _, err := s.o.DB.NewUpdate().Model(row).Column("url", "secret_ciphertext", "events", "enabled", "updated_at").WherePK().Exec(ctx); err != nil {
		return nil, err
	}
	return toWebhook(row), nil
}

// Delete removes a webhook and its delivery log.
func (s *Service) Delete(ctx context.Context, id uuid.UUID) error {
	res, err := s.o.DB.NewDelete().Model((*db.Webhook)(nil)).Where("id = ?", id).Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return nil
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func toDelivery(row *db.WebhookDelivery) Delivery {
	return Delivery{ID: row.ID, WebhookID: row.WebhookID, Event: EventName(row.Event), EventID: row.EventID, Payload: row.Payload,
		StatusCode: row.StatusCode, Attempt: row.Attempt, NextAttemptAt: utc(row.NextAttemptAt), DurationMS: row.DurationMS,
		Error: row.Error, DeliveredAt: utc(row.DeliveredAt), CreatedAt: row.CreatedAt.UTC()}
}

// Deliveries returns a page of a webhook's delivery log, newest first.
// cursor is the ID of the last delivery of the previous page.
func (s *Service) Deliveries(ctx context.Context, webhookID uuid.UUID, cursor string, limit int) ([]Delivery, string, error) {
	if limit <= 0 {
		limit = 50
	}
	var rows []db.WebhookDelivery
	q := s.o.DB.NewSelect().Model(&rows).Where("webhook_id = ?", webhookID).Order("id DESC").Limit(int64(limit + 1))
	if cursor != "" {
		after, err := uuid.Parse(cursor)
		if err != nil {
			return nil, "", fmt.Errorf("%w: bad cursor", ErrInvalid)
		}
		q = q.Where("id < ?", after)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		next = rows[limit-1].ID.String()
	}
	out := make([]Delivery, 0, len(rows))
	for i := range rows {
		out = append(out, toDelivery(&rows[i]))
	}
	return out, next, nil
}

// LastDelivery returns a webhook's newest delivery, or nil.
func (s *Service) LastDelivery(ctx context.Context, webhookID uuid.UUID) (*Delivery, error) {
	list, _, err := s.Deliveries(ctx, webhookID, "", 1)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return &list[0], nil
}

// deliveryJob is the payload of a webhook.deliver Job.
type deliveryJob struct {
	DeliveryID uuid.UUID `json:"delivery_id"`
}

func (s *Service) enqueue(ctx context.Context, d *db.WebhookDelivery, at time.Time) error {
	_, err := s.o.Queue.Enqueue(ctx, jobs.NewJob{Kind: jobs.KindWebhookDeliver, DedupeKey: fmt.Sprintf("%s:%d", d.ID, d.Attempt),
		Payload: deliveryJob{DeliveryID: d.ID}, RunAfter: at, MaxAttempts: 3})
	return err
}

func (s *Service) queueDelivery(ctx context.Context, webhookID uuid.UUID, event EventName, eventID uuid.UUID, payload string) (*Delivery, error) {
	now := s.now()
	row := &db.WebhookDelivery{ID: db.NewID(), WebhookID: webhookID, Event: string(event), EventID: eventID, Payload: payload, NextAttemptAt: &now}
	if _, err := s.o.DB.NewInsert().Model(row).Exec(ctx); err != nil {
		return nil, err
	}
	if err := s.enqueue(ctx, row, time.Time{}); err != nil {
		return nil, err
	}
	d := toDelivery(row)
	return &d, nil
}

// Emit implements Emitter: it records a delivery for every enabled webhook
// that subscribes to the event in the event's Repo or Namespace, and
// queues them. Delivery happens in webhook.deliver Jobs.
func (s *Service) Emit(ctx context.Context, e Event) error {
	if e.Event == "" {
		return fmt.Errorf("%w: the event has no name", ErrInvalid)
	}
	var scopes ListOptions
	if e.NamespaceID != uuid.Nil {
		scopes.NamespaceIDs = append(scopes.NamespaceIDs, e.NamespaceID)
	}
	if e.Repo != nil {
		if id, err := uuid.Parse(e.Repo.UUID); err == nil {
			scopes.RepoUUIDs = append(scopes.RepoUUIDs, id)
		}
	}
	hooks, err := s.List(ctx, scopes)
	if err != nil {
		return err
	}
	var payload []byte
	for _, h := range hooks {
		if !h.Enabled || !h.Wants(e.Event) {
			continue
		}
		if payload == nil {
			if payload, err = json.Marshal(e); err != nil {
				return err
			}
			if len(payload) > MaxPayloadSize {
				return fmt.Errorf("%w: the %s payload is larger than %d bytes", ErrInvalid, e.Event, MaxPayloadSize)
			}
		}
		if _, err := s.queueDelivery(ctx, h.ID, e.Event, e.ID, string(payload)); err != nil {
			return err
		}
	}
	return nil
}

// Redeliver sends a delivery's payload again as a new delivery.
func (s *Service) Redeliver(ctx context.Context, webhookID, deliveryID uuid.UUID) (*Delivery, error) {
	var row db.WebhookDelivery
	err := s.o.DB.NewSelect().Model(&row).Where("id = ?", deliveryID).Where("webhook_id = ?", webhookID).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: delivery %s", ErrNotFound, deliveryID)
	}
	if err != nil {
		return nil, err
	}
	return s.queueDelivery(ctx, webhookID, EventName(row.Event), row.EventID, row.Payload)
}

// Test queues a ping event to a webhook.
func (s *Service) Test(ctx context.Context, webhookID uuid.UUID, actor model.Principal) (*Delivery, error) {
	if _, err := s.row(ctx, webhookID); err != nil {
		return nil, err
	}
	e := NewEvent(Ping, actor, nil, uuid.Nil, uuid.Nil, map[string]any{"webhook_id": webhookID.String()})
	payload, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	return s.queueDelivery(ctx, webhookID, Ping, e.ID, string(payload))
}

// Register adds the webhook.deliver Job handler.
func (s *Service) Register(reg *jobs.Registry) error {
	return reg.Register(jobs.KindWebhookDeliver, s.HandleJob, jobs.HandlerOptions{MaxAttempts: 3, Concurrency: 8, Timeout: time.Minute})
}

// HandleJob makes one delivery attempt. A failed attempt schedules the
// next one in RetrySchedule as a new Job; after the last, the delivery is
// failed. The Job itself only fails on database errors.
func (s *Service) HandleJob(ctx context.Context, j *jobs.Job, _ jobs.Progress) error {
	var p deliveryJob
	if err := j.DecodePayload(&p); err != nil {
		return jobs.Permanent(err)
	}
	var d db.WebhookDelivery
	err := s.o.DB.NewSelect().Model(&d).Where("id = ?", p.DeliveryID).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if d.DeliveredAt != nil || d.NextAttemptAt == nil {
		return nil
	}
	hook, err := s.row(ctx, d.WebhookID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	status, dur, sendErr := s.send(ctx, hook, &d)
	return s.record(ctx, &d, status, dur, sendErr)
}

// errNoRetry marks failures that another attempt cannot fix.
type errNoRetry struct{ error }

func (s *Service) record(ctx context.Context, d *db.WebhookDelivery, status int, dur time.Duration, sendErr error) error {
	now := s.now()
	d.Attempt++
	ms := int(dur.Milliseconds())
	d.DurationMS = &ms
	if status != 0 {
		d.StatusCode = &status
	} else {
		d.StatusCode = nil
	}
	outcome := "success"
	var retryAt time.Time
	switch {
	case sendErr == nil:
		d.DeliveredAt, d.NextAttemptAt, d.Error = &now, nil, ""
	default:
		d.Error = sendErr.Error()
		var nr errNoRetry
		if errors.As(sendErr, &nr) || d.Attempt > len(RetrySchedule) {
			d.NextAttemptAt = nil
			outcome = "failed"
		} else {
			retryAt = now.Add(RetrySchedule[d.Attempt-1])
			d.NextAttemptAt = &retryAt
			outcome = "retry"
		}
	}
	if _, err := s.o.DB.NewUpdate().Model(d).Column("attempt", "status_code", "duration_ms", "error", "delivered_at", "next_attempt_at", "updated_at").WherePK().Exec(ctx); err != nil {
		return err
	}
	if s.o.Instruments != nil && s.o.Instruments.WebhookDeliveries != nil {
		s.o.Instruments.WebhookDeliveries.Add(ctx, 1, metric.WithAttributes(attribute.String(telemetry.MetricOutcome, outcome)))
	}
	if !retryAt.IsZero() {
		return s.enqueue(ctx, d, retryAt)
	}
	return nil
}

func (s *Service) send(ctx context.Context, hook *db.Webhook, d *db.WebhookDelivery) (int, time.Duration, error) {
	if !hook.Enabled {
		return 0, 0, errNoRetry{errors.New("the webhook is disabled")}
	}
	secret, err := s.o.Sealer.Open(hook.SecretCiphertext, SecretPurpose)
	if err != nil {
		return 0, 0, errNoRetry{errors.New("the webhook secret cannot be decrypted with the Instance key")}
	}
	body := []byte(d.Payload)
	ctx, cancel := context.WithTimeout(ctx, DeliveryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hook.URL, bytes.NewReader(body))
	if err != nil {
		return 0, 0, errNoRetry{errors.New("the webhook URL is not valid")}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", s.o.UserAgent)
	req.Header.Set(HeaderEvent, d.Event)
	req.Header.Set(HeaderDelivery, d.ID.String())
	req.Header.Set(HeaderSignature, Sign([]byte(secret), body))
	client := &http.Client{Transport: s.transport(), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	start := time.Now()
	resp, err := client.Do(req)
	dur := time.Since(start)
	if err != nil {
		return 0, dur, sanitise(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, dur, fmt.Errorf("the receiver answered %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	return resp.StatusCode, dur, nil
}

// sanitise drops the URL from client errors, because webhook URLs can
// carry credentials in their query.
func sanitise(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var priv privateError
	if errors.As(err, &priv) {
		return errNoRetry{ErrPrivateTarget}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("no answer within %s", DeliveryTimeout)
	}
	return errors.New(strings.TrimSpace(err.Error()))
}

type privateError struct{ addr netip.Addr }

func (e privateError) Error() string { return "private address " + e.addr.String() }

// IsPrivate reports whether an address is in a range a multi-tenant
// Instance must not reach: loopback, private, link-local (including cloud
// metadata), carrier-grade NAT, unique local, unspecified and multicast.
func IsPrivate(a netip.Addr) bool {
	a = a.Unmap()
	if a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsUnspecified() ||
		a.IsMulticast() || a.IsInterfaceLocalMulticast() {
		return true
	}
	for _, p := range []netip.Prefix{netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("192.0.0.0/24"),
		netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("64:ff9b::/96")} {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func (s *Service) transport() http.RoundTripper {
	if s.o.Transport != nil {
		return s.o.Transport
	}
	s.transOnce.Do(func() { s.trans = s.newTransport() })
	return s.trans
}

func (s *Service) newTransport() http.RoundTripper {
	dialer := &net.Dialer{Timeout: DeliveryTimeout, Control: func(_, address string, _ syscall.RawConn) error {
		if s.o.AllowPrivateTargets == nil {
			return nil
		}
		allow, err := s.o.AllowPrivateTargets(context.Background())
		if err != nil {
			return err
		}
		ap, err := netip.ParseAddrPort(address)
		if err != nil {
			return err
		}
		if !allow && IsPrivate(ap.Addr()) {
			return privateError{addr: ap.Addr()}
		}
		return nil
	}}
	return &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: DeliveryTimeout, ResponseHeaderTimeout: DeliveryTimeout,
		MaxIdleConns: 16, IdleConnTimeout: 30 * time.Second, Proxy: nil}
}

// ReencryptSecrets re-seals every webhook secret with a new Instance key,
// inside the caller's transaction, for `hangar admin rotate-key`.
func ReencryptSecrets(ctx context.Context, tx bun.IDB, from, to *secrets.Sealer) (int, error) {
	var rows []db.Webhook
	if err := tx.NewSelect().Model(&rows).Column("id", "secret_ciphertext").Scan(ctx); err != nil {
		return 0, err
	}
	for _, row := range rows {
		plain, err := from.Open(row.SecretCiphertext, SecretPurpose)
		if err != nil {
			return 0, fmt.Errorf("webhook %s: %w", row.ID, err)
		}
		sealed, err := to.Seal(plain, SecretPurpose)
		if err != nil {
			return 0, err
		}
		if _, err := tx.NewUpdate().Model((*db.Webhook)(nil)).Set("secret_ciphertext = ?", sealed).Where("id = ?", row.ID).Exec(ctx); err != nil {
			return 0, err
		}
	}
	return len(rows), nil
}
