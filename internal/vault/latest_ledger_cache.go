package vault

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// LatestLedgerSource is the one RPC call the status endpoint needs.
type LatestLedgerSource interface {
	GetLatestLedgerSequence(ctx context.Context) (uint32, error)
}

// DefaultLatestLedgerLookupTimeout bounds one chain-tip lookup, so a hung RPC
// turns into status "stalled" quickly instead of tying up the request.
const DefaultLatestLedgerLookupTimeout = 2 * time.Second

// LatestLedgerCache caches the chain tip for a short TTL so GET .../status
// does not cost an RPC round trip per request.
//
// One cache is shared by every tenant. That is safe because the chain tip is
// public, network-wide data; nothing tenant-specific is ever cached here.
//
// Semantics:
//   - Concurrent misses are collapsed: the lock is held across the lookup,
//     so N simultaneous requests make one RPC call.
//   - Errors are cached for the same TTL, so during an outage requests fail
//     fast instead of each waiting out its own timeout.
//   - There is no stale fallback. When the cached value has expired and the
//     refresh fails, the caller gets the error, never an older tip that could
//     make an outage look healthy.
type LatestLedgerCache struct {
	source  LatestLedgerSource
	ttl     time.Duration
	timeout time.Duration
	now     func() time.Time

	mu        sync.Mutex
	fetched   bool
	fetchedAt time.Time
	ledger    uint32
	err       error
}

// NewLatestLedgerCache builds a cache. A nil now uses time.Now.
func NewLatestLedgerCache(source LatestLedgerSource, ttl, timeout time.Duration, now func() time.Time) (*LatestLedgerCache, error) {
	if source == nil {
		return nil, errors.New("a latest-ledger source is required")
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("cache TTL must be positive, got %s", ttl)
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("lookup timeout must be positive, got %s", timeout)
	}
	if now == nil {
		now = time.Now
	}
	return &LatestLedgerCache{source: source, ttl: ttl, timeout: timeout, now: now}, nil
}

// Get returns the chain tip, refreshing it if the cached entry has expired.
func (c *LatestLedgerCache) Get(ctx context.Context) (uint32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.fetched && c.now().Sub(c.fetchedAt) < c.ttl {
		return c.ledger, c.err
	}

	// The lookup must not inherit the triggering request's cancellation:
	// its result is shared, so one client disconnecting must not cache a
	// "context canceled" error for everyone else.
	lookupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.timeout)
	defer cancel()

	ledger, err := c.source.GetLatestLedgerSequence(lookupCtx)
	if err != nil {
		err = fmt.Errorf("getting the latest ledger: %w", err)
		ledger = 0
	}

	c.fetched, c.fetchedAt, c.ledger, c.err = true, c.now(), ledger, err
	return c.ledger, c.err
}
