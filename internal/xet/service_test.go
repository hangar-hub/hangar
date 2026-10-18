package xet_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/hangar-hub/hangar/internal/blob/blobtest"
	"github.com/hangar-hub/hangar/internal/config"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/db/dbtest"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/repo/repotest"
	"github.com/hangar-hub/hangar/internal/telemetry"
	"github.com/hangar-hub/hangar/internal/xet"
	"github.com/hangar-hub/hangar/internal/xet/xettest"
)

var testKey = bytes.Repeat([]byte{7}, 32)

// gcLFS is the fake LFS store with objects that LFS GC has removed.
type gcLFS struct {
	repo.LFSStore
	mu   sync.Mutex
	gone map[string]bool
}

func (g *gcLFS) remove(oid string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.gone[oid] = true
}

func (g *gcLFS) Stat(ctx context.Context, oid string) (repo.LFSObject, error) {
	g.mu.Lock()
	gone := g.gone[oid]
	g.mu.Unlock()
	if gone {
		return repo.LFSObject{}, repo.ErrNotFound
	}
	return g.LFSStore.Stat(ctx, oid)
}

func (g *gcLFS) StatMany(ctx context.Context, oids []string) (map[string]repo.LFSObject, error) {
	out, err := g.LFSStore.StatMany(ctx, oids)
	g.mu.Lock()
	defer g.mu.Unlock()
	for oid := range out {
		if g.gone[oid] {
			delete(out, oid)
		}
	}
	return out, err
}

type env struct {
	svc    *xet.Service
	db     *db.DB
	blobs  *blobtest.Fake
	lfs    *repotest.FakeLFS
	gc     *gcLFS
	reader *sdkmetric.ManualReader
	mu     sync.Mutex
	now    time.Time
	claims *xet.Claims
}

func (e *env) clock() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.now
}

func (e *env) advance(d time.Duration) {
	e.mu.Lock()
	e.now = e.now.Add(d)
	e.mu.Unlock()
}

func newEnv(t *testing.T, d *db.DB, tweak ...func(*xet.Options)) *env {
	t.Helper()
	e := &env{db: d, blobs: blobtest.NewFake(), now: time.Now().UTC().Truncate(time.Second)}
	e.lfs = repotest.NewFakeLFS(e.blobs)
	e.gc = &gcLFS{LFSStore: e.lfs, gone: map[string]bool{}}
	e.reader = sdkmetric.NewManualReader()
	inst, err := telemetry.NewInstruments(sdkmetric.NewMeterProvider(sdkmetric.WithReader(e.reader)).Meter("test"))
	require.NoError(t, err)
	o := xet.Options{
		DB: d, Blobs: e.blobs, LFS: e.gc, InstanceKey: testKey, CASURL: "https://hangar.example.com/xet-cas",
		TempDir: t.TempDir(), Instruments: inst, Now: e.clock,
	}
	for _, fn := range tweak {
		fn(&o)
	}
	e.svc, err = xet.New(o)
	require.NoError(t, err)
	tok, err := e.svc.IssueWriteToken(context.Background(), xet.TokenRequest{Principal: alice(), Repo: aliceRepo, RepoUUID: uuid.New(), Revision: "main"})
	require.NoError(t, err)
	e.claims, err = e.svc.VerifyToken(context.Background(), tok.AccessToken)
	require.NoError(t, err)
	return e
}

func alice() model.Principal {
	return model.Principal{Kind: model.PrincipalUser, UserID: uuid.MustParse("01920000-0000-7000-8000-000000000001"), Username: "alice"}
}

var aliceRepo = model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "tiny"}

// counter sums a counter's data points with the given outcome ("" for any).
func (e *env) counter(t *testing.T, name, outcome string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, e.reader.Collect(context.Background(), &rm))
	var n int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			for _, p := range m.Data.(metricdata.Sum[int64]).DataPoints {
				v, _ := p.Attributes.Value(telemetry.MetricOutcome)
				if outcome == "" || v.AsString() == outcome {
					n += p.Value
				}
			}
		}
	}
	return n
}

func (e *env) putAll(t *testing.T, u *xettest.Upload) {
	t.Helper()
	for _, x := range u.Xorbs {
		inserted, err := e.svc.PutXorb(context.Background(), e.claims, x.Hash.String(), bytes.NewReader(x.Body), int64(len(x.Body)))
		require.NoError(t, err)
		require.True(t, inserted)
	}
}

func (e *env) accept(t *testing.T, u *xettest.Upload) (*xet.ShardResult, []xet.ShardEvent, error) {
	t.Helper()
	b := u.ShardBytes()
	var events []xet.ShardEvent
	res, err := e.svc.AcceptShard(context.Background(), e.claims, bytes.NewReader(b), int64(len(b)), func(ev xet.ShardEvent) {
		events = append(events, ev)
	})
	return res, events, err
}

func (e *env) objectBytes(t *testing.T, oid string) []byte {
	t.Helper()
	rc, _, err := e.lfs.Open(context.Background(), oid, nil)
	require.NoError(t, err)
	defer rc.Close()
	b, err := io.ReadAll(rc)
	require.NoError(t, err)
	return b
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestTokens(t *testing.T) {
	d := dbtest.SQLite(t)
	e := newEnv(t, d)
	ctx := context.Background()
	repoUUID := uuid.New()

	tok, err := e.svc.IssueWriteToken(ctx, xet.TokenRequest{Principal: alice(), Repo: aliceRepo, RepoUUID: repoUUID, Revision: "refs/pr/3", CreatePR: true})
	require.NoError(t, err)
	require.Equal(t, "https://hangar.example.com/xet-cas", tok.CASURL)
	require.Equal(t, e.now.Add(time.Hour).Unix(), tok.Exp, "exp is accurate: hf_xet refreshes 30 s before it")
	require.True(t, strings.HasPrefix(tok.AccessToken, "hxet1."))

	c, err := e.svc.VerifyToken(ctx, tok.AccessToken)
	require.NoError(t, err)
	require.Equal(t, xet.Claims{UserID: alice().UserID, Username: "alice", Repo: aliceRepo, RepoUUID: repoUUID,
		Revision: "refs/pr/3", CreatePR: true, Scope: xet.ScopeWrite, Expires: e.now.Add(time.Hour)}, *c)

	// Valid until the expiry plus the clock skew allowance, then rejected.
	e.advance(time.Hour + xet.DefaultClockSkew)
	_, err = e.svc.VerifyToken(ctx, tok.AccessToken)
	require.NoError(t, err)
	e.advance(time.Second)
	_, err = e.svc.VerifyToken(ctx, tok.AccessToken)
	require.ErrorIs(t, err, xet.ErrInvalidToken)

	// Tampering, other keys and garbage are rejected.
	fresh, err := e.svc.IssueWriteToken(ctx, xet.TokenRequest{Principal: alice(), Repo: aliceRepo, Revision: "main"})
	require.NoError(t, err)
	payload, sig, _ := strings.Cut(strings.TrimPrefix(fresh.AccessToken, "hxet1."), ".")
	for _, bad := range []string{
		"", "hf_token", "hxet1.", "hxet1." + payload, "hxet1." + payload + "." + sig + "x",
		"hxet1.e30." + sig, "hxet1.!!!." + sig, strings.Repeat("a", 5000),
	} {
		_, err := e.svc.VerifyToken(ctx, bad)
		require.ErrorIs(t, err, xet.ErrInvalidToken, bad)
	}
	other, err := xet.New(xet.Options{DB: d, Blobs: e.blobs, LFS: e.lfs, InstanceKey: bytes.Repeat([]byte{8}, 32), CASURL: "https://h/xet-cas", Now: e.clock})
	require.NoError(t, err)
	_, err = other.VerifyToken(ctx, fresh.AccessToken)
	require.ErrorIs(t, err, xet.ErrInvalidToken)

	_, err = e.svc.IssueWriteToken(ctx, xet.TokenRequest{Principal: model.Anonymous(), Repo: aliceRepo})
	require.ErrorIs(t, err, xet.ErrScope)

	rejSvc, err := xet.New(xet.Options{DB: d, Blobs: e.blobs, LFS: e.lfs, InstanceKey: testKey, CASURL: "https://h/xet-cas", Reject: true})
	require.NoError(t, err)
	_, err = rejSvc.IssueWriteToken(ctx, xet.TokenRequest{Principal: alice(), Repo: aliceRepo})
	require.ErrorIs(t, err, xet.ErrRejected)
	_, err = rejSvc.PutXorb(ctx, e.claims, strings.Repeat("a", 64), strings.NewReader(""), 0)
	require.ErrorIs(t, err, xet.ErrRejected)
	_, err = rejSvc.AcceptShard(ctx, e.claims, strings.NewReader(""), 0, nil)
	require.ErrorIs(t, err, xet.ErrRejected)
}

func TestNewAndOptionsFromConfig(t *testing.T) {
	_, err := xet.New(xet.Options{})
	require.ErrorContains(t, err, "DB, Blobs, LFS, CASURL")
	_, err = xet.New(xet.Options{DB: dbtest.SQLite(t), Blobs: blobtest.NewFake(), LFS: repotest.NewFakeLFS(blobtest.NewFake()), CASURL: "x", InstanceKey: []byte("short")})
	require.Error(t, err)

	cfg := config.Default()
	cfg.Server.PublicURL = "https://hangar.example.com/"
	cfg.Security.InstanceKey = "BwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwc="
	cfg.HFAPI.Xet = config.XetReject
	o, err := xet.OptionsFromConfig(&cfg)
	require.NoError(t, err)
	require.Equal(t, "https://hangar.example.com/xet-cas", o.CASURL)
	require.True(t, o.Reject)
	require.Equal(t, time.Hour, o.TokenTTL)
	require.Equal(t, 672*time.Hour, o.XorbRetention)
	require.Equal(t, xet.Limits{MaxFileSize: xet.DefaultMaxFileSize}, o.Limits)
	require.Len(t, o.InstanceKey, 32)
	cfg.Xet.CASURL = "https://cas.example.com"
	o, err = xet.OptionsFromConfig(&cfg)
	require.NoError(t, err)
	require.Equal(t, "https://cas.example.com", o.CASURL)
	cfg.Security.InstanceKey = "not base64!"
	_, err = xet.OptionsFromConfig(&cfg)
	require.Error(t, err)
}

func TestPutXorb(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d)
		ctx := context.Background()
		r := rand.New(rand.NewPCG(5, 6))
		x := xettest.BuildXorb([][]byte{randomBytes(r, 5000), textBytes(9000, 2)}, true, xet.CompressionLZ4, xet.CompressionBG4LZ4)

		inserted, err := e.svc.PutXorb(ctx, e.claims, x.Hash.String(), bytes.NewReader(x.Body), int64(len(x.Body)))
		require.NoError(t, err)
		require.True(t, inserted)
		stored, ok := e.blobs.Bytes("xet/xorbs/" + x.Hash.String())
		require.True(t, ok)
		idx, err := xet.UnmarshalIndex(stored)
		require.NoError(t, err)
		require.Equal(t, x.Hash, idx.Hash())
		require.True(t, bytes.HasSuffix(x.Body[:idx.DataSize], stored[len(stored)-int(idx.DataSize):]), "the chunk data is stored as uploaded, without the footer")

		var row db.XetXorb
		require.NoError(t, d.NewSelect().Model(&row).Where("hash = ?", x.Hash.String()).Scan(ctx))
		require.Equal(t, int64(len(stored)), row.Size)
		require.Equal(t, "xet/xorbs/"+x.Hash.String(), row.StorageKey)
		require.True(t, row.LastReferencedAt.Equal(e.now))

		// Uploading it again is idempotent and restarts its retention clock.
		e.advance(time.Hour)
		puts := e.blobs.Calls("Put")
		inserted, err = e.svc.PutXorb(ctx, e.claims, x.Hash.String(), bytes.NewReader(x.Body), int64(len(x.Body)))
		require.NoError(t, err)
		require.False(t, inserted)
		require.Equal(t, puts, e.blobs.Calls("Put"), "an existing xorb is not stored again")
		require.NoError(t, d.NewSelect().Model(&row).Where("hash = ?", x.Hash.String()).Scan(ctx))
		require.True(t, row.LastReferencedAt.Equal(e.now))

		// A body that does not hash to the path's hash is never stored.
		other := xettest.BuildXorb([][]byte{[]byte("different")}, false)
		_, err = e.svc.PutXorb(ctx, e.claims, other.Hash.String(), bytes.NewReader(x.Body), int64(len(x.Body)))
		require.ErrorIs(t, err, xet.ErrBadXorb)
		_, ok = e.blobs.Bytes("xet/xorbs/" + other.Hash.String())
		require.False(t, ok)

		_, err = e.svc.PutXorb(ctx, e.claims, "NOTAHASH", bytes.NewReader(x.Body), -1)
		require.ErrorIs(t, err, xet.ErrBadXorb)
		_, err = e.svc.PutXorb(ctx, e.claims, other.Hash.String(), bytes.NewReader([]byte{1, 2, 3}), 3)
		require.ErrorIs(t, err, xet.ErrBadXorb)
		_, err = e.svc.PutXorb(ctx, e.claims, other.Hash.String(), bytes.NewReader(nil), xet.MaxXorbSize+1)
		require.ErrorIs(t, err, xet.ErrTooLarge)
		// A body over the limit with no declared length: valid chunks, then
		// an endless footer.
		huge := io.MultiReader(bytes.NewReader(other.Body), strings.NewReader("XETBLOB"), io.LimitReader(zeroReader{}, xet.MaxXorbSize))
		_, err = e.svc.PutXorb(ctx, e.claims, other.Hash.String(), huge, -1)
		require.ErrorIs(t, err, xet.ErrTooLarge)
		read := *e.claims
		read.Scope = xet.ScopeRead
		_, err = e.svc.PutXorb(ctx, &read, other.Hash.String(), bytes.NewReader(other.Body), -1)
		require.ErrorIs(t, err, xet.ErrScope)
		_, err = e.svc.PutXorb(ctx, nil, other.Hash.String(), bytes.NewReader(other.Body), -1)
		require.ErrorIs(t, err, xet.ErrScope)

		require.Equal(t, int64(1), e.counter(t, "hangar.xet.xorbs", "inserted"))
		require.Equal(t, int64(1), e.counter(t, "hangar.xet.xorbs", "existing"))
		require.GreaterOrEqual(t, e.counter(t, "hangar.xet.xorbs", "rejected"), int64(4))
	})
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestAcceptShardReassembles(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d)
		ctx := context.Background()
		r := rand.New(rand.NewPCG(7, 8))
		files := [][]byte{randomBytes(r, 200_000), textBytes(150_000, 3), {}, randomBytes(r, 1)}
		u := xettest.BuildUpload(files, xettest.UploadOptions{ChunkSize: 8 << 10, ChunksPerXorb: 7,
			Schemes: []xet.CompressionScheme{xet.CompressionBG4LZ4, xet.CompressionNone, xet.CompressionLZ4}})
		require.Greater(t, len(u.Xorbs), 3)
		e.putAll(t, u)

		res, events, err := e.accept(t, u)
		require.NoError(t, err)
		require.Equal(t, 1, res.Result)
		require.Len(t, res.Files, len(files))
		for i, data := range files {
			f := res.Files[i]
			require.Equal(t, sha(data), f.OID)
			require.Equal(t, u.Files[i].SHA256, f.OID)
			require.Equal(t, int64(len(data)), f.Size)
			require.Equal(t, u.Files[i].XetHash.String(), f.XetHash)
			require.Equal(t, data, e.objectBytes(t, f.OID), "file %d", i)
			var row db.XetFile
			require.NoError(t, d.NewSelect().Model(&row).Where("xet_hash = ?", f.XetHash).Scan(ctx))
			require.Equal(t, f.OID, row.OID)
			require.Equal(t, f.Size, row.Size)
		}
		// Progress: validating counts up, then committing, then the result.
		require.Equal(t, xet.ShardEvent{Type: xet.EventValidating, Total: len(files) + len(u.Xorbs)}, events[0])
		last := events[len(events)-1]
		require.Equal(t, xet.EventResult, last.Type)
		require.Contains(t, events, xet.ShardEvent{Type: xet.EventCommitting, Stage: xet.StageUploading})
		require.Contains(t, events, xet.ShardEvent{Type: xet.EventValidating, Verified: len(files) + len(u.Xorbs), Total: len(files) + len(u.Xorbs)})
		require.Equal(t, int64(350_001), e.counter(t, "hangar.xet.reassembly.bytes", ""))

		// The same shard again needs no reassembly: the files are known.
		puts := e.blobs.Calls("Put")
		res2, _, err := e.accept(t, u)
		require.NoError(t, err)
		require.Equal(t, res.Files, res2.Files)
		require.Equal(t, puts, e.blobs.Calls("Put"))
	})
}

// TestAcceptShardDeduplicatedUpload is the second-Repo case: the client
// knows the xorbs from its cache and sends only a shard that names them.
func TestAcceptShardDeduplicatedUpload(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d)
		ctx := context.Background()
		data := textBytes(100_000, 9)
		first := xettest.BuildUpload([][]byte{data}, xettest.UploadOptions{ChunkSize: 4096})
		e.putAll(t, first)
		_, _, err := e.accept(t, first)
		require.NoError(t, err)

		e.advance(48 * time.Hour)
		second := xettest.BuildUpload([][]byte{data}, xettest.UploadOptions{ChunkSize: 4096, Known: first.Xorbs})
		require.Empty(t, second.Xorbs, "no xorbs are sent again")
		require.Empty(t, second.Shard.Xorbs)
		tok, err := e.svc.IssueWriteToken(ctx, xet.TokenRequest{Principal: alice(), Repo: model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "second"}, Revision: "main"})
		require.NoError(t, err)
		e.claims, err = e.svc.VerifyToken(ctx, tok.AccessToken)
		require.NoError(t, err)
		res, _, err := e.accept(t, second)
		require.NoError(t, err)
		require.Equal(t, sha(data), res.Files[0].OID)

		// Naming the xorb in a shard restarted its retention clock.
		var row db.XetXorb
		require.NoError(t, d.NewSelect().Model(&row).Where("hash = ?", first.Xorbs[0].Hash.String()).Scan(ctx))
		require.True(t, row.LastReferencedAt.Equal(e.now))

		// An existing object with no file record is checked by rebuilding it.
		_, err = d.NewDelete().Model((*db.XetFile)(nil)).Where("1 = 1").Exec(ctx)
		require.NoError(t, err)
		res, _, err = e.accept(t, second)
		require.NoError(t, err)
		require.Equal(t, sha(data), res.Files[0].OID)
	})
}

func TestAcceptShardRejects(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d)
		ctx := context.Background()
		r := rand.New(rand.NewPCG(9, 10))
		data := randomBytes(r, 50_000)
		build := func() *xettest.Upload {
			return xettest.BuildUpload([][]byte{data}, xettest.UploadOptions{ChunkSize: 4096, ChunksPerXorb: 5})
		}

		// A xorb Hangar does not have.
		u := build()
		_, _, err := e.accept(t, u)
		require.ErrorIs(t, err, xet.ErrUnknownXorb)
		e.putAll(t, u)

		// A wrong SHA-256 claim is caught by rebuilding, and nothing is stored.
		u = build()
		wrong := xettest.SHA256Hash([]byte("not the data"))
		u.Shard.Files[0].SHA256 = &wrong
		_, _, err = e.accept(t, u)
		require.ErrorIs(t, err, xet.ErrBadShard)
		_, err = e.lfs.Stat(ctx, wrong.String())
		require.Error(t, err)
		_, err = e.lfs.Stat(ctx, sha(data))
		require.Error(t, err, "the real object was not stored under a wrong claim either")

		// Claiming another object's SHA-256: that object exists, so the
		// claim is checked by rebuilding without storing.
		otherData := []byte("someone else's private file")
		other, err := e.lfs.Put(ctx, otherData)
		require.NoError(t, err)
		u = build()
		claim, err := xet.ParseHash(other.OID)
		require.NoError(t, err)
		u.Shard.Files[0].SHA256 = &claim
		_, _, err = e.accept(t, u)
		require.ErrorIs(t, err, xet.ErrBadShard)
		require.NotContains(t, err.Error(), strconv.Itoa(len(otherData)), "the answer does not give the stored object's size")
		// With the right size too.
		short := data[:len(otherData)]
		su := xettest.BuildUpload([][]byte{short}, xettest.UploadOptions{ChunkSize: 4096})
		e.putAll(t, su)
		su.Shard.Files[0].SHA256 = &claim
		_, _, err = e.accept(t, su)
		require.ErrorIs(t, err, xet.ErrBadShard)
		n, err := d.NewSelect().Model((*db.XetFile)(nil)).Count(ctx)
		require.NoError(t, err)
		require.Zero(t, n)

		// Wrong verification hash, wrong file hash, CAS info mismatch.
		u = build()
		u.Shard.Files[0].Verification[0][3] ^= 1
		_, _, err = e.accept(t, u)
		require.ErrorIs(t, err, xet.ErrBadShard)
		u = build()
		u.Shard.Files[0].Hash[0] ^= 1
		_, _, err = e.accept(t, u)
		require.ErrorIs(t, err, xet.ErrBadShard)
		u = build()
		u.Shard.Xorbs[0].Chunks[0].Size++
		_, _, err = e.accept(t, u)
		require.ErrorIs(t, err, xet.ErrBadShard)
		u = build()
		u.Shard.Files[0].SHA256 = nil
		_, _, err = e.accept(t, u)
		require.ErrorIs(t, err, xet.ErrBadShard)

		// Malformed, too large and unauthorised shards.
		_, err = e.svc.AcceptShard(ctx, e.claims, strings.NewReader("garbage"), 7, nil)
		require.ErrorIs(t, err, xet.ErrBadShard)
		_, err = e.svc.AcceptShard(ctx, e.claims, strings.NewReader(""), xet.MaxShardSize+1, nil)
		require.ErrorIs(t, err, xet.ErrTooLarge)
		_, err = e.svc.AcceptShard(ctx, e.claims, io.LimitReader(zeroReader{}, xet.MaxShardSize+1), -1, nil)
		require.ErrorIs(t, err, xet.ErrTooLarge)
		read := *e.claims
		read.Scope = xet.ScopeRead
		_, err = e.svc.AcceptShard(ctx, &read, bytes.NewReader(build().ShardBytes()), -1, nil)
		require.ErrorIs(t, err, xet.ErrScope)

		// A row whose object vanished is treated as unknown.
		u = build()
		require.NoError(t, e.blobs.Delete(ctx, xet.XorbKey(u.Xorbs[0].Hash)))
		_, _, err = e.accept(t, u)
		require.ErrorIs(t, err, xet.ErrUnknownXorb)
	})
}

func TestAcceptShardDetectsCorruptStorage(t *testing.T) {
	d := dbtest.SQLite(t)
	e := newEnv(t, d)
	ctx := context.Background()
	data := textBytes(30_000, 4)
	u := xettest.BuildUpload([][]byte{data}, xettest.UploadOptions{ChunkSize: 4096})
	e.putAll(t, u)
	key := xet.XorbKey(u.Xorbs[0].Hash)
	stored, _ := e.blobs.Bytes(key)
	// Flip a byte in the last chunk's payload: the index still matches.
	corrupt := bytes.Clone(stored)
	corrupt[len(corrupt)-2] ^= 0xff
	require.NoError(t, e.blobs.Put(ctx, key, bytes.NewReader(corrupt), int64(len(corrupt))))
	_, _, err := e.accept(t, u)
	require.ErrorContains(t, err, "corrupt")
	_, err = e.lfs.Stat(ctx, sha(data))
	require.Error(t, err)

	// A broken index is reported too.
	require.NoError(t, e.blobs.Put(ctx, key, bytes.NewReader(stored[:10]), 10))
	_, _, err = e.accept(t, u)
	require.Error(t, err)
}

// TestXetUploadsRecordUploadersAndRevealNothing covers the Xet side of the
// decision James made on 2026-10-18 ("check at Commit"): a shard whose
// file rebuilds into an LFS object records the token's User as an
// uploader of it, also when the object was stored by someone else, which
// stores no second copy; and a xorb that is already stored is accepted
// only with its own bytes, so a xorb upload says whether Hangar has a xorb
// only to a client that holds it.
func TestXetUploadsRecordUploadersAndRevealNothing(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		e := newEnv(t, d)
		ctx := context.Background()
		r := rand.New(rand.NewPCG(11, 12))
		data := randomBytes(r, 60_000)
		first := xettest.BuildUpload([][]byte{data}, xettest.UploadOptions{ChunkSize: 4096, ChunksPerXorb: 5})
		e.putAll(t, first)
		_, _, err := e.accept(t, first)
		require.NoError(t, err)
		oid := sha(data)
		uploaded := func(user uuid.UUID) bool {
			t.Helper()
			got, err := e.lfs.Uploaded(ctx, user, []string{oid})
			require.NoError(t, err)
			return got[oid]
		}
		require.True(t, uploaded(alice().UserID))

		// bob uploads the same file to his own Repo: every xorb is sent
		// again and checked, nothing is stored twice, and bob becomes an
		// uploader.
		bob := model.Principal{Kind: model.PrincipalUser, UserID: uuid.New(), Username: "bob"}
		require.False(t, uploaded(bob.UserID))
		tok, err := e.svc.IssueWriteToken(ctx, xet.TokenRequest{Principal: bob, Repo: model.RepoID{Type: model.RepoTypeModel, Namespace: "bob", Name: "copy"}, Revision: "main"})
		require.NoError(t, err)
		e.claims, err = e.svc.VerifyToken(ctx, tok.AccessToken)
		require.NoError(t, err)
		puts := e.blobs.Calls("Put")
		for _, x := range first.Xorbs {
			inserted, err := e.svc.PutXorb(ctx, e.claims, x.Hash.String(), bytes.NewReader(x.Body), int64(len(x.Body)))
			require.NoError(t, err)
			require.False(t, inserted)
		}
		_, _, err = e.accept(t, xettest.BuildUpload([][]byte{data}, xettest.UploadOptions{ChunkSize: 4096, ChunksPerXorb: 5}))
		require.NoError(t, err)
		require.Equal(t, puts, e.blobs.Calls("Put"), "no xorb or object is stored again")
		require.True(t, uploaded(bob.UserID))

		// A stored xorb's hash with other bytes is refused, as an unknown
		// one would be.
		x := first.Xorbs[0]
		other := xettest.BuildXorb([][]byte{[]byte("not the stored xorb")}, false)
		_, err = e.svc.PutXorb(ctx, e.claims, x.Hash.String(), bytes.NewReader(other.Body), int64(len(other.Body)))
		require.ErrorIs(t, err, xet.ErrBadXorb)
		unknown := xettest.BuildXorb([][]byte{[]byte("a xorb nobody stored")}, false)
		_, err = e.svc.PutXorb(ctx, e.claims, unknown.Hash.String(), bytes.NewReader(other.Body), int64(len(other.Body)))
		require.ErrorIs(t, err, xet.ErrBadXorb)
	})
}
