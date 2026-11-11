package vault

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeTipRPC is a switchable chain-tip source: it can answer, fail, or hang
// until its context is done (the "RPC accepts the connection and never
// replies" case, which is the one a plain error test would miss).
type fakeTipRPC struct {
	mu     sync.Mutex
	ledger uint32
	err    error
	hang   bool
	delay  time.Duration
	calls  atomic.Int32
}

func (f *fakeTipRPC) set(ledger uint32, err error, hang bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ledger, f.err, f.hang = ledger, err, hang
}

func (f *fakeTipRPC) GetLatestLedgerSequence(ctx context.Context) (uint32, error) {
	f.calls.Add(1)
	f.mu.Lock()
	ledger, err, hang, delay := f.ledger, f.err, f.hang, f.delay
	f.mu.Unlock()

	if hang {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	if delay > 0 {
		time.Sleep(delay)
	}
	return ledger, err
}

// fakeClock is a manually advanced clock, so TTL expiry is tested without
// sleeping.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestCache(t *testing.T, rpc *fakeTipRPC, timeout time.Duration) (*LatestLedgerCache, *fakeClock) {
	t.Helper()
	clock := &fakeClock{now: time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)}
	cache, err := NewLatestLedgerCache(rpc, 5*time.Second, timeout, clock.Now)
	require.NoError(t, err)
	return cache, clock
}

func Test_NewLatestLedgerCache_validates(t *testing.T) {
	rpc := &fakeTipRPC{}
	testCases := []struct {
		name    string
		source  LatestLedgerSource
		ttl     time.Duration
		timeout time.Duration
		wantErr string
	}{
		{"nil source", nil, time.Second, time.Second, "source is required"},
		{"zero ttl", rpc, 0, time.Second, "TTL must be positive"},
		{"negative ttl", rpc, -time.Second, time.Second, "TTL must be positive"},
		{"zero timeout", rpc, time.Second, 0, "timeout must be positive"},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewLatestLedgerCache(tc.source, tc.ttl, tc.timeout, nil)
			require.ErrorContains(t, err, tc.wantErr)
		})
	}

	cache, err := NewLatestLedgerCache(rpc, time.Second, time.Second, nil)
	require.NoError(t, err)
	assert.NotNil(t, cache.now, "nil clock defaults to time.Now")
}

func Test_LatestLedgerCache_servesFromCacheWithinTTL(t *testing.T) {
	rpc := &fakeTipRPC{ledger: 100}
	cache, clock := newTestCache(t, rpc, time.Second)
	ctx := context.Background()

	got, err := cache.Get(ctx)
	require.NoError(t, err)
	assert.Equal(t, uint32(100), got)

	rpc.set(105, nil, false)
	clock.Advance(4999 * time.Millisecond) // TTL - 1ms
	got, err = cache.Get(ctx)
	require.NoError(t, err)
	assert.Equal(t, uint32(100), got, "inside the TTL the cached tip is served")
	assert.Equal(t, int32(1), rpc.calls.Load())

	clock.Advance(time.Millisecond) // exactly the TTL: expired
	got, err = cache.Get(ctx)
	require.NoError(t, err)
	assert.Equal(t, uint32(105), got)
	assert.Equal(t, int32(2), rpc.calls.Load())
}

func Test_LatestLedgerCache_cachesErrorsAndNeverServesStale(t *testing.T) {
	rpc := &fakeTipRPC{ledger: 100}
	cache, clock := newTestCache(t, rpc, time.Second)
	ctx := context.Background()

	_, err := cache.Get(ctx)
	require.NoError(t, err)

	// RPC goes down. Once the good entry expires, the error is returned —
	// not the old tip, which would make an outage look healthy.
	rpc.set(0, errors.New("connection refused"), false)
	clock.Advance(5 * time.Second)
	got, err := cache.Get(ctx)
	require.ErrorContains(t, err, "connection refused")
	assert.Zero(t, got, "no stale fallback")
	assert.Equal(t, int32(2), rpc.calls.Load())

	// The error itself is cached: no second RPC call within the TTL.
	clock.Advance(4 * time.Second)
	_, err = cache.Get(ctx)
	require.Error(t, err)
	assert.Equal(t, int32(2), rpc.calls.Load(), "errors are cached for the TTL")

	// RPC recovers; after the TTL the next request sees it.
	rpc.set(130, nil, false)
	clock.Advance(time.Second)
	got, err = cache.Get(ctx)
	require.NoError(t, err)
	assert.Equal(t, uint32(130), got)
}

func Test_LatestLedgerCache_hungRPCTimesOut(t *testing.T) {
	rpc := &fakeTipRPC{hang: true}
	cache, _ := newTestCache(t, rpc, 50*time.Millisecond)

	start := time.Now()
	_, err := cache.Get(context.Background())
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 2*time.Second, "a hung RPC is cut off by the lookup timeout")
}

func Test_LatestLedgerCache_callerCancellationDoesNotPoisonTheCache(t *testing.T) {
	rpc := &fakeTipRPC{ledger: 100, delay: 20 * time.Millisecond}
	cache, _ := newTestCache(t, rpc, time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the triggering client has already gone away

	got, err := cache.Get(ctx)
	require.NoError(t, err, "the shared lookup must not inherit one caller's cancellation")
	assert.Equal(t, uint32(100), got)
}

func Test_LatestLedgerCache_collapsesConcurrentMisses(t *testing.T) {
	rpc := &fakeTipRPC{ledger: 100, delay: 50 * time.Millisecond}
	cache, _ := newTestCache(t, rpc, time.Second)

	const callers = 25
	var wg sync.WaitGroup
	results := make([]uint32, callers)
	errs := make([]error, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = cache.Get(context.Background())
		}()
	}
	wg.Wait()

	for i := range callers {
		require.NoError(t, errs[i])
		assert.Equal(t, uint32(100), results[i])
	}
	assert.Equal(t, int32(1), rpc.calls.Load(), "N concurrent misses make one RPC call")
}
