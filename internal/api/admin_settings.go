package api

import (
	"net/http"

	"github.com/hangar-hub/hangar/internal/api/gen"
	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/httpx"
)

func (h *Identity) settingsOut(st auth.InstanceSettings) gen.InstanceSettings {
	reg := gen.InstanceSettingsRegistration(st.Registration)
	minRole := gen.InstanceSettingsPullThroughMinRole(st.PullThroughMinRole)
	level := gen.InstanceSettingsScanBlockLevel(st.ScanBlockLevel)
	windows, allow := append([]string{}, st.PullWindows...), append([]string{}, st.PullThroughAllow...)
	out := gen.InstanceSettings{
		AnonymousRead: &st.AnonymousRead, Registration: &reg, LocalLogin: &st.LocalLogin, UiExternalImages: &st.UIExternalImages,
		ScanBlockUnsafeDownloads: &st.ScanBlockUnsafeDownloads, ScanBlockLevel: &level, WebhooksAllowPrivateTargets: &st.WebhooksAllowPrivateTargets,
		PullBandwidthLimit: &st.PullBandwidthLimit, PullConcurrency: &st.PullConcurrency, PullWindows: &windows,
		PullAllowOverrideWindows: &st.PullAllowOverrideWindows, PullThroughEnabled: &st.PullThroughEnabled,
		PullThroughAllow: &allow, PullThroughMinRole: &minRole,
	}
	providers := []struct {
		Id     *string `json:"id,omitempty"`
		Issuer *string `json:"issuer,omitempty"`
		Name   *string `json:"name,omitempty"`
	}{}
	for _, p := range h.auth.OIDC.Providers() {
		id, iss, name := p.ID, p.Issuer, p.Name
		providers = append(providers, struct {
			Id     *string `json:"id,omitempty"`
			Issuer *string `json:"issuer,omitempty"`
			Name   *string `json:"name,omitempty"`
		}{Id: &id, Issuer: &iss, Name: &name})
	}
	out.OidcProviders = &providers
	return out
}

// AdminGetSettings implements GET /admin/settings. OIDC providers are read
// from the configuration file and cannot be changed here.
func (h *Identity) AdminGetSettings(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.instanceAdmin(w, r); !ok {
		return
	}
	st, err := h.auth.Settings.Get(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, h.settingsOut(st))
}

// AdminUpdateSettings implements PATCH /admin/settings. Absent fields are
// unchanged; oidc_providers is read-only and ignored.
func (h *Identity) AdminUpdateSettings(w http.ResponseWriter, r *http.Request) {
	p, ok := h.instanceAdmin(w, r)
	if !ok {
		return
	}
	var body gen.InstanceSettings
	if !h.decode(w, r, &body) {
		return
	}
	patch := auth.SettingsPatch{
		AnonymousRead: body.AnonymousRead, LocalLogin: body.LocalLogin, UIExternalImages: body.UiExternalImages,
		ScanBlockUnsafeDownloads: body.ScanBlockUnsafeDownloads, WebhooksAllowPrivateTargets: body.WebhooksAllowPrivateTargets,
		PullBandwidthLimit: body.PullBandwidthLimit, PullConcurrency: body.PullConcurrency, PullWindows: body.PullWindows,
		PullAllowOverrideWindows: body.PullAllowOverrideWindows, PullThroughEnabled: body.PullThroughEnabled,
		PullThroughAllow: body.PullThroughAllow,
	}
	if body.Registration != nil {
		s := string(*body.Registration)
		patch.Registration = &s
	}
	if body.PullThroughMinRole != nil {
		s := string(*body.PullThroughMinRole)
		patch.PullThroughMinRole = &s
	}
	if body.ScanBlockLevel != nil {
		s := string(*body.ScanBlockLevel)
		patch.ScanBlockLevel = &s
	}
	st, err := h.auth.Settings.Update(r.Context(), patch, p)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, h.settingsOut(st))
}
