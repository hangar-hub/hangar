package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/uptrace/bun"

	"github.com/hangar-hub/hangar/internal/audit"
	"github.com/hangar-hub/hangar/internal/config"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/model"
)

// ErrInvalidSettings wraps validation errors from SettingsStore.Update.
var ErrInvalidSettings = errors.New("invalid settings")

// InstanceSettings are the Instance settings an admin can change in the UI
// (GET and PATCH /admin/settings). Values not changed in the UI come from
// the configuration file. Field names match the native API.
type InstanceSettings struct {
	AnonymousRead               bool     `json:"anonymous_read"`
	Registration                string   `json:"registration"`
	LocalLogin                  bool     `json:"local_login"`
	UIExternalImages            bool     `json:"ui_external_images"`
	ScanBlockUnsafeDownloads    bool     `json:"scan_block_unsafe_downloads"`
	ScanBlockLevel              string   `json:"scan_block_level"`
	WebhooksAllowPrivateTargets bool     `json:"webhooks_allow_private_targets"`
	PullBandwidthLimit          int64    `json:"pull_bandwidth_limit"`
	PullConcurrency             int      `json:"pull_concurrency"`
	PullWindows                 []string `json:"pull_windows"`
	PullAllowOverrideWindows    bool     `json:"pull_allow_override_windows"`
	PullThroughEnabled          bool     `json:"pull_through_enabled"`
	PullThroughAllow            []string `json:"pull_through_allow"`
	PullThroughMinRole          string   `json:"pull_through_min_role"`
}

// SettingsPatch changes some Instance settings. Nil fields are unchanged.
type SettingsPatch struct {
	AnonymousRead               *bool     `json:"anonymous_read,omitempty"`
	Registration                *string   `json:"registration,omitempty"`
	LocalLogin                  *bool     `json:"local_login,omitempty"`
	UIExternalImages            *bool     `json:"ui_external_images,omitempty"`
	ScanBlockUnsafeDownloads    *bool     `json:"scan_block_unsafe_downloads,omitempty"`
	ScanBlockLevel              *string   `json:"scan_block_level,omitempty"`
	WebhooksAllowPrivateTargets *bool     `json:"webhooks_allow_private_targets,omitempty"`
	PullBandwidthLimit          *int64    `json:"pull_bandwidth_limit,omitempty"`
	PullConcurrency             *int      `json:"pull_concurrency,omitempty"`
	PullWindows                 *[]string `json:"pull_windows,omitempty"`
	PullAllowOverrideWindows    *bool     `json:"pull_allow_override_windows,omitempty"`
	PullThroughEnabled          *bool     `json:"pull_through_enabled,omitempty"`
	PullThroughAllow            *[]string `json:"pull_through_allow,omitempty"`
	PullThroughMinRole          *string   `json:"pull_through_min_role,omitempty"`
}

// Setting keys in the settings table.
const (
	SettingAnonymousRead            = "security.anonymous_read"
	SettingRegistration             = "security.registration"
	SettingLocalLogin               = "security.local_login"
	SettingUIExternalImages         = "ui.external_images"
	SettingScanBlockUnsafe          = "scan.block_unsafe_downloads"
	SettingScanBlockLevel           = "scan.block_level"
	SettingWebhooksAllowPrivate     = "webhooks.allow_private_targets"
	SettingPullBandwidthLimit       = "pull.bandwidth_limit"
	SettingPullConcurrency          = "pull.concurrency"
	SettingPullWindows              = "pull.windows"
	SettingPullAllowOverrideWindows = "pull.allow_override_windows"
	SettingPullThroughEnabled       = "pull.pull_through.enabled"
	SettingPullThroughAllow         = "pull.pull_through.allow"
	SettingPullThroughMinRole       = "pull.pull_through.min_role"
)

// Values of scan.block_level: how strict scan.block_unsafe_downloads is.
// unsafe refuses files the scan marked unsafe; suspicious also refuses
// files whose imports are on neither the allow nor the deny list. The
// values match scan.Level.
const (
	ScanBlockLevelUnsafe     = "unsafe"
	ScanBlockLevelSuspicious = "suspicious"
)

// settingsCacheTTL bounds how long another replica's change takes to apply.
const settingsCacheTTL = 5 * time.Second

// SettingsStore reads and changes Instance settings. Reads are cached for
// a few seconds so permission checks do not query the database each time.
type SettingsStore struct {
	s *Service

	mu      sync.Mutex
	cached  *InstanceSettings
	expires time.Time
}

// Defaults returns the settings from the configuration file alone.
func (st *SettingsStore) Defaults() InstanceSettings {
	c := st.s.cfg
	return InstanceSettings{
		AnonymousRead: c.Security.AnonymousRead, Registration: c.Security.Registration, LocalLogin: c.Security.LocalLogin,
		UIExternalImages: true, ScanBlockUnsafeDownloads: false, ScanBlockLevel: ScanBlockLevelUnsafe, WebhooksAllowPrivateTargets: true,
		PullBandwidthLimit: c.Pull.BandwidthLimit.Int64(), PullConcurrency: c.Pull.Concurrency,
		PullWindows: slices.Clone(c.Pull.Windows), PullAllowOverrideWindows: true,
		PullThroughEnabled: c.Pull.PullThrough.Enabled, PullThroughAllow: slices.Clone(c.Pull.PullThrough.Allow),
		PullThroughMinRole: c.Pull.PullThrough.MinRole,
	}
}

// Get returns the current settings: stored values over the configuration.
func (st *SettingsStore) Get(ctx context.Context) (InstanceSettings, error) {
	st.mu.Lock()
	if st.cached != nil && st.s.now().Before(st.expires) {
		out := *st.cached
		st.mu.Unlock()
		return out, nil
	}
	st.mu.Unlock()
	out, err := st.load(ctx, st.s.db)
	if err != nil {
		return InstanceSettings{}, err
	}
	st.mu.Lock()
	st.cached, st.expires = &out, st.s.now().Add(settingsCacheTTL)
	st.mu.Unlock()
	return out, nil
}

func (st *SettingsStore) load(ctx context.Context, idb bun.IDB) (InstanceSettings, error) {
	out := st.Defaults()
	var rows []db.Setting
	if err := idb.NewSelect().Model(&rows).Scan(ctx); err != nil {
		return out, err
	}
	targets := settingTargets(&out)
	for _, r := range rows {
		if dst, ok := targets[r.Key]; ok {
			if err := json.Unmarshal(r.Value, dst); err != nil {
				st.s.log.WarnContext(ctx, "ignoring unreadable Instance setting", "key", r.Key, "error", err)
			}
		}
	}
	return out, nil
}

// settingTargets maps each key to the field it fills.
func settingTargets(s *InstanceSettings) map[string]any {
	return map[string]any{
		SettingAnonymousRead: &s.AnonymousRead, SettingRegistration: &s.Registration, SettingLocalLogin: &s.LocalLogin,
		SettingUIExternalImages: &s.UIExternalImages, SettingScanBlockUnsafe: &s.ScanBlockUnsafeDownloads,
		SettingScanBlockLevel: &s.ScanBlockLevel, SettingWebhooksAllowPrivate: &s.WebhooksAllowPrivateTargets,
		SettingPullBandwidthLimit: &s.PullBandwidthLimit, SettingPullConcurrency: &s.PullConcurrency, SettingPullWindows: &s.PullWindows,
		SettingPullAllowOverrideWindows: &s.PullAllowOverrideWindows, SettingPullThroughEnabled: &s.PullThroughEnabled,
		SettingPullThroughAllow: &s.PullThroughAllow, SettingPullThroughMinRole: &s.PullThroughMinRole,
	}
}

var windowRe = regexp.MustCompile(`^([01]\d|2[0-3]):([0-5]\d)-([01]\d|2[0-3]):([0-5]\d)$`)

// changes validates p and returns the keys and values to store.
func (p SettingsPatch) changes() (map[string]any, error) {
	var errs []error
	out := map[string]any{}
	set := func(key string, v any) { out[key] = v }
	if p.AnonymousRead != nil {
		set(SettingAnonymousRead, *p.AnonymousRead)
	}
	if p.Registration != nil {
		switch *p.Registration {
		case config.RegistrationOpen, config.RegistrationInvite, config.RegistrationClosed:
			set(SettingRegistration, *p.Registration)
		default:
			errs = append(errs, fmt.Errorf("registration must be open, invite or closed"))
		}
	}
	if p.LocalLogin != nil {
		set(SettingLocalLogin, *p.LocalLogin)
	}
	if p.UIExternalImages != nil {
		set(SettingUIExternalImages, *p.UIExternalImages)
	}
	if p.ScanBlockUnsafeDownloads != nil {
		set(SettingScanBlockUnsafe, *p.ScanBlockUnsafeDownloads)
	}
	if p.ScanBlockLevel != nil {
		switch *p.ScanBlockLevel {
		case ScanBlockLevelUnsafe, ScanBlockLevelSuspicious:
			set(SettingScanBlockLevel, *p.ScanBlockLevel)
		default:
			errs = append(errs, fmt.Errorf("scan_block_level must be unsafe or suspicious"))
		}
	}
	if p.WebhooksAllowPrivateTargets != nil {
		set(SettingWebhooksAllowPrivate, *p.WebhooksAllowPrivateTargets)
	}
	if p.PullBandwidthLimit != nil {
		if *p.PullBandwidthLimit < 0 {
			errs = append(errs, fmt.Errorf("pull_bandwidth_limit cannot be negative"))
		} else {
			set(SettingPullBandwidthLimit, *p.PullBandwidthLimit)
		}
	}
	if p.PullConcurrency != nil {
		if *p.PullConcurrency < 1 || *p.PullConcurrency > 256 {
			errs = append(errs, fmt.Errorf("pull_concurrency must be between 1 and 256"))
		} else {
			set(SettingPullConcurrency, *p.PullConcurrency)
		}
	}
	if p.PullWindows != nil {
		ok := true
		for _, w := range *p.PullWindows {
			if !windowRe.MatchString(w) {
				errs = append(errs, fmt.Errorf("pull_windows: %q must have the form HH:MM-HH:MM", w))
				ok = false
			}
		}
		if ok {
			set(SettingPullWindows, nonNil(*p.PullWindows))
		}
	}
	if p.PullAllowOverrideWindows != nil {
		set(SettingPullAllowOverrideWindows, *p.PullAllowOverrideWindows)
	}
	if p.PullThroughEnabled != nil {
		set(SettingPullThroughEnabled, *p.PullThroughEnabled)
	}
	if p.PullThroughAllow != nil {
		ok := true
		for _, g := range *p.PullThroughAllow {
			if _, err := path.Match(g, "x/y"); err != nil || g == "" {
				errs = append(errs, fmt.Errorf("pull_through_allow: %q is not a valid glob", g))
				ok = false
			}
		}
		if ok {
			set(SettingPullThroughAllow, nonNil(*p.PullThroughAllow))
		}
	}
	if p.PullThroughMinRole != nil {
		switch *p.PullThroughMinRole {
		case "anonymous", "user":
			set(SettingPullThroughMinRole, *p.PullThroughMinRole)
		default:
			errs = append(errs, fmt.Errorf("pull_through_min_role must be anonymous or user"))
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("%w: %w", ErrInvalidSettings, errors.Join(errs...))
	}
	return out, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// Update validates and stores a patch, records a settings.changed audit
// event naming the changed keys, and returns the new settings.
func (st *SettingsStore) Update(ctx context.Context, p SettingsPatch, actor model.Principal) (InstanceSettings, error) {
	changes, err := p.changes()
	if err != nil {
		return InstanceSettings{}, err
	}
	var out InstanceSettings
	err = st.s.db.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		for key, v := range changes {
			raw, err := json.Marshal(v)
			if err != nil {
				return err
			}
			row := &db.Setting{Key: key, Value: raw}
			if _, err := tx.NewInsert().Model(row).On("CONFLICT (key) DO UPDATE").
				Set("value = EXCLUDED.value").Set("updated_at = EXCLUDED.updated_at").Exec(ctx); err != nil {
				return err
			}
		}
		out, err = st.load(ctx, tx)
		return err
	})
	if err != nil {
		return InstanceSettings{}, err
	}
	st.mu.Lock()
	st.cached, st.expires = &out, st.s.now().Add(settingsCacheTTL)
	st.mu.Unlock()
	if len(changes) > 0 {
		keys := make([]string, 0, len(changes))
		for k := range changes {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		st.s.record(ctx, audit.Event{Action: audit.SettingsChanged, Actor: actor, TargetType: "settings", TargetID: "instance",
			Data: map[string]any{"keys": keys}})
	}
	return out, nil
}
