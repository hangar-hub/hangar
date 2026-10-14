package convert

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/jamesagarside/hangar/internal/datasets/arrowipc"
	"github.com/jamesagarside/hangar/internal/datasets/features"
	"github.com/jamesagarside/hangar/internal/datasets/pq"
)

type parquetBuilder struct{}

func (parquetBuilder) name() string { return "parquet" }

func openParquet(ctx context.Context, f File) (*pq.File, error) {
	if f.OpenAt == nil {
		return nil, fmt.Errorf("%w: Parquet file without random access", ErrUnsupported)
	}
	ra, err := f.OpenAt(ctx)
	if err != nil {
		return nil, err
	}
	pf, err := pq.Open(ra, f.Size)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", f.Path, err)
	}
	return pf, nil
}

func (parquetBuilder) infer(e *env, splits []splitFiles) (features.Features, error) {
	if len(splits) == 0 || len(splits[0].files) == 0 {
		return nil, errors.New("no Parquet files")
	}
	pf, err := openParquet(e.ctx, splits[0].files[0])
	if err != nil {
		return nil, err
	}
	return pf.Features, nil
}

// castRow casts a row read from a file with schema from to the features.
func castRow(row []any, from []string, fs features.Features, st *features.Arrow) ([]any, error) {
	out := make([]any, len(fs))
	for i, f := range fs {
		j := -1
		for k, n := range from {
			if n == f.Name {
				j = k
				break
			}
		}
		if j < 0 {
			return nil, fmt.Errorf("couldn't cast the data to the features: column %q is missing", f.Name)
		}
		v, err := Cast(row[j], st.Fields[i].Type)
		if err != nil {
			return nil, fmt.Errorf("column %q: %w", f.Name, err)
		}
		out[i] = v
	}
	return out, nil
}

func (parquetBuilder) read(e *env, split splitFiles, fs features.Features, emit emitFunc) error {
	st, err := fs.StorageSchema()
	if err != nil {
		return err
	}
	for _, f := range split.files {
		pf, err := openParquet(e.ctx, f)
		if err != nil {
			return err
		}
		names := features.FromArrowSchema(pf.Schema).Names()
		if len(names) != len(fs) {
			return fmt.Errorf("%s: couldn't cast the columns %v to the features %v", f.Path, names, fs.Names())
		}
		cols, _ := pf.Columns(nil)
		var start int64
		for g := range pf.NumRowGroups() {
			n := pf.RowGroupRows(g)
			rows, err := pf.ReadRowsWithin(e.budget, cols, start, n)
			if err != nil {
				return fmt.Errorf("%s: %w", f.Path, err)
			}
			start += n
			for i, r := range rows {
				if rows[i], err = castRow(r, names, fs, st); err != nil {
					return fmt.Errorf("%s: %w", f.Path, err)
				}
			}
			if len(rows) > 0 {
				if err := emit(rows); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

type arrowBuilder struct{}

func (arrowBuilder) name() string { return "arrow" }

// arrowFeatures returns the Features of an Arrow schema: the `datasets`
// metadata when present, otherwise inferred from the types.
func arrowFeatures(s *arrowipc.Schema) features.Features {
	fs := features.FromArrowSchema(s.Type)
	if meta, ok := s.Metadata["huggingface"]; ok {
		var doc struct {
			Info struct {
				Features json.RawMessage `json:"features"`
			} `json:"info"`
		}
		if json.Unmarshal([]byte(meta), &doc) == nil && len(doc.Info.Features) > 0 {
			if mfs, err := features.ParseFeatures(doc.Info.Features); err == nil && len(mfs) == len(fs) {
				return mfs
			}
		}
	}
	return fs
}

func (arrowBuilder) infer(e *env, splits []splitFiles) (features.Features, error) {
	if len(splits) == 0 || len(splits[0].files) == 0 {
		return nil, errors.New("no Arrow files")
	}
	f := splits[0].files[0]
	rc, err := f.Open(e.ctx)
	if err != nil {
		return nil, err
	}
	r, err := arrowipc.NewReader(rc)
	_ = rc.Close()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", f.Path, err)
	}
	return arrowFeatures(r.Schema), nil
}

func (arrowBuilder) read(e *env, split splitFiles, fs features.Features, emit emitFunc) error {
	st, err := fs.StorageSchema()
	if err != nil {
		return err
	}
	for _, f := range split.files {
		if err := readArrowFile(e.ctx, f, fs, st, emit); err != nil {
			return fmt.Errorf("%s: %w", f.Path, err)
		}
	}
	return nil
}

func readArrowFile(ctx context.Context, f File, fs features.Features, st *features.Arrow, emit emitFunc) error {
	rc, err := f.Open(ctx)
	if err != nil {
		return err
	}
	defer rc.Close()
	r, err := arrowipc.NewReader(rc)
	if err != nil {
		return err
	}
	names := features.FromArrowSchema(r.Schema.Type).Names()
	if len(names) != len(fs) {
		return fmt.Errorf("couldn't cast the columns %v to the features %v", names, fs.Names())
	}
	for {
		rows, err := r.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		for i, row := range rows {
			if rows[i], err = castRow(row, names, fs, st); err != nil {
				return err
			}
		}
		if len(rows) > 0 {
			if err := emit(rows); err != nil {
				return err
			}
		}
	}
}
