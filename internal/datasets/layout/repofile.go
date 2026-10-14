package layout

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
)

// RepoFile reads ranges of a file at a Commit through the Repo service,
// which reads LFS objects from the Blob store with ranged reads. Reads are
// cached in blocks, so Parquet footers and pages cost few requests.
type RepoFile struct {
	ctx    context.Context
	repos  repo.Service
	id     model.RepoID
	commit model.CommitSHA
	path   string
	size   int64

	mu     sync.Mutex
	blocks map[int64][]byte
	order  []int64
}

// Block and cache sizes of RepoFile.
const (
	blockSize = 1 << 20
	maxBlocks = 64
)

// OpenRepoFile returns random access to a file. size is the file's size.
func OpenRepoFile(ctx context.Context, repos repo.Service, id model.RepoID, commit model.CommitSHA, path string, size int64) *RepoFile {
	return &RepoFile{ctx: ctx, repos: repos, id: id, commit: commit, path: path, size: size, blocks: map[int64][]byte{}}
}

// Size returns the file size.
func (f *RepoFile) Size() int64 { return f.size }

func (f *RepoFile) block(n int64) ([]byte, error) {
	f.mu.Lock()
	if b, ok := f.blocks[n]; ok {
		f.mu.Unlock()
		return b, nil
	}
	f.mu.Unlock()
	start := n * blockSize
	end := min(start+blockSize, f.size) - 1
	rc, _, err := f.repos.OpenFile(f.ctx, f.id, f.commit, f.path, &blob.Range{Start: start, End: end})
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b := make([]byte, end-start+1)
	if _, err := io.ReadFull(rc, b); err != nil {
		return nil, fmt.Errorf("read %s: %w", f.path, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.order) >= maxBlocks {
		delete(f.blocks, f.order[0])
		f.order = f.order[1:]
	}
	f.blocks[n] = b
	f.order = append(f.order, n)
	return b, nil
}

// ReadAt implements io.ReaderAt.
func (f *RepoFile) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("negative offset")
	}
	if off >= f.size {
		return 0, io.EOF
	}
	n := 0
	for n < len(p) && off < f.size {
		b, err := f.block(off / blockSize)
		if err != nil {
			return n, err
		}
		k := copy(p[n:], b[off%blockSize:])
		n += k
		off += int64(k)
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}
