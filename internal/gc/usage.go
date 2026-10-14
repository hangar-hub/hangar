package gc

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/jamesagarside/hangar/internal/db"
)

// ErrQuotaExceeded is wrapped by QuotaError.
var ErrQuotaExceeded = errors.New("storage quota exceeded")

// ErrNamespaceNotFound means no Namespace has the name.
var ErrNamespaceNotFound = errors.New("namespace not found")

// QuotaError says a change would take a Namespace over its quota. Its
// message is written for the user who made the change.
type QuotaError struct {
	Namespace string
	Used      int64
	Adding    int64
	Limit     int64
}

func (e *QuotaError) Error() string {
	return fmt.Sprintf("This change needs %s more, but %s has used %s of its %s storage quota. Delete files or ask an admin to raise the quota.",
		FormatBytes(e.Adding), e.Namespace, FormatBytes(e.Used), FormatBytes(e.Limit))
}

// Unwrap makes errors.Is(err, ErrQuotaExceeded) true.
func (e *QuotaError) Unwrap() error { return ErrQuotaExceeded }

// FormatBytes formats a byte count in binary units, such as "1.5 GiB".
func FormatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 5; m /= unit {
		div *= unit
		exp++
	}
	v := fmt.Sprintf("%.1f", float64(n)/float64(div))
	v = strings.TrimSuffix(v, ".0")
	return v + " " + string("KMGTPE"[exp]) + "iB"
}

// Accounting reports storage usage and manages quotas (OPS-4).
//
// A Namespace's usage is the size of every distinct LFS object its Repos
// hold (present ones, counted once per Namespace even if several of its
// Repos share an object) plus the size of its Repos' live and pending git
// packs. Deleted Repos count until they are purged.
type Accounting struct {
	db *db.DB
}

// NewAccounting returns an Accounting over d.
func NewAccounting(d *db.DB) *Accounting { return &Accounting{db: d} }

// NamespaceUsage is one Namespace's usage.
type NamespaceUsage struct {
	NamespaceID uuid.UUID
	Namespace   string
	LFSBytes    int64
	GitBytes    int64
	// RepoCount counts Repos that are not deleted.
	RepoCount int
	// QuotaBytes is the quota, or zero for none.
	QuotaBytes int64
}

// Total returns the bytes that count against the quota.
func (u NamespaceUsage) Total() int64 { return u.LFSBytes + u.GitBytes }

// Usage is the Instance's storage usage.
type Usage struct {
	// TotalLFSBytes is the size of every stored LFS object, each counted once.
	TotalLFSBytes int64
	// TotalGitBytes is the size of every stored pack, in any state.
	TotalGitBytes int64
	// XetCacheBytes is the size of the xorbs kept for Xet deduplication.
	XetCacheBytes int64
	// Namespaces holds every Namespace with Repos or a quota, largest first.
	Namespaces []NamespaceUsage
}

// lfsByNamespace sums distinct present LFS objects per Namespace.
const lfsByNamespace = `SELECT t.namespace_id AS namespace_id, COALESCE(SUM(o.size), 0) AS bytes
	FROM (SELECT DISTINCT r.namespace_id, rl.oid FROM repo_lfs rl JOIN repos r ON r.id = rl.repo_id WHERE rl.present AND %s) t
	JOIN lfs_objects o ON o.oid = t.oid GROUP BY t.namespace_id`

// gitByNamespace sums live and pending pack sizes per Namespace.
const gitByNamespace = `SELECT r.namespace_id AS namespace_id, COALESCE(SUM(p.size), 0) AS bytes
	FROM packs p JOIN repos r ON r.id = p.repo_id WHERE p.state IN ('live', 'pending') AND %s GROUP BY r.namespace_id`

type nsBytes struct {
	NamespaceID uuid.UUID `bun:"namespace_id,type:uuid"`
	Bytes       int64     `bun:"bytes"`
}

// Usage reports usage for the whole Instance.
func (a *Accounting) Usage(ctx context.Context) (Usage, error) {
	var u Usage
	if err := a.db.NewSelect().Model((*db.LFSObject)(nil)).ColumnExpr("COALESCE(SUM(size), 0)").Scan(ctx, &u.TotalLFSBytes); err != nil {
		return u, err
	}
	if err := a.db.NewSelect().Model((*db.Pack)(nil)).ColumnExpr("COALESCE(SUM(size), 0)").Scan(ctx, &u.TotalGitBytes); err != nil {
		return u, err
	}
	if err := a.db.NewSelect().Model((*db.XetXorb)(nil)).ColumnExpr("COALESCE(SUM(size), 0)").Scan(ctx, &u.XetCacheBytes); err != nil {
		return u, err
	}
	byNS, err := a.usage(ctx, nil)
	if err != nil {
		return u, err
	}
	for _, n := range byNS {
		u.Namespaces = append(u.Namespaces, *n)
	}
	sort.Slice(u.Namespaces, func(i, j int) bool {
		if u.Namespaces[i].Total() != u.Namespaces[j].Total() {
			return u.Namespaces[i].Total() > u.Namespaces[j].Total()
		}
		return u.Namespaces[i].Namespace < u.Namespaces[j].Namespace
	})
	return u, nil
}

// NamespaceUsage reports one Namespace's usage.
func (a *Accounting) NamespaceUsage(ctx context.Context, nsID uuid.UUID) (NamespaceUsage, error) {
	byNS, err := a.usage(ctx, &nsID)
	if err != nil {
		return NamespaceUsage{}, err
	}
	if n, ok := byNS[nsID]; ok {
		return *n, nil
	}
	name, err := a.namespaceName(ctx, nsID)
	if err != nil {
		return NamespaceUsage{}, err
	}
	return NamespaceUsage{NamespaceID: nsID, Namespace: name}, nil
}

func (a *Accounting) usage(ctx context.Context, only *uuid.UUID) (map[uuid.UUID]*NamespaceUsage, error) {
	out := map[uuid.UUID]*NamespaceUsage{}
	get := func(id uuid.UUID) *NamespaceUsage {
		n, ok := out[id]
		if !ok {
			n = &NamespaceUsage{NamespaceID: id}
			out[id] = n
		}
		return n
	}
	filter, args := "TRUE", []any{}
	if a.db.IsSQLite() {
		filter = "1 = 1"
	}
	if only != nil {
		filter, args = "r.namespace_id = ?", []any{*only}
	}
	var lfs, git []nsBytes
	if err := a.db.NewRaw(fmt.Sprintf(lfsByNamespace, filter), args...).Scan(ctx, &lfs); err != nil {
		return nil, err
	}
	if err := a.db.NewRaw(fmt.Sprintf(gitByNamespace, filter), args...).Scan(ctx, &git); err != nil {
		return nil, err
	}
	for _, b := range lfs {
		get(b.NamespaceID).LFSBytes = b.Bytes
	}
	for _, b := range git {
		get(b.NamespaceID).GitBytes = b.Bytes
	}
	var counts []struct {
		NamespaceID uuid.UUID `bun:"namespace_id,type:uuid"`
		N           int       `bun:"n"`
	}
	q := a.db.NewSelect().Model((*db.Repo)(nil)).ColumnExpr("namespace_id, COUNT(*) AS n").Where("deleted_at IS NULL").GroupExpr("namespace_id")
	if only != nil {
		q.Where("namespace_id = ?", *only)
	}
	if err := q.Scan(ctx, &counts); err != nil {
		return nil, err
	}
	for _, c := range counts {
		get(c.NamespaceID).RepoCount = c.N
	}
	var quotas []db.Quota
	qq := a.db.NewSelect().Model(&quotas)
	if only != nil {
		qq.Where("namespace_id = ?", *only)
	}
	if err := qq.Scan(ctx); err != nil {
		return nil, err
	}
	for _, q := range quotas {
		get(q.NamespaceID).QuotaBytes = q.BytesLimit
	}
	if len(out) == 0 {
		return out, nil
	}
	ids := make([]uuid.UUID, 0, len(out))
	for id := range out {
		ids = append(ids, id)
	}
	var names []db.Namespace
	if err := a.db.NewSelect().Model(&names).Column("id", "name").Where("id IN (?)", bun.List(ids)).Scan(ctx); err != nil {
		return nil, err
	}
	for _, n := range names {
		out[n.ID].Namespace = n.Name
	}
	return out, nil
}

func (a *Accounting) namespaceName(ctx context.Context, id uuid.UUID) (string, error) {
	var ns db.Namespace
	if err := a.db.NewSelect().Model(&ns).Column("name").Where("id = ?", id).Scan(ctx); err != nil {
		if db.IsNotFound(err) {
			return "", fmt.Errorf("%w: %s", ErrNamespaceNotFound, id)
		}
		return "", err
	}
	return ns.Name, nil
}

// NamespaceByName returns a Namespace's ID and canonical name. Names are
// case-insensitive.
func (a *Accounting) NamespaceByName(ctx context.Context, name string) (uuid.UUID, string, error) {
	var ns db.Namespace
	if err := a.db.NewSelect().Model(&ns).Column("id", "name").Where("lower(name) = lower(?)", name).Limit(1).Scan(ctx); err != nil {
		if db.IsNotFound(err) {
			return uuid.Nil, "", fmt.Errorf("%w: %s", ErrNamespaceNotFound, name)
		}
		return uuid.Nil, "", err
	}
	return ns.ID, ns.Name, nil
}

// SetQuota sets a Namespace's quota in bytes. Zero removes it.
func (a *Accounting) SetQuota(ctx context.Context, nsID uuid.UUID, bytes int64) error {
	if bytes < 0 {
		return errors.New("a quota cannot be negative")
	}
	if bytes == 0 {
		_, err := a.db.NewDelete().Model((*db.Quota)(nil)).Where("namespace_id = ?", nsID).Exec(ctx)
		return err
	}
	_, err := a.db.NewInsert().Model(&db.Quota{NamespaceID: nsID, BytesLimit: bytes}).
		On("CONFLICT (namespace_id) DO UPDATE").Set("bytes_limit = EXCLUDED.bytes_limit").Set("updated_at = EXCLUDED.updated_at").
		Exec(ctx)
	if db.IsForeignKeyViolation(err) {
		return fmt.Errorf("%w: %s", ErrNamespaceNotFound, nsID)
	}
	return err
}

// Quota returns a Namespace's quota in bytes, or zero for none.
func (a *Accounting) Quota(ctx context.Context, nsID uuid.UUID) (int64, error) {
	var q db.Quota
	err := a.db.NewSelect().Model(&q).Where("namespace_id = ?", nsID).Scan(ctx)
	if db.IsNotFound(err) {
		return 0, nil
	}
	return q.BytesLimit, err
}

// CheckQuota returns a QuotaError if adding bytes would take the Namespace
// over its quota. Pushes, Commits, uploads and Pulls call it before they
// store anything; adding is what the change stores that the Namespace
// does not hold yet (see NewLFSBytes).
func (a *Accounting) CheckQuota(ctx context.Context, nsID uuid.UUID, adding int64) error {
	limit, err := a.Quota(ctx, nsID)
	if err != nil || limit == 0 || adding <= 0 {
		return err
	}
	u, err := a.NamespaceUsage(ctx, nsID)
	if err != nil {
		return err
	}
	if u.Total()+adding > limit {
		return &QuotaError{Namespace: u.Namespace, Used: u.Total(), Adding: adding, Limit: limit}
	}
	return nil
}

// NewLFSBytes returns the total size of the objects (OID to size) that no
// Repo of the Namespace holds yet, which is what a change adds to its usage.
func (a *Accounting) NewLFSBytes(ctx context.Context, nsID uuid.UUID, objects map[string]int64) (int64, error) {
	if len(objects) == 0 {
		return 0, nil
	}
	oids := make([]string, 0, len(objects))
	for oid := range objects {
		oids = append(oids, oid)
	}
	held := map[string]bool{}
	for start := 0; start < len(oids); start += batchSize {
		batch := oids[start:min(start+batchSize, len(oids))]
		var found []string
		if err := a.db.NewSelect().TableExpr("repo_lfs AS rl").ColumnExpr("DISTINCT rl.oid").
			Join("JOIN repos AS r ON r.id = rl.repo_id").
			Where("r.namespace_id = ? AND rl.present AND rl.oid IN (?)", nsID, bun.List(batch)).Scan(ctx, &found); err != nil {
			return 0, err
		}
		for _, o := range found {
			held[o] = true
		}
	}
	var n int64
	for oid, size := range objects {
		if !held[oid] {
			n += size
		}
	}
	return n, nil
}
