package audit

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"

	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/model"
)

// ErrBadCursor means a List cursor was not made by List.
var ErrBadCursor = errors.New("invalid cursor")

// MaxLimit is the largest page List returns.
const MaxLimit = 500

// EventNamePrefix starts the event name of every audit log record.
const EventNamePrefix = "hangar.audit."

// DBRecorder stores audit events in the audit_events table and emits each
// one as an OpenTelemetry log record named `hangar.audit.<action>` with
// the same fields. It implements Recorder and Log.
type DBRecorder struct {
	db     *db.DB
	logger otellog.Logger
	now    func() time.Time
}

var (
	_ Recorder = (*DBRecorder)(nil)
	_ Log      = (*DBRecorder)(nil)
)

// NewDB returns a DBRecorder. lp may be nil, which stores events without
// emitting log records.
func NewDB(d *db.DB, lp otellog.LoggerProvider) *DBRecorder {
	r := &DBRecorder{db: d, now: time.Now}
	if lp != nil {
		r.logger = lp.Logger("github.com/hangar-hub/hangar/internal/audit")
	}
	return r
}

// SetClock replaces the clock, for tests.
func (r *DBRecorder) SetClock(now func() time.Time) { r.now = now }

// secretKeys are refused in Data, as a last guard against secrets in the
// audit log.
var secretKeys = []string{"password", "secret", "token", "authorization", "cookie", "ciphertext"}

func checkData(data map[string]any) error {
	for k := range data {
		lk := strings.ToLower(k)
		for _, s := range secretKeys {
			if strings.Contains(lk, s) && !strings.HasSuffix(lk, "_name") && !strings.HasSuffix(lk, "_id") {
				return fmt.Errorf("audit data key %q looks like a secret", k)
			}
		}
	}
	return nil
}

// Record implements Recorder.
func (r *DBRecorder) Record(ctx context.Context, e Event) error {
	if e.Action == "" {
		return errors.New("audit event needs an action")
	}
	if err := checkData(e.Data); err != nil {
		return err
	}
	if e.Time.IsZero() {
		e.Time = r.now()
	}
	e.Time = e.Time.UTC().Truncate(time.Microsecond)
	data := e.Data
	if data == nil {
		data = map[string]any{}
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("audit data: %w", err)
	}
	row := &db.AuditEvent{
		ID: db.NewID(), Action: string(e.Action), TargetType: e.TargetType, TargetID: e.TargetID,
		IP: e.IP, UserAgent: truncate(e.UserAgent, 512), Data: raw,
	}
	row.CreatedAt, row.UpdatedAt = e.Time, e.Time
	if e.Actor.UserID != uuid.Nil {
		id := e.Actor.UserID
		row.ActorUserID = &id
	}
	if e.Actor.Token != nil && e.Actor.Token.ID != uuid.Nil {
		id := e.Actor.Token.ID
		row.ActorTokenID = &id
	}
	if _, err := r.db.NewInsert().Model(row).Exec(ctx); err != nil {
		return fmt.Errorf("store audit event: %w", err)
	}
	r.emit(ctx, row.ID, e)
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// emit writes the OpenTelemetry log record.
func (r *DBRecorder) emit(ctx context.Context, id uuid.UUID, e Event) {
	if r.logger == nil {
		return
	}
	var rec otellog.Record
	rec.SetEventName(EventNamePrefix + string(e.Action))
	rec.SetTimestamp(e.Time)
	rec.SetObservedTimestamp(r.now())
	rec.SetSeverity(otellog.SeverityInfo)
	rec.SetSeverityText("INFO")
	rec.SetBody(attribute.StringValue("audit " + string(e.Action)))
	attrs := []attribute.KeyValue{
		attribute.String("hangar.audit.id", id.String()),
		attribute.String("hangar.audit.action", string(e.Action)),
		attribute.String("hangar.audit.actor", actorName(e.Actor)),
	}
	if e.Actor.UserID != uuid.Nil {
		attrs = append(attrs, attribute.String("enduser.id", e.Actor.UserID.String()))
	}
	if e.Actor.Token != nil && e.Actor.Token.ID != uuid.Nil {
		attrs = append(attrs, attribute.String("hangar.audit.actor_token_id", e.Actor.Token.ID.String()))
	}
	if e.TargetType != "" {
		attrs = append(attrs, attribute.String("hangar.audit.target_type", e.TargetType))
	}
	if e.TargetID != "" {
		attrs = append(attrs, attribute.String("hangar.audit.target_id", e.TargetID))
	}
	if e.IP != "" {
		attrs = append(attrs, attribute.String("client.address", e.IP))
	}
	if e.UserAgent != "" {
		attrs = append(attrs, attribute.String("user_agent.original", truncate(e.UserAgent, 512)))
	}
	keys := make([]string, 0, len(e.Data))
	for k := range e.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		attrs = append(attrs, attribute.String("hangar.audit.data."+k, fmt.Sprint(e.Data[k])))
	}
	rec.AddAttributes(attrs...)
	r.logger.Emit(ctx, rec)
}

func actorName(p model.Principal) string {
	switch {
	case p.Username != "":
		return p.Username
	case p.Kind == model.PrincipalSystem:
		return model.SystemUsername
	default:
		return "anonymous"
	}
}

type auditRow struct {
	db.AuditEvent `bun:",extend"`
	ActorUsername *string `bun:"actor_username"`
}

type cursor struct {
	At time.Time `json:"t"`
	ID uuid.UUID `json:"i"`
}

func encodeCursor(c cursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string) (cursor, error) {
	var c cursor
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || json.Unmarshal(b, &c) != nil || c.ID == uuid.Nil {
		return cursor{}, ErrBadCursor
	}
	return c, nil
}

// List implements Log: events newest first, filtered by actor username,
// actions and a [Since, Until) time range, in pages of at most MaxLimit.
func (r *DBRecorder) List(ctx context.Context, f Filter) ([]Entry, string, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, MaxLimit)
	q := r.db.NewSelect().
		TableExpr("audit_events AS e").
		ColumnExpr("e.*").
		ColumnExpr("n.name AS actor_username").
		Join("LEFT JOIN users AS u ON u.id = e.actor_user_id").
		Join("LEFT JOIN namespaces AS n ON n.id = u.namespace_id")
	if f.ActorUsername != "" {
		q = q.Where("lower(n.name) = lower(?)", f.ActorUsername)
	}
	if len(f.Actions) > 0 {
		acts := make([]string, 0, len(f.Actions))
		for _, a := range f.Actions {
			acts = append(acts, string(a))
		}
		slices.Sort(acts)
		q = q.Where("e.action IN (?)", bun.List(acts))
	}
	if !f.Since.IsZero() {
		q = q.Where("e.created_at >= ?", f.Since.UTC())
	}
	if !f.Until.IsZero() {
		q = q.Where("e.created_at < ?", f.Until.UTC())
	}
	if f.Cursor != "" {
		c, err := decodeCursor(f.Cursor)
		if err != nil {
			return nil, "", err
		}
		q = q.Where("(e.created_at < ? OR (e.created_at = ? AND e.id < ?))", c.At, c.At, c.ID)
	}
	var rows []auditRow
	if err := q.OrderExpr("e.created_at DESC, e.id DESC").Limit(int64(limit+1)).Scan(ctx, &rows); err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		next = encodeCursor(cursor{At: last.CreatedAt, ID: last.ID})
	}
	out := make([]Entry, 0, len(rows))
	for _, row := range rows {
		e := Entry{ID: row.ID.String()}
		e.Action = Action(row.Action)
		e.TargetType, e.TargetID, e.IP, e.UserAgent = row.TargetType, row.TargetID, row.IP, row.UserAgent
		e.Time = row.CreatedAt.UTC()
		if row.ActorUserID != nil {
			e.Actor = model.Principal{Kind: model.PrincipalUser, UserID: *row.ActorUserID}
		} else {
			e.Actor = model.Anonymous()
		}
		if row.ActorUsername != nil {
			e.ActorUsername = *row.ActorUsername
			e.Actor.Username = *row.ActorUsername
		}
		if row.ActorTokenID != nil {
			e.Actor.Token = &model.TokenInfo{ID: *row.ActorTokenID}
		}
		if len(row.Data) > 0 {
			if err := json.Unmarshal(row.Data, &e.Data); err != nil {
				return nil, "", fmt.Errorf("decode audit data: %w", err)
			}
		}
		out = append(out, e)
	}
	return out, next, nil
}
