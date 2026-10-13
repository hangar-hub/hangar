package upstream

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/jamesagarside/hangar/internal/audit"
	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/config"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/secrets"
)

// Registry errors.
var (
	// ErrUpstreamNotFound means no Upstream has that ID or name.
	ErrUpstreamNotFound = errors.New("upstream not found")
	// ErrUpstreamDisabled means an admin turned the Upstream off.
	ErrUpstreamDisabled = errors.New("upstream is disabled")
	// ErrUpstreamExists means the name is taken.
	ErrUpstreamExists = errors.New("an upstream with that name already exists")
	// ErrUpstreamInvalid wraps validation errors.
	ErrUpstreamInvalid = errors.New("invalid upstream")
	// ErrUpstreamConfigured means the Upstream comes from the
	// configuration file, so only `enabled` can change in the admin UI.
	ErrUpstreamConfigured = errors.New("upstream is set in the configuration file")
	// ErrUpstreamInUse means Mirrors still use the Upstream.
	ErrUpstreamInUse = errors.New("upstream is used by mirrors")
)

// tokenPurpose binds Instance token ciphertexts to their use.
const tokenPurpose = "upstream_instance_token"

// DefaultUpstream is the built-in Upstream's name.
const DefaultUpstream = "huggingface"

var upstreamNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// Registry stores Upstreams in the upstreams table (PULL-1): the ones from
// the configuration file, kept in step at start-up, and the ones admins
// add. Instance tokens are encrypted with the Instance key and never
// returned by any API.
type Registry struct {
	db     *db.DB
	sealer *secrets.Sealer
	audit  audit.Recorder
}

// NewRegistry returns a Registry. audit may be nil in tests.
func NewRegistry(d *db.DB, sealer *secrets.Sealer, rec audit.Recorder) *Registry {
	return &Registry{db: d, sealer: sealer, audit: rec}
}

func toUpstream(r *db.Upstream) Upstream {
	return Upstream{ID: r.ID, Name: r.Name, DisplayName: r.DisplayName, BaseURL: r.BaseURL, ViewerURL: r.ViewerURL,
		Enabled: r.Enabled, NamespacePrefix: r.NamespacePrefix, Source: r.Source,
		HasInstanceToken: r.InstanceTokenCiphertext != nil && *r.InstanceTokenCiphertext != ""}
}

func validateBaseURL(raw string, field string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", fmt.Errorf("%w: %s must be an http or https URL with no query, fragment or user name", ErrUpstreamInvalid, field)
	}
	return raw, nil
}

func validatePrefix(p string) error {
	if p == "" {
		return nil
	}
	if err := model.ValidateNamespaceName(p + "x"); err != nil {
		return fmt.Errorf("%w: namespace_prefix %q makes invalid Namespace names", ErrUpstreamInvalid, p)
	}
	return nil
}

func (r *Registry) seal(token string) (*string, error) {
	if token == "" {
		return nil, nil
	}
	if r.sealer == nil {
		return nil, errors.New("no Instance key to encrypt the upstream token with")
	}
	ct, err := r.sealer.Seal(token, tokenPurpose)
	if err != nil {
		return nil, err
	}
	return &ct, nil
}

func (r *Registry) record(ctx context.Context, actor model.Principal, up Upstream, change string) {
	if r.audit == nil {
		return
	}
	_ = r.audit.Record(ctx, audit.Event{Action: audit.UpstreamChanged, Actor: actor, TargetType: "upstream", TargetID: up.Name,
		Data: map[string]any{"change": change, "base_url": up.BaseURL, "enabled": up.Enabled}})
}

// SyncConfig makes the configured Upstreams exist with the configured base
// URL, viewer URL, prefix and Instance token. A configured Upstream that
// an admin disabled stays disabled; `enabled: false` in the configuration
// always disables it. Rows that came from the configuration and are no
// longer in it are disabled, not deleted, because Mirrors refer to them.
func (r *Registry) SyncConfig(ctx context.Context, ups map[string]config.UpstreamConfig) error {
	names := make([]string, 0, len(ups))
	for n := range ups {
		names = append(names, n)
	}
	sort.Strings(names)
	return r.db.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		for _, name := range names {
			c := ups[name]
			if !upstreamNameRe.MatchString(name) {
				return fmt.Errorf("%w: upstream name %q must be lower-case letters, digits, '-' or '_'", ErrUpstreamInvalid, name)
			}
			base, err := validateBaseURL(c.BaseURL, "upstreams."+name+".base_url")
			if err != nil {
				return err
			}
			viewer := ""
			if c.ViewerURL != "" {
				if viewer, err = validateBaseURL(c.ViewerURL, "upstreams."+name+".viewer_url"); err != nil {
					return err
				}
			}
			if err := validatePrefix(c.NamespacePrefix); err != nil {
				return err
			}
			tok, err := r.seal(c.Token)
			if err != nil {
				return err
			}
			var row db.Upstream
			err = tx.NewSelect().Model(&row).Where("name = ?", name).Scan(ctx)
			now := db.Now()
			switch {
			case db.IsNotFound(err):
				row = db.Upstream{ID: db.NewID(), Name: name, DisplayName: displayName(name, base), BaseURL: base, ViewerURL: viewer,
					Kind: "hf", Enabled: c.Enabled, NamespacePrefix: c.NamespacePrefix, Source: SourceConfig, InstanceTokenCiphertext: tok,
					Timestamps: db.Timestamps{CreatedAt: now, UpdatedAt: now}}
				if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
					return err
				}
			case err != nil:
				return err
			default:
				row.BaseURL, row.ViewerURL, row.NamespacePrefix, row.Source = base, viewer, c.NamespacePrefix, SourceConfig
				row.InstanceTokenCiphertext = tok
				if !c.Enabled {
					row.Enabled = false
				}
				row.UpdatedAt = now
				if _, err := tx.NewUpdate().Model(&row).WherePK().Exec(ctx); err != nil {
					return err
				}
			}
		}
		q := tx.NewUpdate().Model((*db.Upstream)(nil)).Set("enabled = ?", false).Set("updated_at = ?", db.Now()).Where("source = ?", SourceConfig)
		if len(names) > 0 {
			q = q.Where("name NOT IN (?)", bun.List(names))
		}
		_, err := q.Exec(ctx)
		return err
	})
}

func displayName(name, base string) string {
	if name == DefaultUpstream {
		return "Hugging Face"
	}
	if u, err := url.Parse(base); err == nil && u.Host != "" {
		return u.Host
	}
	return name
}

// List returns Upstreams, the default one first, then by name.
func (r *Registry) List(ctx context.Context, enabledOnly bool) ([]Upstream, error) {
	var rows []db.Upstream
	q := r.db.NewSelect().Model(&rows).OrderExpr("name")
	if enabledOnly {
		q = q.Where("enabled = ?", true)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, err
	}
	out := make([]Upstream, 0, len(rows))
	for i := range rows {
		out = append(out, toUpstream(&rows[i]))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name == DefaultUpstream && out[j].Name != DefaultUpstream })
	return out, nil
}

// Get returns an Upstream by ID.
func (r *Registry) Get(ctx context.Context, id uuid.UUID) (*Upstream, error) {
	var row db.Upstream
	if err := r.db.NewSelect().Model(&row).Where("id = ?", id).Scan(ctx); err != nil {
		if db.IsNotFound(err) {
			return nil, fmt.Errorf("%w: %s", ErrUpstreamNotFound, id)
		}
		return nil, err
	}
	u := toUpstream(&row)
	return &u, nil
}

// ByName returns an Upstream by name.
func (r *Registry) ByName(ctx context.Context, name string) (*Upstream, error) {
	var row db.Upstream
	if err := r.db.NewSelect().Model(&row).Where("name = ?", strings.ToLower(name)).Scan(ctx); err != nil {
		if db.IsNotFound(err) {
			return nil, fmt.Errorf("%w: %q", ErrUpstreamNotFound, name)
		}
		return nil, err
	}
	u := toUpstream(&row)
	return &u, nil
}

// Enabled returns the named Upstream if it is enabled, or the default
// Upstream (huggingface if enabled, else the first enabled) for "".
func (r *Registry) Enabled(ctx context.Context, name string) (*Upstream, error) {
	if name != "" {
		u, err := r.ByName(ctx, name)
		if err != nil {
			return nil, err
		}
		if !u.Enabled {
			return nil, fmt.Errorf("%w: %s", ErrUpstreamDisabled, u.Name)
		}
		return u, nil
	}
	list, err := r.List(ctx, true)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("%w: no Upstream is enabled", ErrUpstreamNotFound)
	}
	return &list[0], nil
}

// UpstreamID implements auth.UpstreamLookup for /me/upstream-tokens.
func (r *Registry) UpstreamID(ctx context.Context, name string) (uuid.UUID, error) {
	u, err := r.ByName(ctx, name)
	if errors.Is(err, ErrUpstreamNotFound) {
		return uuid.Nil, fmt.Errorf("%w: upstream %q", auth.ErrNotFound, name)
	}
	if err != nil {
		return uuid.Nil, err
	}
	return u.ID, nil
}

// InstanceToken returns the Upstream's Instance token, or "".
func (r *Registry) InstanceToken(ctx context.Context, id uuid.UUID) (string, error) {
	var row db.Upstream
	if err := r.db.NewSelect().Model(&row).Column("instance_token_ciphertext").Where("id = ?", id).Scan(ctx); err != nil {
		if db.IsNotFound(err) {
			return "", fmt.Errorf("%w: %s", ErrUpstreamNotFound, id)
		}
		return "", err
	}
	if row.InstanceTokenCiphertext == nil || *row.InstanceTokenCiphertext == "" {
		return "", nil
	}
	if r.sealer == nil {
		return "", errors.New("no Instance key to decrypt the upstream token with")
	}
	return r.sealer.Open(*row.InstanceTokenCiphertext, tokenPurpose)
}

// UpstreamParams describe a new Upstream.
type UpstreamParams struct {
	Name            string
	DisplayName     string
	BaseURL         string
	ViewerURL       string
	Enabled         bool
	NamespacePrefix string
	InstanceToken   string
}

// UpstreamUpdate changes an Upstream. Nil fields are unchanged; an empty
// InstanceToken removes the token.
type UpstreamUpdate struct {
	DisplayName     *string
	BaseURL         *string
	ViewerURL       *string
	Enabled         *bool
	NamespacePrefix *string
	InstanceToken   *string
}

// Create adds an Upstream (Instance admins only; the caller checks).
func (r *Registry) Create(ctx context.Context, p UpstreamParams, actor model.Principal) (*Upstream, error) {
	p.Name = strings.TrimSpace(p.Name)
	if !upstreamNameRe.MatchString(p.Name) {
		return nil, fmt.Errorf("%w: the name must be 1 to 63 lower-case letters, digits, '-' or '_'", ErrUpstreamInvalid)
	}
	base, err := validateBaseURL(p.BaseURL, "base_url")
	if err != nil {
		return nil, err
	}
	viewer := ""
	if p.ViewerURL != "" {
		if viewer, err = validateBaseURL(p.ViewerURL, "viewer_url"); err != nil {
			return nil, err
		}
	}
	if err := validatePrefix(p.NamespacePrefix); err != nil {
		return nil, err
	}
	tok, err := r.seal(strings.TrimSpace(p.InstanceToken))
	if err != nil {
		return nil, err
	}
	dn := strings.TrimSpace(p.DisplayName)
	if dn == "" {
		dn = displayName(p.Name, base)
	}
	now := db.Now()
	row := db.Upstream{ID: db.NewID(), Name: p.Name, DisplayName: dn, BaseURL: base, ViewerURL: viewer, Kind: "hf", Enabled: p.Enabled,
		NamespacePrefix: p.NamespacePrefix, Source: SourceAdmin, InstanceTokenCiphertext: tok, Timestamps: db.Timestamps{CreatedAt: now, UpdatedAt: now}}
	if _, err := r.db.NewInsert().Model(&row).Exec(ctx); err != nil {
		if db.IsUniqueViolation(err) {
			return nil, fmt.Errorf("%w: %s", ErrUpstreamExists, p.Name)
		}
		return nil, err
	}
	u := toUpstream(&row)
	r.record(ctx, actor, u, "created")
	return &u, nil
}

// Update changes an Upstream. Configured Upstreams only accept `enabled`.
func (r *Registry) Update(ctx context.Context, id uuid.UUID, p UpstreamUpdate, actor model.Principal) (*Upstream, error) {
	var out Upstream
	err := r.db.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		var row db.Upstream
		if err := forUpdate(r.db, tx.NewSelect().Model(&row).Where("id = ?", id)).Scan(ctx); err != nil {
			if db.IsNotFound(err) {
				return fmt.Errorf("%w: %s", ErrUpstreamNotFound, id)
			}
			return err
		}
		if row.Source == SourceConfig && (p.BaseURL != nil || p.ViewerURL != nil || p.NamespacePrefix != nil || p.InstanceToken != nil) {
			return fmt.Errorf("%w: change base_url, viewer_url, namespace_prefix and token in the configuration file", ErrUpstreamConfigured)
		}
		if p.DisplayName != nil {
			row.DisplayName = strings.TrimSpace(*p.DisplayName)
		}
		if p.BaseURL != nil {
			base, err := validateBaseURL(*p.BaseURL, "base_url")
			if err != nil {
				return err
			}
			row.BaseURL = base
		}
		if p.ViewerURL != nil {
			row.ViewerURL = ""
			if *p.ViewerURL != "" {
				v, err := validateBaseURL(*p.ViewerURL, "viewer_url")
				if err != nil {
					return err
				}
				row.ViewerURL = v
			}
		}
		if p.NamespacePrefix != nil {
			if err := validatePrefix(*p.NamespacePrefix); err != nil {
				return err
			}
			n, err := tx.NewSelect().Model((*db.Mirror)(nil)).Where("upstream_id = ?", id).Count(ctx)
			if err != nil {
				return err
			}
			if n > 0 && *p.NamespacePrefix != row.NamespacePrefix {
				return fmt.Errorf("%w: the namespace prefix cannot change while %d Mirrors use this Upstream", ErrUpstreamInUse, n)
			}
			row.NamespacePrefix = *p.NamespacePrefix
		}
		if p.Enabled != nil {
			row.Enabled = *p.Enabled
		}
		if p.InstanceToken != nil {
			tok, err := r.seal(strings.TrimSpace(*p.InstanceToken))
			if err != nil {
				return err
			}
			row.InstanceTokenCiphertext = tok
		}
		row.UpdatedAt = db.Now()
		if _, err := tx.NewUpdate().Model(&row).WherePK().Exec(ctx); err != nil {
			return err
		}
		out = toUpstream(&row)
		return nil
	})
	if err != nil {
		return nil, err
	}
	r.record(ctx, actor, out, "updated")
	return &out, nil
}

// Delete removes an Upstream that came from the admin UI and has no Mirrors.
func (r *Registry) Delete(ctx context.Context, id uuid.UUID, actor model.Principal) error {
	u, err := r.Get(ctx, id)
	if err != nil {
		return err
	}
	if u.Source == SourceConfig {
		return fmt.Errorf("%w: remove it from the configuration file, or disable it", ErrUpstreamConfigured)
	}
	err = r.db.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		n, err := tx.NewSelect().Model((*db.Mirror)(nil)).Where("upstream_id = ?", id).Count(ctx)
		if err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("%w: %d Mirrors use it; disable it instead, or detach the Mirrors first", ErrUpstreamInUse, n)
		}
		_, err = tx.NewDelete().Model((*db.Upstream)(nil)).Where("id = ?", id).Exec(ctx)
		return err
	})
	if err != nil {
		return err
	}
	r.record(ctx, actor, *u, "deleted")
	return nil
}

// Test checks that the Upstream answers: it lists one model with the
// Instance token. It returns the round-trip time.
func Test(ctx context.Context, c Client, u Upstream, cred Credentials) (time.Duration, error) {
	start := time.Now()
	_, err := c.Search(ctx, u, cred, SearchQuery{Type: model.RepoTypeModel, Limit: 1})
	return time.Since(start), err
}

// forUpdate locks the selected rows on Postgres. SQLite has one writer, so
// its transactions need no row locks.
func forUpdate(d *db.DB, q *bun.SelectQuery) *bun.SelectQuery {
	if d.IsSQLite() {
		return q
	}
	return q.For("UPDATE")
}
