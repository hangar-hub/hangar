package webhook_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/db/dbtest"
	"github.com/hangar-hub/hangar/internal/index/indextest"
	"github.com/hangar-hub/hangar/internal/jobs"
	"github.com/hangar-hub/hangar/internal/jobs/jobstest"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/repo/repotest"
	"github.com/hangar-hub/hangar/internal/secrets"
	"github.com/hangar-hub/hangar/internal/telemetry"
	"github.com/hangar-hub/hangar/internal/webhook"
)

type receiver struct {
	mu       sync.Mutex
	statuses []int
	got      []*http.Request
	bodies   [][]byte
}

func (rc *receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	b, _ := io.ReadAll(r.Body)
	rc.got = append(rc.got, r)
	rc.bodies = append(rc.bodies, b)
	status := http.StatusOK
	if len(rc.statuses) > 0 {
		status, rc.statuses = rc.statuses[0], rc.statuses[1:]
	}
	w.WriteHeader(status)
}

type env struct {
	db    *db.DB
	svc   *webhook.Service
	q     *jobstest.Fake
	clock *time.Time
	nsID  uuid.UUID
	repo  *repo.Repo
	srv   *httptest.Server
	recv  *receiver
}

func newEnv(t *testing.T, d *db.DB) *env {
	t.Helper()
	e := &env{db: d, recv: &receiver{}}
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	e.clock = &now
	sealer, err := secrets.New([]byte("0123456789abcdef0123456789abcdef"))
	require.NoError(t, err)
	reg := jobs.NewRegistry()
	e.q = jobstest.NewFake(reg)
	e.q.SetClock(func() time.Time { return *e.clock })
	reader := sdkmetric.NewManualReader()
	inst, err := telemetry.NewInstruments(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test"))
	require.NoError(t, err)
	// The test receiver listens on loopback, a private address.
	e.svc = webhook.New(webhook.Options{DB: d, Queue: e.q, Sealer: sealer, Instruments: inst, Now: func() time.Time { return *e.clock },
		AllowPrivateTargets: func(context.Context) (bool, error) { return true, nil }})
	require.NoError(t, e.svc.Register(reg))
	e.srv = httptest.NewServer(e.recv)
	t.Cleanup(e.srv.Close)

	fake := repotest.NewFake(t)
	id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "tiny"}
	e.repo = indextest.CreateRepo(t, fake, id, model.VisibilityPublic)
	indextest.InsertRepoRow(t, d, e.repo)
	e.nsID = e.repo.NamespaceID
	return e
}

func (e *env) event(name webhook.EventName) webhook.Event {
	return webhook.NewEvent(name, indextest.Owner("alice"), &e.repo.ID, e.repo.UUID, e.nsID, map[string]any{"x": 1})
}

func TestCreateValidatesAndLists(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d)
		ctx := t.Context()
		bad := []webhook.CreateParams{
			{URL: e.srv.URL, Secret: "long enough", Events: []string{"repo.created"}},
			{NamespaceID: &e.nsID, RepoUUID: &e.repo.UUID, URL: e.srv.URL, Secret: "long enough", Events: []string{"repo.created"}},
			{NamespaceID: &e.nsID, URL: "ftp://x", Secret: "long enough", Events: []string{"repo.created"}},
			{NamespaceID: &e.nsID, URL: "https://user:pw@x", Secret: "long enough", Events: []string{"repo.created"}},
			{NamespaceID: &e.nsID, URL: e.srv.URL, Secret: "short", Events: []string{"repo.created"}},
			{NamespaceID: &e.nsID, URL: e.srv.URL, Secret: "long enough"},
			{NamespaceID: &e.nsID, URL: e.srv.URL, Secret: "long enough", Events: []string{"repo.exploded"}},
		}
		for _, p := range bad {
			_, err := e.svc.Create(ctx, p)
			require.ErrorIs(t, err, webhook.ErrInvalid, "%+v", p)
		}
		ns, err := e.svc.Create(ctx, webhook.CreateParams{NamespaceID: &e.nsID, URL: e.srv.URL, Secret: "s3cret-value",
			Events: []string{"repo.updated", "repo.created", "repo.created"}, Enabled: true})
		require.NoError(t, err)
		require.Equal(t, []webhook.EventName{webhook.RepoCreated, webhook.RepoUpdated}, ns.Events)
		rp, err := e.svc.Create(ctx, webhook.CreateParams{RepoUUID: &e.repo.UUID, URL: e.srv.URL + "/repo", Secret: "s3cret-value",
			Events: []string{"scan.completed"}, Enabled: true})
		require.NoError(t, err)

		list, err := e.svc.List(ctx, webhook.ListOptions{NamespaceIDs: []uuid.UUID{e.nsID}})
		require.NoError(t, err)
		require.Len(t, list, 1)
		list, err = e.svc.List(ctx, webhook.ListOptions{NamespaceIDs: []uuid.UUID{e.nsID}, RepoUUIDs: []uuid.UUID{e.repo.UUID}})
		require.NoError(t, err)
		require.Len(t, list, 2)
		list, err = e.svc.List(ctx, webhook.ListOptions{})
		require.NoError(t, err)
		require.Empty(t, list)

		got, err := e.svc.Get(ctx, rp.ID)
		require.NoError(t, err)
		require.Equal(t, rp.URL, got.URL)
		_, err = e.svc.Get(ctx, uuid.New())
		require.ErrorIs(t, err, webhook.ErrNotFound)

		off, events, url, secret := false, []string{"repo.deleted"}, e.srv.URL+"/new", "another-secret"
		up, err := e.svc.Update(ctx, rp.ID, webhook.Update{Enabled: &off, Events: &events, URL: &url, Secret: &secret})
		require.NoError(t, err)
		require.False(t, up.Enabled)
		require.Equal(t, []webhook.EventName{webhook.RepoDeleted}, up.Events)
		bad1 := "nope"
		_, err = e.svc.Update(ctx, rp.ID, webhook.Update{URL: &bad1})
		require.ErrorIs(t, err, webhook.ErrInvalid)
		short := "x"
		_, err = e.svc.Update(ctx, rp.ID, webhook.Update{Secret: &short})
		require.ErrorIs(t, err, webhook.ErrInvalid)
		_, err = e.svc.Update(ctx, uuid.New(), webhook.Update{})
		require.ErrorIs(t, err, webhook.ErrNotFound)

		require.NoError(t, e.svc.Delete(ctx, rp.ID))
		require.ErrorIs(t, e.svc.Delete(ctx, rp.ID), webhook.ErrNotFound)
	})
}

func TestEmitSignsAndDelivers(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d)
		ctx := t.Context()
		ns, err := e.svc.Create(ctx, webhook.CreateParams{NamespaceID: &e.nsID, URL: e.srv.URL + "/ns", Secret: "namespace-secret",
			Events: []string{"repo.updated"}, Enabled: true})
		require.NoError(t, err)
		_, err = e.svc.Create(ctx, webhook.CreateParams{RepoUUID: &e.repo.UUID, URL: e.srv.URL + "/disabled", Secret: "namespace-secret",
			Events: []string{"repo.updated"}, Enabled: false})
		require.NoError(t, err)
		_, err = e.svc.Create(ctx, webhook.CreateParams{RepoUUID: &e.repo.UUID, URL: e.srv.URL + "/other-event", Secret: "namespace-secret",
			Events: []string{"repo.deleted"}, Enabled: true})
		require.NoError(t, err)

		ev := e.event(webhook.RepoUpdated)
		require.NoError(t, e.svc.Emit(ctx, ev))
		require.Equal(t, 1, e.q.RunPending(ctx))
		require.Len(t, e.recv.got, 1)
		req, body := e.recv.got[0], e.recv.bodies[0]
		require.Equal(t, "/ns", req.URL.Path)
		require.Equal(t, "repo.updated", req.Header.Get(webhook.HeaderEvent))
		require.Equal(t, "application/json", req.Header.Get("Content-Type"))
		require.True(t, webhook.Verify([]byte("namespace-secret"), body, req.Header.Get(webhook.HeaderSignature)))
		var payload map[string]any
		require.NoError(t, json.Unmarshal(body, &payload))
		require.Equal(t, "repo.updated", payload["event"])
		require.Equal(t, ev.ID.String(), payload["id"])
		require.Equal(t, "alice/tiny", payload["repo"].(map[string]any)["id"])
		require.NotContains(t, payload, "NamespaceID")

		list, next, err := e.svc.Deliveries(ctx, ns.ID, "", 10)
		require.NoError(t, err)
		require.Empty(t, next)
		require.Len(t, list, 1)
		require.Equal(t, "delivered", list[0].State())
		require.Equal(t, 200, *list[0].StatusCode)
		require.Equal(t, 1, list[0].Attempt)
		require.Equal(t, req.Header.Get(webhook.HeaderDelivery), list[0].ID.String())
		last, err := e.svc.LastDelivery(ctx, ns.ID)
		require.NoError(t, err)
		require.Equal(t, list[0].ID, last.ID)

		require.ErrorIs(t, e.svc.Emit(ctx, webhook.Event{}), webhook.ErrInvalid)
	})
}

func TestRetriesFollowTheScheduleThenFail(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d)
		ctx := t.Context()
		hook, err := e.svc.Create(ctx, webhook.CreateParams{NamespaceID: &e.nsID, URL: e.srv.URL, Secret: "retry-secret",
			Events: []string{"repo.created"}, Enabled: true})
		require.NoError(t, err)
		e.recv.statuses = []int{500, 502, 503, 404, 500, 500}
		require.NoError(t, e.svc.Emit(ctx, e.event(webhook.RepoCreated)))
		require.Equal(t, 1, e.q.RunPending(ctx))
		for i, wait := range webhook.RetrySchedule {
			list, _, err := e.svc.Deliveries(ctx, hook.ID, "", 1)
			require.NoError(t, err)
			require.Equal(t, "pending", list[0].State())
			require.Equal(t, i+1, list[0].Attempt)
			require.Equal(t, e.clock.Add(wait), *list[0].NextAttemptAt)
			require.Zero(t, e.q.RunPending(ctx), "nothing runs before the next attempt is due")
			*e.clock = e.clock.Add(wait)
			require.Equal(t, 1, e.q.RunPending(ctx))
		}
		list, _, err := e.svc.Deliveries(ctx, hook.ID, "", 1)
		require.NoError(t, err)
		require.Equal(t, "failed", list[0].State())
		require.Equal(t, 6, list[0].Attempt)
		require.Contains(t, list[0].Error, "500")
		require.Len(t, e.recv.got, 6)

		// Redelivery sends the same payload as a new delivery.
		re, err := e.svc.Redeliver(ctx, hook.ID, list[0].ID)
		require.NoError(t, err)
		require.NotEqual(t, list[0].ID, re.ID)
		require.Equal(t, list[0].EventID, re.EventID)
		require.Equal(t, 1, e.q.RunPending(ctx))
		require.Equal(t, e.recv.bodies[0], e.recv.bodies[6])
		_, err = e.svc.Redeliver(ctx, hook.ID, uuid.New())
		require.ErrorIs(t, err, webhook.ErrNotFound)

		all, next, err := e.svc.Deliveries(ctx, hook.ID, "", 1)
		require.NoError(t, err)
		require.NotEmpty(t, next)
		older, _, err := e.svc.Deliveries(ctx, hook.ID, next, 1)
		require.NoError(t, err)
		require.NotEqual(t, all[0].ID, older[0].ID)
		_, _, err = e.svc.Deliveries(ctx, hook.ID, "not-a-uuid", 1)
		require.ErrorIs(t, err, webhook.ErrInvalid)
	})
}

func TestTestDeliveryAndPrivateTargets(t *testing.T) {
	d := dbtest.SQLite(t)
	e := newEnv(t, d)
	ctx := t.Context()
	hook, err := e.svc.Create(ctx, webhook.CreateParams{NamespaceID: &e.nsID, URL: e.srv.URL, Secret: "ping-secret",
		Events: []string{"repo.created"}, Enabled: true})
	require.NoError(t, err)
	del, err := e.svc.Test(ctx, hook.ID, indextest.Owner("alice"))
	require.NoError(t, err)
	require.Equal(t, webhook.Ping, del.Event)
	e.q.RunPending(ctx)
	require.Equal(t, "ping", e.recv.got[0].Header.Get(webhook.HeaderEvent))
	_, err = e.svc.Test(ctx, uuid.New(), indextest.Owner("alice"))
	require.ErrorIs(t, err, webhook.ErrNotFound)

	// With private targets off, and when the setting is not wired up at
	// all (the default), the loopback receiver is refused without retries.
	for name, allow := range map[string]func(context.Context) (bool, error){
		"off":   func(context.Context) (bool, error) { return false, nil },
		"unset": nil,
	} {
		sealer, _ := secrets.New([]byte("0123456789abcdef0123456789abcdef"))
		reg := jobs.NewRegistry()
		q := jobstest.NewFake(reg)
		strict := webhook.New(webhook.Options{DB: d, Queue: q, Sealer: sealer, Now: func() time.Time { return *e.clock },
			AllowPrivateTargets: allow})
		require.NoError(t, strict.Register(reg))
		del, err = strict.Test(ctx, hook.ID, indextest.Owner("alice"))
		require.NoError(t, err)
		q.RunPending(ctx)
		list, _, err := strict.Deliveries(ctx, hook.ID, "", 1)
		require.NoError(t, err)
		require.Equal(t, del.ID, list[0].ID, name)
		require.Equal(t, "failed", list[0].State(), name)
		require.Equal(t, webhook.ErrPrivateTarget.Error(), list[0].Error, name)
		require.Len(t, e.recv.got, 1, "%s: the private receiver was never contacted", name)
	}

	// A disabled webhook fails its pending deliveries without sending.
	off := false
	_, err = e.svc.Update(ctx, hook.ID, webhook.Update{Enabled: &off})
	require.NoError(t, err)
	_, err = e.svc.Test(ctx, hook.ID, indextest.Owner("alice"))
	require.NoError(t, err)
	e.q.RunPending(ctx)
	list, _, err := e.svc.Deliveries(ctx, hook.ID, "", 1)
	require.NoError(t, err)
	require.Equal(t, "the webhook is disabled", list[0].Error)
}

// TestDeliveriesReuseConnectionsWithinAPolicy: deliveries share one
// transport per private-target policy, so they reuse kept-alive
// connections instead of opening a new one each time, and a connection
// kept alive while private targets were allowed is not reused once they
// are turned off.
func TestDeliveriesReuseConnectionsWithinAPolicy(t *testing.T) {
	d := dbtest.SQLite(t)
	e := newEnv(t, d)
	ctx := t.Context()
	var allow atomic.Bool
	allow.Store(true)
	sealer, err := secrets.New([]byte("0123456789abcdef0123456789abcdef"))
	require.NoError(t, err)
	reg := jobs.NewRegistry()
	q := jobstest.NewFake(reg)
	svc := webhook.New(webhook.Options{DB: d, Queue: q, Sealer: sealer, Now: func() time.Time { return *e.clock },
		AllowPrivateTargets: func(context.Context) (bool, error) { return allow.Load(), nil }})
	require.NoError(t, svc.Register(reg))
	hook, err := svc.Create(ctx, webhook.CreateParams{NamespaceID: &e.nsID, URL: e.srv.URL, Secret: "keep-alive-secret",
		Events: []string{"repo.created"}, Enabled: true})
	require.NoError(t, err)

	for range 3 {
		_, err := svc.Test(ctx, hook.ID, indextest.Owner("alice"))
		require.NoError(t, err)
		q.RunPending(ctx)
	}
	e.recv.mu.Lock()
	require.Len(t, e.recv.got, 3)
	for _, r := range e.recv.got[1:] {
		require.Equal(t, e.recv.got[0].RemoteAddr, r.RemoteAddr, "every delivery uses the same kept-alive connection")
	}
	e.recv.mu.Unlock()

	allow.Store(false)
	del, err := svc.Test(ctx, hook.ID, indextest.Owner("alice"))
	require.NoError(t, err)
	q.RunPending(ctx)
	list, _, err := svc.Deliveries(ctx, hook.ID, "", 1)
	require.NoError(t, err)
	require.Equal(t, del.ID, list[0].ID)
	require.Equal(t, "failed", list[0].State())
	require.Equal(t, webhook.ErrPrivateTarget.Error(), list[0].Error)
	e.recv.mu.Lock()
	require.Len(t, e.recv.got, 3, "the kept-alive connection to the private receiver is not reused")
	e.recv.mu.Unlock()
}

func TestUnreachableReceiverErrorsHideTheURL(t *testing.T) {
	d := dbtest.SQLite(t)
	e := newEnv(t, d)
	ctx := t.Context()
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL + "/hook?token=hunter2"
	srv.Close()
	hook, err := e.svc.Create(ctx, webhook.CreateParams{NamespaceID: &e.nsID, URL: url, Secret: "closed-secret",
		Events: []string{"repo.created"}, Enabled: true})
	require.NoError(t, err)
	require.NoError(t, e.svc.Emit(ctx, e.event(webhook.RepoCreated)))
	e.q.RunPending(ctx)
	list, _, err := e.svc.Deliveries(ctx, hook.ID, "", 1)
	require.NoError(t, err)
	require.Equal(t, "pending", list[0].State())
	require.NotEmpty(t, list[0].Error)
	require.NotContains(t, list[0].Error, "hunter2")
	require.Nil(t, list[0].StatusCode)
}

func TestHandleJobEdgeCases(t *testing.T) {
	d := dbtest.SQLite(t)
	e := newEnv(t, d)
	ctx := t.Context()
	err := e.svc.HandleJob(ctx, &jobs.Job{Payload: []byte("{")}, nil)
	require.True(t, jobs.IsPermanent(err))
	require.NoError(t, e.svc.HandleJob(ctx, &jobs.Job{Payload: []byte(`{"delivery_id":"` + uuid.NewString() + `"}`)}, nil))

	other, _ := secrets.New([]byte("ffffffffffffffffffffffffffffffff"))
	hook, err := e.svc.Create(ctx, webhook.CreateParams{NamespaceID: &e.nsID, URL: e.srv.URL, Secret: "rotated-secret",
		Events: []string{"repo.created"}, Enabled: true})
	require.NoError(t, err)
	sealer, _ := secrets.New([]byte("0123456789abcdef0123456789abcdef"))
	n, err := webhook.ReencryptSecrets(ctx, d, sealer, other)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.NoError(t, e.svc.Emit(ctx, e.event(webhook.RepoCreated)))
	e.q.RunPending(ctx)
	list, _, err := e.svc.Deliveries(ctx, hook.ID, "", 1)
	require.NoError(t, err)
	require.Equal(t, "failed", list[0].State(), "a secret sealed with another key cannot be used")
	require.Contains(t, list[0].Error, "Instance key")
	_, err = webhook.ReencryptSecrets(ctx, d, sealer, other)
	require.ErrorIs(t, err, secrets.ErrDecrypt)
}

func TestIsPrivate(t *testing.T) {
	for _, a := range []string{"127.0.0.1", "10.1.2.3", "192.168.1.1", "172.16.0.1", "169.254.169.254", "100.64.0.1", "::1", "fd00::1", "fe80::1", "0.0.0.0", "::ffff:10.0.0.1"} {
		require.True(t, webhook.IsPrivate(netip.MustParseAddr(a)), a)
	}
	for _, a := range []string{"8.8.8.8", "1.1.1.1", "2606:4700::1111"} {
		require.False(t, webhook.IsPrivate(netip.MustParseAddr(a)), a)
	}
	require.True(t, webhook.ValidEvent("pr.merged"))
	require.False(t, webhook.ValidEvent("ping"))
	require.True(t, errors.Is(webhook.ErrPrivateTarget, webhook.ErrPrivateTarget))
}

// TestReservedTargetsAreAlwaysRefused checks that webhooks never reach
// link-local, metadata or unspecified addresses, even with private targets
// allowed (the default). Before, http://0.0.0.0:<port>/ delivered to the
// receiver on the local host, and 169.254.169.254 (the cloud metadata
// service) was dialled.
func TestReservedTargetsAreAlwaysRefused(t *testing.T) {
	d := dbtest.SQLite(t)
	e := newEnv(t, d)
	ctx := t.Context()
	sealer, _ := secrets.New([]byte("0123456789abcdef0123456789abcdef"))
	reg := jobs.NewRegistry()
	q := jobstest.NewFake(reg)
	open := webhook.New(webhook.Options{DB: d, Queue: q, Sealer: sealer, Now: func() time.Time { return *e.clock },
		AllowPrivateTargets: func(context.Context) (bool, error) { return true, nil }})
	require.NoError(t, open.Register(reg))
	port := netip.MustParseAddrPort(e.srv.Listener.Addr().String()).Port()
	for _, u := range []string{
		"http://0.0.0.0:" + itoa(port) + "/",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::ffff:169.254.169.254]/latest/meta-data/",
		"http://[fe80::1]/",
		"http://100.100.100.200/latest/meta-data/",
	} {
		hook, err := open.Create(ctx, webhook.CreateParams{NamespaceID: &e.nsID, URL: u, Secret: "ping-secret", Events: []string{"repo.created"}, Enabled: true})
		require.NoError(t, err, u)
		del, err := open.Test(ctx, hook.ID, indextest.Owner("alice"))
		require.NoError(t, err)
		q.RunPending(ctx)
		list, _, err := open.Deliveries(ctx, hook.ID, "", 1)
		require.NoError(t, err)
		require.Equal(t, del.ID, list[0].ID)
		require.Equal(t, "failed", list[0].State(), u)
		require.Equal(t, webhook.ErrReservedTarget.Error(), list[0].Error, u)
	}
	require.Empty(t, e.recv.got, "the local receiver was never reached through 0.0.0.0")

	// The loopback receiver itself is a private target, which this
	// Instance allows.
	hook, err := open.Create(ctx, webhook.CreateParams{NamespaceID: &e.nsID, URL: e.srv.URL, Secret: "ping-secret", Events: []string{"repo.created"}, Enabled: true})
	require.NoError(t, err)
	_, err = open.Test(ctx, hook.ID, indextest.Owner("alice"))
	require.NoError(t, err)
	q.RunPending(ctx)
	require.Len(t, e.recv.got, 1)
}

func itoa(n uint16) string { return strconv.Itoa(int(n)) }
