package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/google/uuid"
)

// ErrQuotaExceeded means storing what a Pull fetched would take the
// Mirror's Namespace over its storage quota (OPS-4). Errors that wrap it
// carry a message written for users.
var ErrQuotaExceeded = errors.New("storage quota exceeded")

// QuotaChecker refuses storage that would take a Namespace over its quota
// (OPS-4). Pulls consult it before they apply a fetched pack, before each
// LFS object transfer (Xet-backed upstream files arrive through the same
// transfers, because Hangar never asks an Upstream for Xet) and before
// they mark objects the Instance already holds as present in a Mirror.
//
// W1's gc.Accounting provides both methods; the integration lead wires it
// through an adapter whose CheckQuota returns QuotaExceeded(err) when
// errors.Is(err, gc.ErrQuotaExceeded).
type QuotaChecker interface {
	// CheckQuota returns an error wrapping ErrQuotaExceeded, with a
	// message for users, when storing adding more bytes would take the
	// Namespace over its quota. Any other error is a failure to check,
	// which a Pull retries.
	CheckQuota(ctx context.Context, namespaceID uuid.UUID, adding int64) error
	// NewLFSBytes returns the total size of the objects (OID to size) that
	// no Repo of the Namespace holds yet.
	NewLFSBytes(ctx context.Context, namespaceID uuid.UUID, objects map[string]int64) (int64, error)
}

// QuotaExceeded marks err, a quota refusal written for users, as
// ErrQuotaExceeded while keeping its message and its own error chain.
func QuotaExceeded(err error) error {
	if err == nil {
		return nil
	}
	return &quotaError{err: err}
}

type quotaError struct{ err error }

func (e *quotaError) Error() string   { return e.err.Error() }
func (e *quotaError) Unwrap() []error { return []error{ErrQuotaExceeded, e.err} }

// reasonQuotaExceeded is the failure reason of a Pull, and of a pull.file
// Job, that the Namespace quota stopped.
const reasonQuotaExceeded = "quota_exceeded"

// quotaMessage is the progress message of a Pull the quota stopped.
const quotaMessage = "Storage quota exceeded"

// checkQuota refuses storing extra bytes plus the objects (OID to size)
// that the Namespace does not hold yet. It allows everything when no
// QuotaChecker is set.
func (s *Service) checkQuota(ctx context.Context, namespaceID uuid.UUID, objects map[string]int64, extra int64) error {
	q := s.opts.Quota
	if q == nil {
		return nil
	}
	adding := extra
	if len(objects) > 0 {
		n, err := q.NewLFSBytes(ctx, namespaceID, objects)
		if err != nil {
			return fmt.Errorf("check the storage quota: %w", err)
		}
		adding += n
	}
	if adding <= 0 {
		return nil
	}
	if err := q.CheckQuota(ctx, namespaceID, adding); err != nil {
		if errors.Is(err, ErrQuotaExceeded) {
			return err
		}
		return fmt.Errorf("check the storage quota: %w", err)
	}
	return nil
}

// spoolPack copies a fetched pack to a temporary file in SpoolDir, so its
// size is known before anything is stored. The caller closes the file,
// which removes it.
func (s *Service) spoolPack(pack io.Reader) (*spooledPack, error) {
	f, err := os.CreateTemp(s.opts.SpoolDir, "pack-*.pack")
	if err != nil {
		return nil, fmt.Errorf("spool the upstream history: %w", err)
	}
	sp := &spooledPack{File: f}
	n, err := io.Copy(f, pack)
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err != nil {
		_ = sp.Close()
		return nil, fmt.Errorf("spool the upstream history: %w", err)
	}
	sp.size = n
	return sp, nil
}

type spooledPack struct {
	*os.File
	size int64
}

// Close closes and removes the spool file.
func (p *spooledPack) Close() error {
	err := p.File.Close()
	if rerr := os.Remove(p.Name()); rerr != nil && !errors.Is(rerr, os.ErrNotExist) && err == nil {
		err = rerr
	}
	return err
}
