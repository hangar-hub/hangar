package convert

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"regexp"
	"strings"

	"github.com/jamesagarside/hangar/internal/datasets"
	"github.com/jamesagarside/hangar/internal/datasets/features"
	"github.com/jamesagarside/hangar/internal/datasets/pq"
)

// Limits from the Hub's converter (services/worker parquet_and_info.py).
const (
	// MaxFilesPerDirectory splits large exports into `{split}-partN` folders.
	MaxFilesPerDirectory = 10_000
	// DefaultShardBytes is datasets.config.MAX_SHARD_SIZE (500 MB of Arrow data).
	DefaultShardBytes = 500_000_000
	// DefaultRowGroupRows is datasets.config.DEFAULT_MAX_BATCH_SIZE.
	DefaultRowGroupRows = 1000
	// MediaRowGroupRows is the row group size for image, audio and binary data.
	MediaRowGroupRows = 100
)

// Options configure a conversion.
type Options struct {
	// MaxSplitBytes stops a Split's conversion once this much Arrow data is
	// written (viewer.max_convert_bytes); the export is then partial.
	MaxSplitBytes int64
	// ShardBytes is the Arrow data per Parquet file. Zero means 500 MB.
	ShardBytes int64
	// TempDir holds the Parquet files until they are committed.
	TempDir string
	// DatasetName is the `datasets` dataset_name: the Repo name in snake case.
	DatasetName string
	// RepoURL is hf://datasets/{ns}/{name}@{commit}.
	RepoURL string
	// Open returns any file of the Repo at the Commit.
	Open func(ctx context.Context, path string) (File, error)
}

// Shard is one Parquet file of the export.
type Shard struct {
	// ExportPath is the path on refs/convert/parquet, such as
	// default/train/0000.parquet.
	ExportPath string
	// LocalPath is the written file, or "" for a copied source file.
	LocalPath string
	// CopyOID is the LFS object of a source Parquet file copied as is.
	CopyOID string
	// SourcePath is the copied source file's path.
	SourcePath string
	Size       int64
	SHA256     string
	NumRows    int64
}

// SplitResult is the outcome of one Split.
type SplitResult struct {
	Name     string
	NumRows  int64
	NumBytes int64
	Shards   []Shard
}

// ConfigResult is the outcome of one Config.
type ConfigResult struct {
	Name     string
	Builder  string
	Features features.Features
	Splits   []SplitResult
	// Copied is true when source Parquet files were copied as is.
	Copied bool
	// Partial is true when a Split was cut at MaxSplitBytes.
	Partial bool
	// SourceFiles are the data files read, with their sizes, for
	// download_checksums and download_size.
	SourceFiles []File
}

// Cleanup removes the result's local files.
func (r *ConfigResult) Cleanup() {
	for _, s := range r.Splits {
		for _, sh := range s.Shards {
			if sh.LocalPath != "" {
				_ = os.Remove(sh.LocalPath)
			}
		}
	}
}

var (
	upperUpperRe = regexp.MustCompile(`([A-Z]+)([A-Z][a-z])`)
	lowerUpperRe = regexp.MustCompile(`([a-z\d])([A-Z])`)
)

// SnakeCase is datasets.naming.camelcase_to_snakecase, which gives the
// dataset_name of a Repo.
func SnakeCase(name string) string {
	name = upperUpperRe.ReplaceAllString(name, "${1}_${2}")
	name = lowerUpperRe.ReplaceAllString(name, "${1}_${2}")
	return strings.ToLower(name)
}

// ExportPath is ParquetFile.path_in_repo.
func ExportPath(config, split string, shard, numShards int, partial bool) string {
	prefix := ""
	if partial {
		prefix = "partial-"
	}
	suffix := ""
	if numShards > MaxFilesPerDirectory {
		suffix = fmt.Sprintf("-part%d", shard/MaxFilesPerDirectory)
	}
	return fmt.Sprintf("%s/%s%s%s/%04d.parquet", config, prefix, split, suffix, shard%MaxFilesPerDirectory)
}

// rowGroupRows is get_writer_batch_size_from_info.
func rowGroupRows(builder string, fs features.Features) int64 {
	if builder == "imagefolder" || builder == "audiofolder" {
		return MediaRowGroupRows
	}
	media := false
	for _, f := range fs {
		if f.Feature.Contains(func(x *features.Feature) bool {
			return x.IsMedia() || (x.Kind == features.KindValue && (x.Dtype == "binary" || x.Dtype == "large_binary"))
		}) {
			media = true
		}
	}
	if media {
		return MediaRowGroupRows
	}
	return DefaultRowGroupRows
}

// Convert converts one Config. files maps the Config's file paths to
// openable files.
func Convert(ctx context.Context, cfg datasets.Config, files map[string]File, opts Options) (*ConfigResult, error) {
	b, err := builderFor(cfg.Builder)
	if err != nil {
		return nil, err
	}
	if opts.ShardBytes <= 0 {
		opts.ShardBytes = DefaultShardBytes
	}
	var splits []splitFiles
	res := &ConfigResult{Name: cfg.Name, Builder: cfg.Builder}
	for _, s := range cfg.Splits() {
		sf := splitFiles{name: s}
		for _, p := range cfg.Files[s] {
			f, ok := files[p]
			if !ok {
				return nil, fmt.Errorf("data file %s is missing", p)
			}
			sf.files = append(sf.files, f)
			res.SourceFiles = append(res.SourceFiles, f)
		}
		splits = append(splits, sf)
	}
	e := &env{ctx: ctx, opts: cfg.BuilderOptions, open: opts.Open, repoURL: opts.RepoURL, splits: splits}
	if e.opts == nil {
		e.opts = map[string]any{}
	}
	fs := cfg.Features
	if fs == nil {
		if fs, err = b.infer(e, splits); err != nil {
			return nil, err
		}
	}
	if _, err := fs.StorageSchema(); err != nil {
		return nil, err
	}
	res.Features = fs

	if cfg.Builder == "parquet" {
		return copyParquet(ctx, res, splits)
	}

	st, err := fs.StorageSchema()
	if err != nil {
		return nil, err
	}
	rows := rowGroupRows(cfg.Builder, fs)
	for _, sf := range splits {
		sr, partial, err := convertSplit(e, b, sf, fs, st, rows, opts)
		if err != nil {
			res.Cleanup()
			return nil, fmt.Errorf("split %s: %w", sf.name, err)
		}
		res.Partial = res.Partial || partial
		res.Splits = append(res.Splits, *sr)
	}
	// `datasets` names every file of a partial config with the partial prefix.
	for i := range res.Splits {
		n := len(res.Splits[i].Shards)
		for k := range res.Splits[i].Shards {
			res.Splits[i].Shards[k].ExportPath = ExportPath(cfg.Name, res.Splits[i].Name, k, n, res.Partial)
		}
	}
	return res, nil
}

// shardWriter writes one Parquet file and hashes it.
type shardWriter struct {
	f    *os.File
	h    hash.Hash
	w    *pq.Writer
	rows int64
	size int64
}

type hashingWriter struct {
	w io.Writer
	h hash.Hash
	n *int64
}

func (hw hashingWriter) Write(p []byte) (int, error) {
	n, err := hw.w.Write(p)
	hw.h.Write(p[:n])
	*hw.n += int64(n)
	return n, err
}

func newShard(dir string, st *features.Arrow, fs features.Features, rows int64) (*shardWriter, error) {
	f, err := os.CreateTemp(dir, "export-*.parquet")
	if err != nil {
		return nil, err
	}
	s := &shardWriter{f: f, h: sha256.New()}
	w, err := pq.NewWriter(hashingWriter{w: f, h: s.h, n: &s.size}, st, pq.WriterOptions{Features: fs, RowGroupRows: rows})
	if err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, err
	}
	s.w = w
	return s, nil
}

func (s *shardWriter) finish() (Shard, error) {
	err := s.w.Close()
	if cerr := s.f.Close(); err == nil {
		err = cerr
	}
	sh := Shard{LocalPath: s.f.Name(), Size: s.size, SHA256: hex.EncodeToString(s.h.Sum(nil)), NumRows: s.rows}
	if err != nil {
		_ = os.Remove(s.f.Name())
	}
	return sh, err
}

func (s *shardWriter) abort() {
	_ = s.f.Close()
	_ = os.Remove(s.f.Name())
}

var errLimit = errors.New("conversion limit reached")

func convertSplit(e *env, b builder, sf splitFiles, fs features.Features, st *features.Arrow, rgRows int64, opts Options) (*SplitResult, bool, error) {
	sr := &SplitResult{Name: sf.name}
	var cur *shardWriter
	var shardBytes int64
	partial := false
	closeShard := func() error {
		if cur == nil {
			return nil
		}
		sh, err := cur.finish()
		cur = nil
		shardBytes = 0
		if err != nil {
			return err
		}
		sr.Shards = append(sr.Shards, sh)
		return nil
	}
	err := b.read(e, sf, fs, func(rows [][]any) error {
		if opts.MaxSplitBytes > 0 && sr.NumBytes >= opts.MaxSplitBytes {
			partial = true
			return errLimit
		}
		if err := e.ctx.Err(); err != nil {
			return err
		}
		if cur == nil {
			w, err := newShard(opts.TempDir, st, fs, rgRows)
			if err != nil {
				return err
			}
			cur = w
		}
		if err := cur.w.Write(rows); err != nil {
			return err
		}
		cur.rows += int64(len(rows))
		var n int64
		for i, f := range st.Fields {
			col := make([]any, len(rows))
			for r := range rows {
				col[r] = rows[r][i]
			}
			n += features.NBytes(f.Type, col)
		}
		sr.NumBytes += n
		sr.NumRows += int64(len(rows))
		shardBytes += n
		if shardBytes > opts.ShardBytes {
			return closeShard()
		}
		return nil
	})
	if err != nil && !errors.Is(err, errLimit) {
		if cur != nil {
			cur.abort()
		}
		for _, sh := range sr.Shards {
			_ = os.Remove(sh.LocalPath)
		}
		return nil, false, err
	}
	if err := closeShard(); err != nil {
		return nil, false, err
	}
	if len(sr.Shards) == 0 {
		// An empty Split still gets one Parquet file with its schema.
		w, err := newShard(opts.TempDir, st, fs, rgRows)
		if err != nil {
			return nil, false, err
		}
		sh, err := w.finish()
		if err != nil {
			return nil, false, err
		}
		sr.Shards = append(sr.Shards, sh)
	}
	return sr, partial, nil
}

// copyParquet copies source Parquet files as they are, as the Hub does for
// Parquet datasets, reading only their footers for row counts.
func copyParquet(ctx context.Context, res *ConfigResult, splits []splitFiles) (*ConfigResult, error) {
	res.Copied = true
	for _, sf := range splits {
		sr := SplitResult{Name: sf.name}
		if len(sf.files) == 0 {
			return nil, fmt.Errorf("empty parquet data_files for split %s", sf.name)
		}
		for i, f := range sf.files {
			pf, err := openParquet(ctx, f)
			if err != nil {
				return nil, err
			}
			if f.OID == "" {
				return nil, fmt.Errorf("%w: %s is not stored in LFS, so it cannot be copied", ErrUnsupported, f.Path)
			}
			sr.Shards = append(sr.Shards, Shard{ExportPath: ExportPath(res.Name, sf.name, i, len(sf.files), false),
				CopyOID: f.OID, SourcePath: f.Path, Size: f.Size, SHA256: f.OID, NumRows: pf.NumRows()})
			sr.NumRows += pf.NumRows()
			sr.NumBytes += pf.UncompressedBytes()
		}
		res.Splits = append(res.Splits, sr)
	}
	return res, nil
}

// DownloadChecksumKey is the download_checksums key `datasets` uses for a file.
func DownloadChecksumKey(repoURL, p string) string { return repoURL + "/" + path.Clean(p) }
