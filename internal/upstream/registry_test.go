package upstream_test

import (
	"context"
	"fmt"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/audit"
	"github.com/jamesagarside/hangar/internal/audit/audittest"
	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/config"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/secrets"
	"github.com/jamesagarside/hangar/internal/upstream"
	"github.com/jamesagarside/hangar/internal/upstream/upstreamtest"
)

func newRegistry(t *testing.T, d *db.DB) (*upstream.Registry, *audittest.Fake) {
	t.Helper()
	key := make([]byte, 32)
	sealer, err := secrets.New(key)
	require.NoError(t, err)
	rec := audittest.NewFake()
	return upstream.NewRegistry(d, sealer, rec), rec
}

func TestRegistryConfigAndAdminUpstreams(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		reg, rec := newRegistry(t, d)
		admin := model.System()
		require.NoError(t, reg.SyncConfig(ctx, map[string]config.UpstreamConfig{
			"huggingface": {BaseURL: "https://huggingface.co/", Enabled: true, Token: "hf_instance"},
			"corp":        {BaseURL: "https://hub.corp.example", Enabled: true, NamespacePrefix: "corp-", ViewerURL: "https://viewer.corp.example"},
		}))
		list, err := reg.List(ctx, false)
		require.NoError(t, err)
		require.Len(t, list, 2)
		require.Equal(t, "huggingface", list[0].Name, "the default Upstream comes first")
		require.Equal(t, "https://huggingface.co", list[0].BaseURL)
		require.Equal(t, "Hugging Face", list[0].DisplayName)
		require.True(t, list[0].HasInstanceToken)
		require.Equal(t, upstream.SourceConfig, list[0].Source)
		tok, err := reg.InstanceToken(ctx, list[0].ID)
		require.NoError(t, err)
		require.Equal(t, "hf_instance", tok)
		var raw string
		require.NoError(t, d.NewSelect().Model((*db.Upstream)(nil)).Column("instance_token_ciphertext").Where("id = ?", list[0].ID).Scan(ctx, &raw))
		require.NotContains(t, raw, "hf_instance", "the Instance token is stored encrypted")

		// Admins can disable a configured Upstream; a restart keeps it disabled.
		off := false
		_, err = reg.Update(ctx, list[0].ID, upstream.UpstreamUpdate{Enabled: &off}, admin)
		require.NoError(t, err)
		base := "https://elsewhere"
		_, err = reg.Update(ctx, list[0].ID, upstream.UpstreamUpdate{BaseURL: &base}, admin)
		require.ErrorIs(t, err, upstream.ErrUpstreamConfigured)
		require.NoError(t, reg.SyncConfig(ctx, map[string]config.UpstreamConfig{"huggingface": {BaseURL: "https://huggingface.co", Enabled: true}}))
		hf, err := reg.ByName(ctx, "huggingface")
		require.NoError(t, err)
		require.False(t, hf.Enabled)
		require.False(t, hf.HasInstanceToken, "the token follows the configuration")
		corp, err := reg.ByName(ctx, "corp")
		require.NoError(t, err)
		require.False(t, corp.Enabled, "removed from the configuration: disabled, not deleted")
		_, err = reg.Enabled(ctx, "")
		require.ErrorIs(t, err, upstream.ErrUpstreamNotFound)
		_, err = reg.Enabled(ctx, "huggingface")
		require.ErrorIs(t, err, upstream.ErrUpstreamDisabled)
		require.ErrorIs(t, reg.Delete(ctx, hf.ID, admin), upstream.ErrUpstreamConfigured)

		// Admin Upstreams.
		other, err := reg.Create(ctx, upstream.UpstreamParams{Name: "other-hangar", BaseURL: "http://hangar.lan:8080/", Enabled: true, InstanceToken: "hgr_x"}, admin)
		require.NoError(t, err)
		require.Equal(t, upstream.SourceAdmin, other.Source)
		require.Equal(t, "hangar.lan:8080", other.DisplayName)
		require.Equal(t, "http://datasets-server.hangar.lan:8080", other.DatasetViewerURL())
		_, err = reg.Create(ctx, upstream.UpstreamParams{Name: "other-hangar", BaseURL: "http://x"}, admin)
		require.ErrorIs(t, err, upstream.ErrUpstreamExists)
		for _, bad := range []upstream.UpstreamParams{{Name: "Bad Name", BaseURL: "http://x"}, {Name: "ok", BaseURL: "ftp://x"},
			{Name: "ok", BaseURL: "http://u:p@x"}, {Name: "ok", BaseURL: "http://x?q=1"}, {Name: "ok", BaseURL: "http://x", NamespacePrefix: "-"}} {
			_, err = reg.Create(ctx, bad, admin)
			require.ErrorIs(t, err, upstream.ErrUpstreamInvalid, fmt.Sprint(bad))
		}
		empty := ""
		viewer := "https://v.example/"
		u, err := reg.Update(ctx, other.ID, upstream.UpstreamUpdate{InstanceToken: &empty, ViewerURL: &viewer}, admin)
		require.NoError(t, err)
		require.False(t, u.HasInstanceToken)
		require.Equal(t, "https://v.example", u.ViewerURL)
		got, err := reg.Enabled(ctx, "")
		require.NoError(t, err)
		require.Equal(t, "other-hangar", got.Name)
		id, err := reg.UpstreamID(ctx, "other-hangar")
		require.NoError(t, err)
		require.Equal(t, other.ID, id)
		_, err = reg.UpstreamID(ctx, "nope")
		require.ErrorIs(t, err, auth.ErrNotFound)
		require.NoError(t, reg.Delete(ctx, other.ID, admin))
		_, err = reg.Get(ctx, other.ID)
		require.ErrorIs(t, err, upstream.ErrUpstreamNotFound)
		require.Contains(t, rec.Actions(), audit.UpstreamChanged)
		for _, ev := range rec.Events() {
			for _, v := range ev.Data {
				require.NotContains(t, fmt.Sprint(v), "hgr_x", "audit data never holds tokens")
			}
		}
		require.Error(t, reg.SyncConfig(ctx, map[string]config.UpstreamConfig{"BAD": {BaseURL: "http://x"}}))
	})
}

func TestRegistryRefusesChangesUnderMirrors(t *testing.T) {
	forEachDB(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d, upstreamtest.HarnessOptions{})
		require.NoError(t, e.AddUpstream("second", "s-"))
		up, err := e.Upstreams.ByName(e.Ctx, "second")
		require.NoError(t, err)
		e.Seed(tiny, nil, map[string][]byte{"m.bin": []byte("m")})
		id, _, err := e.Service.StartPull(e.Ctx, upstream.PullRequest{Upstream: "second", Repo: tiny, Actor: e.User("alice")})
		require.NoError(t, err)
		e.Run(5)
		require.Equal(t, "succeeded", string(e.Job(id).Status))
		require.ErrorIs(t, e.Upstreams.Delete(e.Ctx, up.ID, model.System()), upstream.ErrUpstreamInUse)
		p := "t-"
		_, err = e.Upstreams.Update(e.Ctx, up.ID, upstream.UpstreamUpdate{NamespacePrefix: &p}, model.System())
		require.ErrorIs(t, err, upstream.ErrUpstreamInUse)
		lat, err := upstream.Test(e.Ctx, newClient(), *up, upstream.Credentials{})
		require.NoError(t, err)
		require.Positive(t, lat)
	})
}

func TestUpstreamIDMapping(t *testing.T) {
	up := upstream.Upstream{BaseURL: "https://huggingface.co", NamespacePrefix: "hf-"}
	id := model.RepoID{Type: model.RepoTypeModel, Namespace: "jinaai", Name: "x"}
	local := up.LocalID(id)
	require.Equal(t, "hf-jinaai/x", local.String())
	back, ok := up.UpstreamID(local)
	require.True(t, ok)
	require.Equal(t, id, back)
	_, ok = up.UpstreamID(id)
	require.False(t, ok)
	require.Equal(t, "huggingface.co", up.Host())
	plain := upstream.Upstream{}
	same, ok := plain.UpstreamID(id)
	require.True(t, ok)
	require.Equal(t, id, same)
	_, err := url.Parse(up.BaseURL)
	require.NoError(t, err)
}
