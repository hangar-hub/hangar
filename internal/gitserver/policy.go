package gitserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"

	"github.com/hangar-hub/hangar/internal/model"
)

// verifyPoll is how often a push re-checks an LFS object that is still
// being verified.
const verifyPoll = 250 * time.Millisecond

// maxPathSearch bounds the tree walk that names oversized files in a
// refusal message.
const maxPathSearch = 200000

// checkObjects enforces the per-object push policy: non-LFS blobs are under
// git.max_blob_size, and every LFS pointer refers to an object that is
// stored and verified, with a matching size.
func (h *pushHook) checkObjects(ctx context.Context, objs []plumbing.Hash, cmds []*packp.Command) error {
	pointers := map[string]int64{}
	var oversized []plumbing.Hash
	sizes := map[plumbing.Hash]int64{}
	for _, id := range objs {
		obj, err := h.st.EncodedObject(plumbing.AnyObject, id)
		if err != nil {
			return fmt.Errorf("read pushed object %s: %w", id, err)
		}
		if obj.Type() != plumbing.BlobObject {
			continue
		}
		size := obj.Size()
		if size <= model.MaxLFSPointerSize {
			content, err := readObject(obj)
			if err != nil {
				return err
			}
			if p, err := model.ParseLFSPointer(content); err == nil {
				pointers[p.OID] = p.Size
				continue
			}
		}
		if h.s.cfg.MaxBlobSize > 0 && size > h.s.cfg.MaxBlobSize {
			oversized = append(oversized, id)
			sizes[id] = size
		}
	}
	if len(oversized) > 0 {
		paths := h.findPaths(oversized, cmds)
		id := oversized[0]
		name := paths[id]
		if name == "" {
			name = "a file (blob " + id.String() + ")"
		}
		return reject("%s is %s, over the %s limit for files stored in git (git.max_blob_size). Track it with Git LFS (git lfs track), commit it again and push",
			name, formatSize(sizes[id]), formatSize(h.s.cfg.MaxBlobSize))
	}
	if len(pointers) > 0 {
		return h.checkLFS(ctx, pointers)
	}
	return nil
}

func readObject(obj plumbing.EncodedObject) ([]byte, error) {
	rc, err := obj.Reader()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, model.MaxLFSPointerSize+1))
}

// checkLFS makes sure every pointer's object is stored and verified, waiting
// up to lfs.verify_wait for objects that are still being verified.
func (h *pushHook) checkLFS(ctx context.Context, pointers map[string]int64) error {
	oids := make([]string, 0, len(pointers))
	for oid := range pointers {
		oids = append(oids, oid)
	}
	sort.Strings(oids)
	deadline := h.s.now().Add(h.s.cfg.VerifyWait)
	for {
		stored, err := h.s.deps.LFS.StatMany(ctx, oids)
		if err != nil {
			return fmt.Errorf("check lfs objects: %w", err)
		}
		var pending []string
		for _, oid := range oids {
			o, ok := stored[oid]
			if !ok {
				return reject("this push refers to the Git LFS object sha256:%s, which Hangar does not have. Install Git LFS (git lfs install) so that git push uploads LFS objects, then push again", oid)
			}
			if o.Verified && o.Size != pointers[oid] {
				return reject("the Git LFS pointer for sha256:%s says %d bytes, but the stored object has %d bytes", oid, pointers[oid], o.Size)
			}
			if !o.Verified {
				pending = append(pending, oid)
			}
		}
		if len(pending) == 0 {
			return nil
		}
		if !h.s.now().Before(deadline) {
			return reject("the Git LFS object sha256:%s is still being verified. Push again in a few minutes", pending[0])
		}
		oids = pending
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(verifyPoll):
		}
	}
}

// findPaths names the files that hold the given blobs in the pushed tips,
// for refusal messages. It stops when every blob is named or after
// maxPathSearch files.
func (h *pushHook) findPaths(blobs []plumbing.Hash, cmds []*packp.Command) map[plumbing.Hash]string {
	want := map[plumbing.Hash]bool{}
	for _, b := range blobs {
		want[b] = true
	}
	found := map[plumbing.Hash]string{}
	seen := 0
	for _, c := range cmds {
		if c.Action() == packp.Delete {
			continue
		}
		commit, err := object.GetCommit(h.st, c.New)
		if err != nil {
			continue
		}
		tree, err := commit.Tree()
		if err != nil {
			continue
		}
		err = tree.Files().ForEach(func(f *object.File) error {
			seen++
			if want[f.Hash] {
				if _, ok := found[f.Hash]; !ok {
					found[f.Hash] = f.Name
				}
			}
			if len(found) == len(want) || seen > maxPathSearch {
				return errStopWalk
			}
			return nil
		})
		if err != nil && !errors.Is(err, errStopWalk) {
			continue
		}
		if len(found) == len(want) || seen > maxPathSearch {
			break
		}
	}
	return found
}

var errStopWalk = errors.New("stop")

// formatSize formats a byte count with binary units, as the configuration uses.
func formatSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	v := float64(n) / unit
	i := 0
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d %s", int64(v), units[i])
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}
