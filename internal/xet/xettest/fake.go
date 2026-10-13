// Package xettest provides a fake xet.Ingestor. It issues and verifies
// opaque tokens and stores xorbs without parsing them; AcceptShard reports
// the files set with SetShardFiles. Use it to test the Hub token endpoints
// and the CAS HTTP handlers, not the Xet formats.
package xettest

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/xet"
)

// Fake is a fake xet.Ingestor.
type Fake struct {
	CASURL string
	TTL    time.Duration
	// Reject turns on reject mode.
	Reject bool

	mu     sync.Mutex
	tokens map[string]xet.Claims
	xorbs  map[string][]byte
	files  []xet.ReassembledFile
	shards int
	now    func() time.Time
}

var _ xet.Ingestor = (*Fake)(nil)

// NewFake returns a Fake whose tokens name casURL and last ttl.
func NewFake(casURL string, ttl time.Duration) *Fake {
	return &Fake{CASURL: casURL, TTL: ttl, tokens: map[string]xet.Claims{}, xorbs: map[string][]byte{}, now: time.Now}
}

// SetClock makes token expiry use now.
func (f *Fake) SetClock(now func() time.Time) { f.mu.Lock(); f.now = now; f.mu.Unlock() }

// SetShardFiles sets the files the next shards report as reassembled.
func (f *Fake) SetShardFiles(files ...xet.ReassembledFile) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files = files
}

// Xorb returns a stored xorb's bytes.
func (f *Fake) Xorb(hash string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.xorbs[hash]
	return b, ok
}

// Shards returns how many shards were accepted.
func (f *Fake) Shards() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.shards
}

// IssueWriteToken implements xet.Ingestor.
func (f *Fake) IssueWriteToken(_ context.Context, req xet.TokenRequest) (xet.Token, error) {
	if f.Reject {
		return xet.Token{}, xet.ErrRejected
	}
	if req.Principal.IsAnonymous() {
		return xet.Token{}, fmt.Errorf("%w: sign in to upload", xet.ErrScope)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	exp := f.now().Add(f.TTL).Truncate(time.Second)
	tok := "xet_" + uuid.NewString()
	f.tokens[tok] = xet.Claims{UserID: req.Principal.UserID, Username: req.Principal.Username, Repo: req.Repo, RepoUUID: req.RepoUUID,
		Revision: req.Revision, CreatePR: req.CreatePR, Scope: xet.ScopeWrite, Expires: exp}
	return xet.Token{AccessToken: tok, Exp: exp.Unix(), CASURL: f.CASURL}, nil
}

// VerifyToken implements xet.Ingestor. Tokens are valid up to and including Exp.
func (f *Fake) VerifyToken(_ context.Context, token string) (*xet.Claims, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.tokens[token]
	if !ok || f.now().After(c.Expires) {
		return nil, xet.ErrInvalidToken
	}
	return &c, nil
}

func writable(c *xet.Claims) error {
	if c == nil || c.Scope != xet.ScopeWrite {
		return xet.ErrScope
	}
	return nil
}

// PutXorb implements xet.Ingestor.
func (f *Fake) PutXorb(_ context.Context, c *xet.Claims, hash string, r io.Reader, size int64) (bool, error) {
	if err := writable(c); err != nil {
		return false, err
	}
	if !xet.ValidHash(hash) {
		return false, fmt.Errorf("%w: hash %q", xet.ErrBadXorb, hash)
	}
	if size > xet.MaxObjectSize {
		return false, xet.ErrTooLarge
	}
	b, err := io.ReadAll(io.LimitReader(r, xet.MaxObjectSize+1))
	if err != nil {
		return false, err
	}
	if len(b) > xet.MaxObjectSize {
		return false, xet.ErrTooLarge
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.xorbs[hash]; ok {
		return false, nil
	}
	f.xorbs[hash] = b
	return true, nil
}

// AcceptShard implements xet.Ingestor.
func (f *Fake) AcceptShard(_ context.Context, c *xet.Claims, r io.Reader, size int64, events func(xet.ShardEvent)) (*xet.ShardResult, error) {
	if err := writable(c); err != nil {
		return nil, err
	}
	if size > xet.MaxObjectSize {
		return nil, xet.ErrTooLarge
	}
	if _, err := io.Copy(io.Discard, r); err != nil {
		return nil, err
	}
	emit := func(e xet.ShardEvent) {
		if events != nil {
			events(e)
		}
	}
	f.mu.Lock()
	files := append([]xet.ReassembledFile(nil), f.files...)
	f.shards++
	f.mu.Unlock()
	emit(xet.ShardEvent{Type: xet.EventValidating, Verified: len(files), Total: len(files)})
	emit(xet.ShardEvent{Type: xet.EventCommitting, Stage: "uploading"})
	emit(xet.ShardEvent{Type: xet.EventResult, Result: 1})
	return &xet.ShardResult{Result: 1, Files: files}, nil
}
