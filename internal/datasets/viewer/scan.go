package viewer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/parquet-go/parquet-go"

	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/datasets"
	"github.com/jamesagarside/hangar/internal/datasets/features"
	"github.com/jamesagarside/hangar/internal/datasets/pipeline"
	"github.com/jamesagarside/hangar/internal/datasets/query"
	"github.com/jamesagarside/hangar/internal/model"
)

// result is a scan's matching rows, in response order.
type result struct {
	idx     []int64
	partial bool
}

// maxCachedResult caps a cached result, so a filter that keeps every row
// of a huge Split does not write a huge object.
const maxCachedResult = 50_000_000

func resultKey(st *state, parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return fmt.Sprintf("datasets/%s/%s/results/%s", st.repo.UUID, st.commit, hex.EncodeToString(h.Sum(nil)))
}

func encodeResult(r *result) []byte {
	var b bytes.Buffer
	b.WriteString("HIDX1")
	if r.partial {
		b.WriteByte(1)
	} else {
		b.WriteByte(0)
	}
	buf := make([]byte, binary.MaxVarintLen64)
	n := binary.PutUvarint(buf, uint64(len(r.idx)))
	b.Write(buf[:n])
	var prev int64
	for _, i := range r.idx {
		n := binary.PutVarint(buf, i-prev)
		b.Write(buf[:n])
		prev = i
	}
	return b.Bytes()
}

func decodeResult(b []byte) (*result, error) {
	if len(b) < 6 || string(b[:5]) != "HIDX1" {
		return nil, errors.New("not a cached result")
	}
	r := &result{partial: b[5] == 1}
	rd := bytes.NewReader(b[6:])
	n, err := binary.ReadUvarint(rd)
	if err != nil || n > maxCachedResult {
		return nil, errors.New("corrupt cached result")
	}
	r.idx = make([]int64, 0, n)
	var prev int64
	for range n {
		d, err := binary.ReadVarint(rd)
		if err != nil {
			return nil, errors.New("corrupt cached result")
		}
		prev += d
		r.idx = append(r.idx, prev)
	}
	return r, nil
}

func (s *Service) cached(ctx context.Context, key string) *result {
	rc, _, err := s.d.Blobs.Get(ctx, key, nil)
	if err != nil {
		return nil
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, 10*maxCachedResult))
	if err != nil {
		return nil
	}
	r, err := decodeResult(b)
	if err != nil {
		return nil
	}
	return r
}

func (s *Service) store(ctx context.Context, key string, r *result) {
	if len(r.idx) > maxCachedResult {
		return
	}
	b := encodeResult(r)
	if err := s.d.Blobs.Put(ctx, key, bytes.NewReader(b), int64(len(b))); err != nil {
		s.d.Log.WarnContext(ctx, "could not cache a viewer result", "error", err.Error())
	}
}

// pageOf reads the rows of a page of a result.
func (s *Service) pageOf(ctx context.Context, st *state, info *pipeline.ConfigInfo, config, split string, t *table, r *result, offset int64, length int) (*datasets.RowsResponse, error) {
	var idx []int64
	if offset < int64(len(r.idx)) {
		idx = r.idx[offset:min(int64(len(r.idx)), offset+int64(length))]
	}
	rows := make([][]any, 0, len(idx))
	// Read runs of consecutive rows together.
	for i := 0; i < len(idx); {
		j := i + 1
		for j < len(idx) && idx[j] == idx[j-1]+1 {
			j++
		}
		got, err := t.read(ctx, nil, idx[i], int64(j-i))
		if err != nil {
			return nil, err
		}
		rows = append(rows, got...)
		i = j
	}
	if len(rows) != len(idx) {
		return nil, fmt.Errorf("read %d rows of %d", len(rows), len(idx))
	}
	return s.page(st, info, config, split, t, rows, idx, int64(len(r.idx)), r.partial || info.Partial), nil
}

// scanner walks a table's rows for a set of columns, skipping row groups
// that cannot match, within the scan limits.
type scanner struct {
	s        *Service
	t        *table
	columns  []string
	deadline time.Time
	scanned  int64
	partial  bool
	// skip reports, from per-column min and max, whether a row group cannot match.
	skip func(stats map[string][2]any) bool
}

func statValue(v parquet.Value, a *features.Arrow) any {
	if v.IsNull() {
		return nil
	}
	switch {
	case a.IsInteger():
		if a.ID == features.Uint64 {
			return uint64(v.Int64()) //nolint:gosec // unsigned values are stored as int64 bits
		}
		if v.Kind() == parquet.Int32 {
			if a.ID == features.Uint32 {
				return int64(uint32(v.Int32())) //nolint:gosec // unsigned values are stored as int32 bits
			}
			return int64(v.Int32())
		}
		return v.Int64()
	case a.ID == features.Float32:
		return float64(v.Float())
	case a.ID == features.Float64:
		return v.Double()
	case a.IsString():
		return string(v.ByteArray())
	}
	return nil
}

// each calls fn for every scanned row with the values of the scanner's
// columns. It stops at the limits and marks the scan partial.
func (sc *scanner) each(ctx context.Context, fn func(rowIdx int64, vals []any) error) error {
	var base int64
	for i, sh := range sc.t.split.Shards {
		f, err := sc.t.file(i)
		if err != nil {
			return err
		}
		cols, err := f.Columns(sc.columns)
		if err != nil {
			return err
		}
		var start int64
		for g := range f.NumRowGroups() {
			n := f.RowGroupRows(g)
			if sc.skip != nil {
				stats := map[string][2]any{}
				for k, name := range sc.columns {
					leaf, count := f.LeafRange(cols[k])
					ft := sc.t.storage.Fields[sc.t.features.Index(name)].Type
					if count != 1 {
						continue
					}
					if lo, hi, ok := f.ColumnStats(g, leaf); ok {
						stats[name] = [2]any{statValue(lo, ft), statValue(hi, ft)}
					}
				}
				if sc.skip(stats) {
					start += n
					sc.scanned += n
					continue
				}
			}
			it, err := f.Scan(cols, start, n)
			if err != nil {
				return err
			}
			for {
				if sc.scanned >= sc.s.o.MaxScanRows || (sc.scanned%4096 == 0 && time.Now().After(sc.deadline)) {
					it.Close()
					sc.partial = true
					return nil
				}
				local, row, err := it.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					it.Close()
					return err
				}
				sc.scanned++
				if err := fn(base+local, row); err != nil {
					it.Close()
					return err
				}
			}
			it.Close()
			start += n
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		base += sh.NumRows
	}
	return nil
}

// Search implements datasets.Viewer.
func (s *Service) Search(ctx context.Context, p model.Principal, req datasets.SearchRequest) (*datasets.RowsResponse, error) {
	resp, _, err := s.SearchFor(ctx, p, req)
	return resp, err
}

// SearchFor answers /search: rows where every query term occurs, without
// regard to case, in some text column. The Hub ranks matches with full
// text search; Hangar returns them in row order, a superset of the Hub's
// matches (compat/viewer/DIFFERENCES.md).
func (s *Service) SearchFor(ctx context.Context, p model.Principal, req datasets.SearchRequest) (*datasets.RowsResponse, *state, error) {
	if err := checkPage(req.Offset, req.Length); err != nil {
		return nil, nil, err
	}
	st, err := s.head(ctx, p, req.Repo, auth.ActionReadFiles)
	if err != nil {
		return nil, nil, err
	}
	_, info, err := s.readySplit(ctx, st, req.Config, req.Split)
	if err != nil {
		return nil, st, err
	}
	t, err := s.table(st, info, req.Split)
	if err != nil {
		return nil, st, err
	}
	cols := searchColumns(t.features)
	if len(cols) == 0 {
		return nil, st, fmt.Errorf("%w: the split has no text columns to search", datasets.ErrInvalidQuery)
	}
	terms := strings.Fields(strings.ToLower(req.Query))
	key := resultKey(st, "search", req.Config, req.Split, strings.Join(terms, " "))
	r := s.cached(ctx, key)
	if r == nil {
		var names []string
		for _, c := range cols {
			names = append(names, t.features[c].Name)
		}
		sc := &scanner{s: s, t: t, columns: names, deadline: time.Now().Add(s.o.MaxScanTime)}
		r = &result{}
		err := sc.each(ctx, func(rowIdx int64, vals []any) error {
			for _, term := range terms {
				found := false
				for _, v := range vals {
					if containsText(v, term, 0) {
						found = true
						break
					}
				}
				if !found {
					return nil
				}
			}
			r.idx = append(r.idx, rowIdx)
			return nil
		})
		if err != nil {
			return nil, st, err
		}
		r.partial = sc.partial
		s.store(ctx, key, r)
	}
	resp, err := s.pageOf(ctx, st, info, req.Config, req.Split, t, r, req.Offset, req.Length)
	return resp, st, err
}

// containsText reports whether any string inside v contains term (lower case).
func containsText(v any, term string, depth int) bool {
	if depth > 64 {
		return false
	}
	switch x := v.(type) {
	case string:
		return strings.Contains(strings.ToLower(x), term)
	case []any:
		for _, e := range x {
			if containsText(e, term, depth+1) {
				return true
			}
		}
	}
	return false
}

// Filter implements datasets.Viewer.
func (s *Service) Filter(ctx context.Context, p model.Principal, req datasets.FilterRequest) (*datasets.RowsResponse, error) {
	resp, _, err := s.FilterFor(ctx, p, req)
	return resp, err
}

// FilterFor answers /filter.
func (s *Service) FilterFor(ctx context.Context, p model.Principal, req datasets.FilterRequest) (*datasets.RowsResponse, *state, error) {
	if err := checkPage(req.Offset, req.Length); err != nil {
		return nil, nil, err
	}
	where, err := query.ParseWhere(req.Where)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: Parameter 'where' contains errors or invalid symbols: %w", datasets.ErrInvalidQuery, err)
	}
	order, err := query.ParseOrderBy(req.OrderBy)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: Parameter 'orderby' contains errors or invalid symbols: %w", datasets.ErrInvalidQuery, err)
	}
	st, err := s.head(ctx, p, req.Repo, auth.ActionReadFiles)
	if err != nil {
		return nil, nil, err
	}
	_, info, err := s.readySplit(ctx, st, req.Config, req.Split)
	if err != nil {
		return nil, st, err
	}
	t, err := s.table(st, info, req.Split)
	if err != nil {
		return nil, st, err
	}
	cols := make([]query.Column, len(t.features))
	for i, f := range t.features {
		cols[i] = query.Column{Name: f.Name, Type: t.storage.Fields[i].Type}
	}
	plan, err := query.Compile(where, order, cols)
	if err != nil {
		return nil, st, fmt.Errorf("%w: %w", datasets.ErrInvalidQuery, err)
	}
	canonWhere, canonOrder := "", ""
	if where != nil {
		canonWhere = where.String()
	}
	for _, o := range order {
		canonOrder += fmt.Sprintf("%v %v;", o.Column, o.Desc)
	}
	key := resultKey(st, "filter", req.Config, req.Split, canonWhere, canonOrder)
	r := s.cached(ctx, key)
	if r == nil {
		r = &result{}
		if len(plan.Columns) == 0 {
			// No where clause and no order: every row, in order.
			n := t.numRows()
			if n > s.o.MaxScanRows {
				n, r.partial = s.o.MaxScanRows, true
			}
			r.idx = make([]int64, n)
			for i := range r.idx {
				r.idx[i] = int64(i)
			}
		} else {
			sc := &scanner{s: s, t: t, columns: plan.Columns, deadline: time.Now().Add(s.o.MaxScanTime), skip: func(stats map[string][2]any) bool {
				return !plan.MayMatch(stats)
			}}
			var keys [][]any
			err := sc.each(ctx, func(rowIdx int64, vals []any) error {
				if plan.Match(vals) {
					r.idx = append(r.idx, rowIdx)
					if plan.Ordered() {
						keys = append(keys, plan.Key(vals))
					}
				}
				return nil
			})
			if err != nil {
				return nil, st, err
			}
			r.partial = sc.partial
			if plan.Ordered() {
				perm := make([]int, len(r.idx))
				for i := range perm {
					perm[i] = i
				}
				sort.SliceStable(perm, func(a, b int) bool { return plan.Less(keys[perm[a]], keys[perm[b]]) })
				sorted := make([]int64, len(perm))
				for i, k := range perm {
					sorted[i] = r.idx[k]
				}
				r.idx = sorted
			}
		}
		if r.partial || len(plan.Columns) > 0 {
			s.store(ctx, key, r)
		}
	}
	resp, err := s.pageOf(ctx, st, info, req.Config, req.Split, t, r, req.Offset, req.Length)
	return resp, st, err
}

// blobNotFound reports whether err means a missing cache entry.
func blobNotFound(err error) bool { return errors.Is(err, blob.ErrNotFound) }
