package gc_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/gc"
)

func TestUsageAndQuotas(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		f := newFixture(t, d)
		a := gc.NewAccounting(d)
		ml := f.namespace("ML-Team")
		solo := f.namespace("solo")
		empty := f.namespace("empty")

		r1 := f.repo(ml, "one", false)
		r2 := f.repo(ml, "two", false)
		r3 := f.repo(ml, "deleted", true)
		r4 := f.repo(solo, "x", false)
		shared, _ := f.object(strings.Repeat("s", 1000), f.now)
		big, _ := f.object(strings.Repeat("b", 5000), f.now)
		absent, _ := f.object(strings.Repeat("a", 7000), f.now)
		f.reference(r1, shared, 1000, true)
		f.reference(r2, shared, 1000, true) // the same object twice in one Namespace counts once
		f.reference(r3, big, 5000, true)    // deleted Repos count until purged
		f.reference(r2, absent, 7000, false)
		f.reference(r4, shared, 1000, true) // and once per Namespace
		f.pack(r1, strings.Repeat("1", 40), "live", f.now)
		f.pack(r1, strings.Repeat("2", 40), "retired", f.now)
		f.pack(r4, strings.Repeat("3", 40), "pending", f.now)
		_, err := d.NewInsert().Model(&db.XetXorb{Hash: "x1", Size: 64, StorageKey: "xet/xorbs/x1", LastReferencedAt: f.now}).Exec(ctx)
		require.NoError(t, err)
		require.NoError(t, a.SetQuota(ctx, empty, 1<<20))

		u, err := a.Usage(ctx)
		require.NoError(t, err)
		require.Equal(t, int64(13000), u.TotalLFSBytes)
		require.Equal(t, int64(300), u.TotalGitBytes, "every stored pack, retired ones too")
		require.Equal(t, int64(64), u.XetCacheBytes)
		require.Len(t, u.Namespaces, 3)
		require.Equal(t, "ML-Team", u.Namespaces[0].Namespace, "largest first")
		require.Equal(t, int64(6000), u.Namespaces[0].LFSBytes)
		require.Equal(t, int64(100), u.Namespaces[0].GitBytes)
		require.Equal(t, 2, u.Namespaces[0].RepoCount)
		require.Equal(t, "solo", u.Namespaces[1].Namespace)
		require.Equal(t, int64(1100), u.Namespaces[1].Total())
		require.Equal(t, "empty", u.Namespaces[2].Namespace)
		require.Equal(t, int64(1<<20), u.Namespaces[2].QuotaBytes)

		nu, err := a.NamespaceUsage(ctx, solo)
		require.NoError(t, err)
		require.Equal(t, int64(1000), nu.LFSBytes)
		require.Equal(t, 1, nu.RepoCount)
		nu, err = a.NamespaceUsage(ctx, f.namespace("nothing"))
		require.NoError(t, err)
		require.Equal(t, "nothing", nu.Namespace)
		require.Zero(t, nu.Total())
		_, err = a.NamespaceUsage(ctx, uuid.New())
		require.ErrorIs(t, err, gc.ErrNamespaceNotFound)

		// Quotas.
		require.NoError(t, a.CheckQuota(ctx, ml, 1<<40), "no quota, no limit")
		require.NoError(t, a.SetQuota(ctx, ml, 7000))
		q, err := a.Quota(ctx, ml)
		require.NoError(t, err)
		require.Equal(t, int64(7000), q)
		require.NoError(t, a.CheckQuota(ctx, ml, 900))
		err = a.CheckQuota(ctx, ml, 901)
		require.ErrorIs(t, err, gc.ErrQuotaExceeded)
		var qe *gc.QuotaError
		require.True(t, errors.As(err, &qe))
		require.Equal(t, int64(6100), qe.Used)
		require.Equal(t, "This change needs 901 B more, but ML-Team has used 6 KiB of its 6.8 KiB storage quota. Delete files or ask an admin to raise the quota.", err.Error())
		require.NoError(t, a.SetQuota(ctx, ml, 8000))
		q, err = a.Quota(ctx, ml)
		require.NoError(t, err)
		require.Equal(t, int64(8000), q, "setting again replaces the quota")
		require.NoError(t, a.SetQuota(ctx, ml, 0))
		q, err = a.Quota(ctx, ml)
		require.NoError(t, err)
		require.Zero(t, q, "zero removes the quota")
		require.Error(t, a.SetQuota(ctx, ml, -1))
		require.ErrorIs(t, a.SetQuota(ctx, uuid.New(), 10), gc.ErrNamespaceNotFound)

		// New bytes: what a change adds that the Namespace does not hold.
		n, err := a.NewLFSBytes(ctx, ml, map[string]int64{shared: 1000, absent: 7000, "f00d": 50})
		require.NoError(t, err)
		require.Equal(t, int64(7050), n, "objects that are not present still need storing")
		n, err = a.NewLFSBytes(ctx, ml, nil)
		require.NoError(t, err)
		require.Zero(t, n)

		id, name, err := a.NamespaceByName(ctx, "ml-team")
		require.NoError(t, err)
		require.Equal(t, ml, id)
		require.Equal(t, "ML-Team", name)
		_, _, err = a.NamespaceByName(ctx, "nobody")
		require.ErrorIs(t, err, gc.ErrNamespaceNotFound)
	})
}

func TestFormatBytes(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1 KiB", 1536: "1.5 KiB", 5 << 30: "5 GiB", 5 << 40: "5 TiB", 1 << 60: "1 EiB"} {
		require.Equal(t, want, gc.FormatBytes(n))
	}
}
