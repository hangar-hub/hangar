package xet

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/hangar-hub/hangar/internal/blob"
	"github.com/hangar-hub/hangar/internal/config"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/telemetry"
)

// Defaults for Options.
const (
	DefaultTokenTTL      = time.Hour
	DefaultXorbRetention = 672 * time.Hour
	DefaultClockSkew     = 30 * time.Second
)

// Options configure a Service.
type Options struct {
	// DB holds xet_xorbs and xet_files.
	DB *db.DB
	// Blobs stores xorbs under xet/xorbs/<hash>.
	Blobs blob.Store
	// LFS stores the reassembled files as LFS objects.
	LFS repo.LFSStore
	// InstanceKey (security.instance_key, decoded) signs CAS tokens.
	InstanceKey []byte
	// CASURL is the casUrl given to clients: xet.cas_url or
	// <public_url>/xet-cas.
	CASURL string
	// TokenTTL is xet.token_ttl. Zero means DefaultTokenTTL.
	TokenTTL time.Duration
	// XorbRetention is xet.xorb_retention. Zero means DefaultXorbRetention.
	XorbRetention time.Duration
	// Reject is true when hfapi.xet is reject.
	Reject bool
	// ClockSkew is how long after its expiry a token is still accepted, for
	// replicas whose clocks differ. Zero means DefaultClockSkew.
	ClockSkew time.Duration
	// Limits bound the work one shard can cause. Zero fields take their
	// defaults (Limits.WithDefaults); MaxFileSize is xet.max_file_size.
	Limits Limits
	// TempDir is where xorb bodies are spooled while they are verified.
	// Empty means the system temporary directory.
	TempDir string
	// Instruments records the hangar.xet.* metrics; nil records none.
	Instruments *telemetry.Instruments
	// Tracer creates spans; nil uses the global tracer provider.
	Tracer trace.Tracer
	// Log receives operational messages; nil discards them.
	Log *slog.Logger
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// OptionsFromConfig fills the settings from the configuration. The caller
// sets DB, Blobs, LFS, Instruments, Tracer and Log.
func OptionsFromConfig(c *config.Config) (Options, error) {
	key, err := base64.StdEncoding.DecodeString(c.Security.InstanceKey)
	if err != nil {
		return Options{}, fmt.Errorf("xet: decode security.instance_key: %w", err)
	}
	return Options{
		InstanceKey:   key,
		CASURL:        c.CASURL(),
		TokenTTL:      c.Xet.TokenTTL,
		XorbRetention: c.Xet.XorbRetention,
		Reject:        c.HFAPI.Xet == config.XetReject,
		Limits:        Limits{MaxFileSize: int64(c.Xet.MaxFileSize)},
		TempDir:       os.TempDir(),
	}, nil
}

// Service is the Xet write path: the real Ingestor.
type Service struct {
	db        *db.DB
	blobs     blob.Store
	lfs       repo.LFSStore
	signer    *tokenSigner
	casURL    string
	ttl       time.Duration
	retention time.Duration
	reject    bool
	skew      time.Duration
	tempDir   string
	limits    Limits
	inst      *telemetry.Instruments
	tracer    trace.Tracer
	log       *slog.Logger
	now       func() time.Time
}

var _ Ingestor = (*Service)(nil)

// New returns a Service. DB, Blobs, LFS, InstanceKey and CASURL are required.
func New(o Options) (*Service, error) {
	var missing []string
	if o.DB == nil {
		missing = append(missing, "DB")
	}
	if o.Blobs == nil {
		missing = append(missing, "Blobs")
	}
	if o.LFS == nil {
		missing = append(missing, "LFS")
	}
	if o.CASURL == "" {
		missing = append(missing, "CASURL")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("xet: missing options: %s", strings.Join(missing, ", "))
	}
	signer, err := newTokenSigner(o.InstanceKey)
	if err != nil {
		return nil, err
	}
	s := &Service{
		db: o.DB, blobs: o.Blobs, lfs: o.LFS, signer: signer, casURL: strings.TrimRight(o.CASURL, "/"),
		ttl: o.TokenTTL, retention: o.XorbRetention, reject: o.Reject, skew: o.ClockSkew, tempDir: o.TempDir,
		limits: o.Limits.WithDefaults(), inst: o.Instruments, tracer: o.Tracer, log: o.Log, now: o.Now,
	}
	if s.ttl <= 0 {
		s.ttl = DefaultTokenTTL
	}
	if s.retention <= 0 {
		s.retention = DefaultXorbRetention
	}
	if s.skew <= 0 {
		s.skew = DefaultClockSkew
	}
	if s.tracer == nil {
		s.tracer = otel.Tracer(telemetry.ScopeName)
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s, nil
}

// clock returns the current time in the database's precision.
func (s *Service) clock() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

// IssueWriteToken implements Ingestor.
func (s *Service) IssueWriteToken(_ context.Context, req TokenRequest) (Token, error) {
	if s.reject {
		return Token{}, ErrRejected
	}
	if req.Principal.IsAnonymous() {
		return Token{}, fmt.Errorf("%w: sign in to upload", ErrScope)
	}
	exp := s.now().Add(s.ttl).Truncate(time.Second).UTC()
	tok, err := s.signer.issue(Claims{
		UserID: req.Principal.UserID, Username: req.Principal.Username, Repo: req.Repo, RepoUUID: req.RepoUUID,
		Revision: req.Revision, CreatePR: req.CreatePR, Scope: ScopeWrite, Expires: exp,
	})
	if err != nil {
		return Token{}, err
	}
	return Token{AccessToken: tok, Exp: exp.Unix(), CASURL: s.casURL}, nil
}

// VerifyToken implements Ingestor.
func (s *Service) VerifyToken(_ context.Context, token string) (*Claims, error) {
	return s.signer.verify(token, s.now(), s.skew)
}

func (s *Service) writable(c *Claims) error {
	if s.reject {
		return ErrRejected
	}
	if c == nil || c.Scope != ScopeWrite {
		return ErrScope
	}
	return nil
}

// XorbKey returns the Blob store key of a xorb.
func XorbKey(hash Hash) string { return blob.PrefixXorbs + hash.String() }

func (s *Service) xorbCounter(ctx context.Context, outcome string) {
	if s.inst != nil && s.inst.XetXorbs != nil {
		s.inst.XetXorbs.Add(ctx, 1, metric.WithAttributes(attribute.String(telemetry.MetricOutcome, outcome)))
	}
}

func endSpan(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

// PutXorb implements Ingestor. It verifies the whole xorb before storing
// it, so bytes that do not hash to the path's hash are never kept
// (docs/research/xet-upload-research.md §3.6).
func (s *Service) PutXorb(ctx context.Context, c *Claims, hash string, r io.Reader, size int64) (inserted bool, err error) {
	ctx, span := s.tracer.Start(ctx, "xet.PutXorb", trace.WithAttributes(attribute.String("xet.xorb.hash", hash)))
	defer func() { endSpan(span, err) }()
	if err := s.writable(c); err != nil {
		return false, err
	}
	h, err := ParseHash(hash)
	if err != nil {
		s.xorbCounter(ctx, "rejected")
		return false, fmt.Errorf("%w: %w", ErrBadXorb, err)
	}
	if size > MaxXorbSize {
		s.xorbCounter(ctx, "rejected")
		return false, fmt.Errorf("%w: the xorb is %d bytes, over %d", ErrTooLarge, size, MaxXorbSize)
	}
	body := &io.LimitedReader{R: r, N: MaxXorbSize + 1}
	tooLarge := func() error {
		s.xorbCounter(ctx, "rejected")
		return fmt.Errorf("%w: the xorb is over %d bytes", ErrTooLarge, MaxXorbSize)
	}

	// A xorb Hangar already has is not stored again, but its retention
	// clock restarts (§2.2). Its body is still checked against the hash,
	// so only a client that holds the xorb's bytes learns that Hangar has
	// it (04-git.md "LFS objects").
	found, err := s.touchXorbs(ctx, []Hash{h})
	if err != nil {
		return false, err
	}
	if found[h] {
		idx, err := ParseXorb(body, io.Discard)
		if body.N <= 0 {
			return false, tooLarge()
		}
		if err != nil {
			if errors.Is(err, ErrBadXorb) {
				s.xorbCounter(ctx, "rejected")
			}
			return false, err
		}
		if got := idx.Hash(); got != h {
			s.xorbCounter(ctx, "rejected")
			return false, fmt.Errorf("%w: the xorb hash does not match the body (the body hashes to %s)", ErrBadXorb, got)
		}
		s.xorbCounter(ctx, "existing")
		span.SetAttributes(attribute.Bool("xet.xorb.inserted", false))
		return false, nil
	}

	f, err := os.CreateTemp(s.tempDir, "hangar-xorb-*")
	if err != nil {
		return false, fmt.Errorf("xet: spool xorb: %w", err)
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}()
	idx, err := ParseXorb(body, f)
	if body.N <= 0 {
		return false, tooLarge()
	}
	if err != nil {
		if errors.Is(err, ErrBadXorb) {
			s.xorbCounter(ctx, "rejected")
		}
		return false, err
	}
	if got := idx.Hash(); got != h {
		s.xorbCounter(ctx, "rejected")
		return false, fmt.Errorf("%w: the xorb hash does not match the body (the body hashes to %s)", ErrBadXorb, got)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	index := idx.MarshalIndex()
	stored := int64(len(index)) + idx.DataSize
	key := XorbKey(h)
	if err := s.blobs.Put(ctx, key, io.MultiReader(bytes.NewReader(index), io.LimitReader(f, idx.DataSize)), stored); err != nil {
		return false, fmt.Errorf("xet: store xorb: %w", err)
	}
	now := s.clock()
	row := &db.XetXorb{Hash: hash, Size: stored, StorageKey: key, LastReferencedAt: now}
	if _, err := s.db.NewInsert().Model(row).
		On("CONFLICT (hash) DO UPDATE").
		Set("size = EXCLUDED.size").
		Set("storage_key = EXCLUDED.storage_key").
		Set("last_referenced_at = EXCLUDED.last_referenced_at").
		Set("updated_at = EXCLUDED.updated_at").
		Exec(ctx); err != nil {
		return false, fmt.Errorf("xet: record xorb: %w", err)
	}
	s.xorbCounter(ctx, "inserted")
	span.SetAttributes(attribute.Bool("xet.xorb.inserted", true), attribute.Int("xet.xorb.chunks", len(idx.Chunks)))
	return true, nil
}

// touchBatch bounds the number of hashes in one IN list.
const touchBatch = 500

// touchXorbs restarts the retention clock of the given xorbs and reports
// which exist. The update takes each row's lock, so it waits for a
// concurrent GC deletion and then finds the row gone: a xorb reported as
// found here is not deleted for another xorb_retention.
func (s *Service) touchXorbs(ctx context.Context, hashes []Hash) (map[Hash]bool, error) {
	found := make(map[Hash]bool, len(hashes))
	now := s.clock()
	for i := 0; i < len(hashes); i += touchBatch {
		batch := hashes[i:min(i+touchBatch, len(hashes))]
		keys := make([]string, len(batch))
		for j, h := range batch {
			keys[j] = h.String()
		}
		var got []string
		if _, err := s.db.NewUpdate().Model((*db.XetXorb)(nil)).
			Set("last_referenced_at = ?", now).
			Set("updated_at = ?", now).
			Where("hash IN (?)", bun.List(keys)).
			Returning("hash").
			Exec(ctx, &got); err != nil {
			return nil, fmt.Errorf("xet: touch xorbs: %w", err)
		}
		for _, k := range got {
			h, err := ParseHash(k)
			if err == nil {
				found[h] = true
			}
		}
	}
	return found, nil
}

// loadIndex reads a stored xorb's index and checks it describes the xorb.
// It reads the header first and passes the chunk count to charge before it
// reads or decodes any entry, so a shard over its index limits never loads
// the index.
func (s *Service) loadIndex(ctx context.Context, h Hash, charge func(chunks int) error) (*XorbIndex, error) {
	rc, _, err := s.blobs.Get(ctx, XorbKey(h), &blob.Range{Start: 0, End: indexReadSize - 1})
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	var hdr [indexHeaderSize]byte
	got, err := io.ReadFull(rc, hdr[:])
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("xet: read xorb %s: %w", h, err)
	}
	n, err := indexChunkCount(hdr[:got])
	if err != nil {
		return nil, fmt.Errorf("xet: xorb %s: %w", h, err)
	}
	if err := charge(n); err != nil {
		return nil, err
	}
	b := make([]byte, indexHeaderSize+n*indexEntrySize)
	copy(b, hdr[:])
	got, err = io.ReadFull(rc, b[indexHeaderSize:])
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("xet: read xorb %s: %w", h, err)
	}
	idx, err := UnmarshalIndex(b[:indexHeaderSize+got])
	if err != nil {
		return nil, fmt.Errorf("xet: xorb %s: %w", h, err)
	}
	if idx.Hash() != h {
		return nil, fmt.Errorf("xet: the stored xorb %s is corrupt: its index does not match its hash", h)
	}
	return idx, nil
}

// unknownXorb reports a xorb Hangar does not have, with the advice users need.
func unknownXorb(h Hash) error {
	return fmt.Errorf("%w: %s", ErrUnknownXorb, h)
}

// AcceptShard implements Ingestor. It validates the shard against the
// stored xorbs (docs/research/xet-upload-research.md §4.5), then rebuilds
// every file, checks its SHA-256 and Xet hash, and stores it as an LFS
// object (§5.1). Files are processed one at a time, so memory stays flat
// whatever their size.
func (s *Service) AcceptShard(ctx context.Context, c *Claims, r io.Reader, size int64, events func(ShardEvent)) (res *ShardResult, err error) {
	ctx, span := s.tracer.Start(ctx, "xet.AcceptShard")
	defer func() { endSpan(span, err) }()
	emit := func(e ShardEvent) {
		if events != nil {
			events(e)
		}
	}
	if err := s.writable(c); err != nil {
		return nil, err
	}
	span.SetAttributes(attribute.String("hangar.repo.id", c.Repo.Path()))
	if size > MaxShardSize {
		return nil, fmt.Errorf("%w: the shard is %d bytes, over %d", ErrTooLarge, size, MaxShardSize)
	}
	body, err := io.ReadAll(io.LimitReader(r, MaxShardSize+1))
	if err != nil {
		return nil, err
	}
	sh, err := ParseShard(body)
	if err != nil {
		return nil, err
	}
	if err := sh.CheckUpload(); err != nil {
		return nil, err
	}
	// The limits need only the shard, so a shard describing too much work
	// is refused before any xorb is touched.
	if err := sh.CheckLimits(s.limits); err != nil {
		return nil, err
	}
	total := len(sh.Files) + len(sh.Xorbs)
	span.SetAttributes(attribute.Int("xet.shard.files", len(sh.Files)), attribute.Int("xet.shard.xorbs", len(sh.Xorbs)))
	emit(ShardEvent{Type: EventValidating, Verified: 0, Total: total})

	// Every xorb the shard names must exist; naming it restarts its
	// retention clock (03-hf-api.md "Keeping xorbs"). Lookups are global,
	// not per Repo, because clients reuse xorbs across Repos.
	hashes := sh.XorbHashes()
	found, err := s.touchXorbs(ctx, hashes)
	if err != nil {
		return nil, err
	}
	for _, h := range hashes {
		if !found[h] {
			return nil, unknownXorb(h)
		}
	}
	// Indexes are loaded as each check needs them, within the shard's
	// index limits, rather than all held until the shard is done. What a
	// shard may load grows with the xorbs and chunks it names, not with
	// how often it names them.
	cache := newIndexCache(s.limits, sh.indexReloadAllowance(s.limits), func(h Hash, charge func(int) error) (*XorbIndex, error) {
		idx, err := s.loadIndex(ctx, h, charge)
		if errors.Is(err, blob.ErrNotFound) {
			return nil, unknownXorb(h)
		}
		return idx, err
	})
	defer func() {
		span.SetAttributes(attribute.Int64("xet.shard.index_chunks_loaded", cache.loaded),
			attribute.Int64("xet.shard.index_chunks_peak", cache.peak),
			attribute.Int64("xet.shard.index_chunks_allowed", cache.allowed))
	}()
	index := IndexFunc(cache.get)
	// Every term must fit its stored xorb before any file is checked.
	if err := CheckRanges(sh, index); err != nil {
		return nil, err
	}

	verified := 0
	for i := range sh.Xorbs {
		idx, err := index(sh.Xorbs[i].Hash)
		if err != nil {
			return nil, err
		}
		if err := CheckXorb(&sh.Xorbs[i], idx); err != nil {
			return nil, err
		}
		verified++
		emit(ShardEvent{Type: EventValidating, Verified: verified, Total: total})
	}
	for i := range sh.Files {
		if err := CheckFile(&sh.Files[i], index); err != nil {
			return nil, err
		}
		verified++
		emit(ShardEvent{Type: EventValidating, Verified: verified, Total: total})
	}

	emit(ShardEvent{Type: EventCommitting, Stage: StageUploading})
	res = &ShardResult{Result: 1}
	budget := s.limits.MaxShardBytes
	for i := range sh.Files {
		f, err := s.reassemble(ctx, c, &sh.Files[i], index, &budget)
		if err != nil {
			return nil, err
		}
		res.Files = append(res.Files, f)
	}
	emit(ShardEvent{Type: EventCommitting, Stage: StageSyncing})
	emit(ShardEvent{Type: EventResult, Result: 1})
	s.log.InfoContext(ctx, "accepted Xet shard", "repo", c.Repo.Path(), "files", len(res.Files), "xorbs", len(hashes))
	return res, nil
}

// reassemble rebuilds one file as an LFS object and records its Xet hash,
// and the token's User as an uploader of the object, so their Commits may
// reference it (04-git.md "LFS objects"). Every rebuild is paid for from
// budget, the bytes the shard may still rebuild, before it starts.
func (s *Service) reassemble(ctx context.Context, c *Claims, f *ShardFile, index IndexFunc, budget *int64) (out ReassembledFile, err error) {
	start := time.Now()
	oid := f.SHA256.String()
	size := f.Size()
	out = ReassembledFile{XetHash: f.Hash.String(), OID: oid, Size: size}
	outcome := "ok"
	defer func() {
		if err != nil {
			outcome = "error"
			if errors.Is(err, ErrBadShard) || errors.Is(err, ErrTooLarge) {
				outcome = "rejected"
			}
		}
		if s.inst != nil && s.inst.XetReassemblyDuration != nil {
			s.inst.XetReassemblyDuration.Record(ctx, time.Since(start).Seconds(),
				metric.WithAttributes(attribute.String(telemetry.MetricOutcome, outcome)))
		}
	}()

	obj, statErr := s.lfs.Stat(ctx, oid)
	switch {
	case statErr == nil && obj.Verified:
		if obj.Size != size {
			// The answer does not give the stored size: the caller may not
			// be able to read the object.
			return out, fmt.Errorf("%w: file %s does not have SHA-256 %s", ErrBadShard, f.Hash, oid)
		}
		// A file reassembled and checked before needs no second pass.
		var row db.XetFile
		err := s.db.NewSelect().Model(&row).Where("xet_hash = ?", out.XetHash).Scan(ctx)
		if err == nil && row.OID == oid {
			return out, s.recordFile(ctx, c, out)
		}
		if err != nil && !db.IsNotFound(err) {
			return out, fmt.Errorf("xet: look up file: %w", err)
		}
		// The object exists, but nothing proves this file's chunks are
		// its bytes: rebuild them and compare without storing.
		if err := spend(budget, f, size, s.limits.MaxShardBytes); err != nil {
			return out, err
		}
		fr := s.newFileReader(ctx, f, index)
		defer fr.Close()
		if err := fr.verify(oid, size); err != nil {
			return out, err
		}
	case statErr != nil && !errors.Is(statErr, repo.ErrNotFound) && !errors.Is(statErr, blob.ErrNotFound):
		return out, fmt.Errorf("xet: look up LFS object: %w", statErr)
	default:
		if err := spend(budget, f, size, s.limits.MaxShardBytes); err != nil {
			return out, err
		}
		fr := s.newFileReader(ctx, f, index)
		defer fr.Close()
		_, ierr := s.lfs.Ingest(ctx, oid, size, fr)
		if fr.err != nil && !errors.Is(fr.err, io.EOF) {
			return out, fr.err
		}
		if ierr != nil {
			if errors.Is(ierr, repo.ErrChecksumMismatch) {
				return out, fmt.Errorf("%w: file %s does not have SHA-256 %s", ErrBadShard, f.Hash, oid)
			}
			return out, fmt.Errorf("xet: store reassembled file: %w", ierr)
		}
		// The store may skip reading when the object appeared meanwhile;
		// verify reads whatever is left and checks the hash either way.
		if err := fr.verify(oid, size); err != nil {
			return out, err
		}
		if s.inst != nil && s.inst.XetReassemblyBytes != nil {
			s.inst.XetReassemblyBytes.Add(ctx, size)
		}
	}
	return out, s.recordFile(ctx, c, out)
}

// recordFile records a file's Xet hash and the token's User as an uploader
// of its LFS object.
func (s *Service) recordFile(ctx context.Context, c *Claims, f ReassembledFile) error {
	if err := s.lfs.RecordUploader(ctx, f.OID, c.UserID); err != nil {
		return fmt.Errorf("xet: record uploader: %w", err)
	}
	row := &db.XetFile{XetHash: f.XetHash, OID: f.OID, Size: f.Size}
	if _, err := s.db.NewInsert().Model(row).
		On("CONFLICT (xet_hash) DO UPDATE").
		Set("oid = EXCLUDED.oid").
		Set("size = EXCLUDED.size").
		Set("updated_at = EXCLUDED.updated_at").
		Exec(ctx); err != nil {
		return fmt.Errorf("xet: record file: %w", err)
	}
	return nil
}
