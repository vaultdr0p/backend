package data

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stellar/stellar-disbursement-platform-backend/db"
	"github.com/stellar/stellar-disbursement-platform-backend/db/dbtest"
)

// Real testnet addresses, so the VARCHAR(56) columns and any downstream
// strkey validation see genuine values.
const (
	testVaultContractID = "CB5HTYC4GZW67ET23SACDCWCCDQ3AKU4TYX7SAUQJSKS6LLQLYA4HG53"
	testVaultTokenID    = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
	testVaultAdmin      = "GAODBHVR63Z56MVQRBEJSYM2H5423LJ4WAPUUBOFG4JYY72S6ROKVZRX"
	testRecipientA      = "GCNMZYNV3NVFLK25K3KLZ2LM6HAAD4BVYHULQX746XERD6HQRGGJ6SLD"
	testRecipientB      = "GD6KZDL3Q7DO5AV3FHR44INMTTWPRMHZVLHKGCTCIQ23OM22FHYXBLBF"
)

// newVaultTestDB opens a migrated database and returns the model under test.
// Each test gets its own, so ordering between tests cannot matter.
func newVaultTestDB(t *testing.T) (context.Context, db.DBConnectionPool, VaultModel) {
	t.Helper()

	dbt := dbtest.Open(t)
	t.Cleanup(func() { dbt.Close() })

	dbConnectionPool, err := db.OpenDBConnectionPool(dbt.DSN)
	require.NoError(t, err)
	t.Cleanup(func() { dbConnectionPool.Close() })

	return context.Background(), dbConnectionPool, VaultModel{dbConnectionPool: dbConnectionPool}
}

// bootstrapVault creates the indexer row and the cached state row, which is
// the precondition for every projection.
func bootstrapVault(t *testing.T, ctx context.Context, sqlExec db.SQLExecuter, m VaultModel) {
	t.Helper()

	_, err := m.BootstrapIndexerState(ctx, sqlExec, VaultIndexerState{
		ContractID: testVaultContractID,
		LastCursor: "0000000000000000000-0000000000",
		LastLedger: 100,
		Status:     VaultIndexerStatusHealthy,
	})
	require.NoError(t, err)

	require.NoError(t, m.UpsertState(ctx, sqlExec, VaultState{
		ContractID:     testVaultContractID,
		AdminAddress:   testVaultAdmin,
		TokenAddress:   testVaultTokenID,
		Deadline:       time.Now().Add(24 * time.Hour).UTC().Truncate(time.Microsecond),
		SnapshotLedger: 100,
	}))
}

// applied asserts that a vault_state update succeeded and found its row.
func applied(t *testing.T) func(StateUpdate, error) {
	return func(update StateUpdate, err error) {
		t.Helper()
		require.NoError(t, err)
		require.True(t, update.RowExists, "the summary row must exist")
	}
}

func newVaultEvent(eventType string, eventID string, ledger int32, address string, amount *VaultAmount) VaultEvent {
	return VaultEvent{
		EventID:        eventID,
		ContractID:     testVaultContractID,
		EventType:      eventType,
		Ledger:         ledger,
		LedgerClosedAt: time.Now().UTC().Truncate(time.Microsecond),
		TxHash:         fmt.Sprintf("%064d", ledger),
		Address:        &address,
		Amount:         amount,
		Payload:        json.RawMessage(`{"topic":[],"value":{}}`),
	}
}

func Test_VaultModel_BootstrapIndexerState(t *testing.T) {
	ctx, dbConnectionPool, m := newVaultTestDB(t)

	first, err := m.BootstrapIndexerState(ctx, dbConnectionPool, VaultIndexerState{
		ContractID: testVaultContractID,
		LastCursor: "cursor-1",
		LastLedger: 500,
		Status:     VaultIndexerStatusHealthy,
	})
	require.NoError(t, err)
	assert.Equal(t, "cursor-1", first.LastCursor)
	assert.Equal(t, int32(500), first.LastLedger)
	assert.Equal(t, VaultIndexerStatusHealthy, first.Status)
	assert.Nil(t, first.GapFromLedger)

	// Bootstrapping again must not reset a cursor that has already advanced:
	// two schedulers racing on the same tenant would otherwise rewind each
	// other. The existing row comes back instead.
	require.NoError(t, m.AdvanceCursor(ctx, dbConnectionPool, testVaultContractID, "cursor-2", 900))

	second, err := m.BootstrapIndexerState(ctx, dbConnectionPool, VaultIndexerState{
		ContractID: testVaultContractID,
		LastCursor: "cursor-rewound",
		LastLedger: 1,
		Status:     VaultIndexerStatusHealthy,
	})
	require.NoError(t, err)
	assert.Equal(t, "cursor-2", second.LastCursor)
	assert.Equal(t, int32(900), second.LastLedger)
}

func Test_VaultModel_GetIndexerState_notFound(t *testing.T) {
	ctx, dbConnectionPool, m := newVaultTestDB(t)

	state, err := m.GetIndexerState(ctx, dbConnectionPool, testVaultContractID)

	assert.ErrorIs(t, err, ErrRecordNotFound)
	assert.Nil(t, state)
}

func Test_VaultModel_AdvanceCursor(t *testing.T) {
	ctx, dbConnectionPool, m := newVaultTestDB(t)
	bootstrapVault(t, ctx, dbConnectionPool, m)

	require.NoError(t, m.AdvanceCursor(ctx, dbConnectionPool, testVaultContractID, "cursor-abc", 1234))

	state, err := m.GetIndexerState(ctx, dbConnectionPool, testVaultContractID)
	require.NoError(t, err)
	assert.Equal(t, "cursor-abc", state.LastCursor)
	assert.Equal(t, int32(1234), state.LastLedger)
	require.NotNil(t, state.LastIndexedAt)
	assert.WithinDuration(t, time.Now(), *state.LastIndexedAt, time.Minute)

	// Advancing a vault that was never bootstrapped is a caller bug, not a
	// state to tolerate silently.
	err = m.AdvanceCursor(ctx, dbConnectionPool, "CNOTBOOTSTRAPPED", "cursor", 1)
	assert.ErrorIs(t, err, ErrRecordNotFound)
}

func Test_VaultModel_MarkDegraded(t *testing.T) {
	ctx, dbConnectionPool, m := newVaultTestDB(t)
	bootstrapVault(t, ctx, dbConnectionPool, m)

	require.NoError(t, m.MarkDegraded(ctx, dbConnectionPool, testVaultContractID, 101, 5000,
		"events between ledgers 101 and 5000 are outside the RPC retention window"))

	state, err := m.GetIndexerState(ctx, dbConnectionPool, testVaultContractID)
	require.NoError(t, err)
	assert.Equal(t, VaultIndexerStatusDegraded, state.Status)
	require.NotNil(t, state.GapFromLedger)
	require.NotNil(t, state.GapToLedger)
	assert.Equal(t, int32(101), *state.GapFromLedger)
	assert.Equal(t, int32(5000), *state.GapToLedger)
	require.NotNil(t, state.DegradedReason)
	assert.Contains(t, *state.DegradedReason, "retention window")
}

func Test_VaultModel_InsertEvent_isIdempotentOnEventID(t *testing.T) {
	ctx, dbConnectionPool, m := newVaultTestDB(t)
	bootstrapVault(t, ctx, dbConnectionPool, m)

	amount := NewVaultAmount(10_000_000)
	event := newVaultEvent("funded", "0000000000000000001-0000000000", 101, testVaultAdmin, &amount)

	inserted, err := m.InsertEvent(ctx, dbConnectionPool, event)
	require.NoError(t, err)
	assert.True(t, inserted, "a new event must report inserted")

	// Polling is at-least-once, so the same event will be re-presented. The
	// second attempt must report false — that is the signal the caller uses to
	// skip the projection, and it is the only thing standing between a replay
	// and a double-counted balance.
	inserted, err = m.InsertEvent(ctx, dbConnectionPool, event)
	require.NoError(t, err)
	assert.False(t, inserted, "a replayed event must report not inserted")

	var count int
	require.NoError(t, dbConnectionPool.GetContext(ctx, &count, `SELECT COUNT(*) FROM vault_events`))
	assert.Equal(t, 1, count)
}

func Test_VaultModel_InsertEvent_preservesAmountExactly(t *testing.T) {
	ctx, dbConnectionPool, m := newVaultTestDB(t)
	bootstrapVault(t, ctx, dbConnectionPool, m)

	amount, err := ParseVaultAmount(i128Max)
	require.NoError(t, err)

	_, err = m.InsertEvent(ctx, dbConnectionPool,
		newVaultEvent("funded", "0000000000000000001-0000000000", 101, testVaultAdmin, &amount))
	require.NoError(t, err)

	events, err := m.ListEvents(ctx, dbConnectionPool, VaultEventFilter{ContractID: testVaultContractID}, VaultPage{})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.NotNil(t, events[0].Amount)
	assert.Equal(t, i128Max, events[0].Amount.String(), "i128::MAX must survive NUMERIC(40,0) unchanged")
}

func Test_VaultModel_InsertEvent_rejectsAnUnknownEventType(t *testing.T) {
	ctx, dbConnectionPool, m := newVaultTestDB(t)
	bootstrapVault(t, ctx, dbConnectionPool, m)

	// vault_events_type_check is the backstop for the decoder's event-type
	// allowlist: a type that got past the code still cannot reach a row.
	_, err := m.InsertEvent(ctx, dbConnectionPool,
		newVaultEvent("topped_up", "0000000000000000001-0000000000", 101, testVaultAdmin, nil))

	assert.ErrorContains(t, err, "vault_events_type_check")
}

func Test_VaultModel_allocationLifecycle(t *testing.T) {
	ctx, dbConnectionPool, m := newVaultTestDB(t)
	bootstrapVault(t, ctx, dbConnectionPool, m)

	registeredAt := time.Now().UTC().Truncate(time.Microsecond)
	require.NoError(t, m.RegisterAllocation(ctx, dbConnectionPool, VaultAllocation{
		ContractID:       testVaultContractID,
		Recipient:        testRecipientA,
		Amount:           NewVaultAmount(10_000_000),
		RegisteredLedger: 101,
		RegisteredAt:     registeredAt,
		LastEventID:      "event-register",
	}))

	allocation, err := m.GetAllocation(ctx, dbConnectionPool, testVaultContractID, testRecipientA)
	require.NoError(t, err)
	assert.Equal(t, VaultAllocationStatusPending, allocation.Status)
	assert.Equal(t, "10000000", allocation.Amount.String())
	assert.Nil(t, allocation.SettledLedger)
	assert.Nil(t, allocation.SettledAt)

	settledAt := registeredAt.Add(time.Minute)
	settled, err := m.SettleAllocation(ctx, dbConnectionPool, testVaultContractID, testRecipientA,
		VaultAllocationStatusClaimed, 150, settledAt, "event-claim")
	require.NoError(t, err)
	assert.True(t, settled)

	allocation, err = m.GetAllocation(ctx, dbConnectionPool, testVaultContractID, testRecipientA)
	require.NoError(t, err)
	assert.Equal(t, VaultAllocationStatusClaimed, allocation.Status)
	require.NotNil(t, allocation.SettledLedger)
	assert.Equal(t, int32(150), *allocation.SettledLedger)
	require.NotNil(t, allocation.SettledAt)
	assert.Equal(t, settledAt, allocation.SettledAt.UTC())
}

func Test_VaultModel_SettleAllocation_illegalTransitions(t *testing.T) {
	// The contract's lifecycle is monotonic and one-shot: register fails if an
	// allocation exists in any status, and claim and cancel both require
	// Pending. These are the transitions that must therefore be unreachable —
	// enforced by SQL, not only by the caller, so a projection bug cannot
	// rewrite settled history.
	testCases := []struct {
		name          string
		initialStatus VaultAllocationStatus
		target        VaultAllocationStatus
	}{
		{name: "claimed cannot be walked back to claimed", initialStatus: VaultAllocationStatusClaimed, target: VaultAllocationStatusClaimed},
		{name: "claimed cannot become cancelled", initialStatus: VaultAllocationStatusClaimed, target: VaultAllocationStatusCancelled},
		{name: "cancelled cannot become claimed", initialStatus: VaultAllocationStatusCancelled, target: VaultAllocationStatusClaimed},
		{name: "cancelled cannot be re-cancelled", initialStatus: VaultAllocationStatusCancelled, target: VaultAllocationStatusCancelled},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, dbConnectionPool, m := newVaultTestDB(t)
			bootstrapVault(t, ctx, dbConnectionPool, m)

			require.NoError(t, m.RegisterAllocation(ctx, dbConnectionPool, VaultAllocation{
				ContractID:       testVaultContractID,
				Recipient:        testRecipientA,
				Amount:           NewVaultAmount(10_000_000),
				RegisteredLedger: 101,
				RegisteredAt:     time.Now().UTC(),
				LastEventID:      "event-register",
			}))

			// Move it into its initial terminal state.
			settled, err := m.SettleAllocation(ctx, dbConnectionPool, testVaultContractID, testRecipientA,
				tc.initialStatus, 150, time.Now().UTC(), "event-first")
			require.NoError(t, err)
			require.True(t, settled)

			// Now attempt the illegal transition.
			settled, err = m.SettleAllocation(ctx, dbConnectionPool, testVaultContractID, testRecipientA,
				tc.target, 200, time.Now().UTC(), "event-second")
			require.NoError(t, err)
			assert.False(t, settled, "an illegal transition must move no rows")

			// And the row must be untouched.
			allocation, err := m.GetAllocation(ctx, dbConnectionPool, testVaultContractID, testRecipientA)
			require.NoError(t, err)
			assert.Equal(t, tc.initialStatus, allocation.Status)
			assert.Equal(t, "event-first", allocation.LastEventID)
			require.NotNil(t, allocation.SettledLedger)
			assert.Equal(t, int32(150), *allocation.SettledLedger)
		})
	}
}

func Test_VaultModel_SettleAllocation_missingRowIsNotAnError(t *testing.T) {
	ctx, dbConnectionPool, m := newVaultTestDB(t)
	bootstrapVault(t, ctx, dbConnectionPool, m)

	// A `claimed` event whose `registered` event aged out of the RPC retention
	// window before it was read. This is the degraded case: reporting it as an
	// error would wedge the batch, so it reports "no row moved" and the
	// indexer records it as an anomaly instead.
	settled, err := m.SettleAllocation(ctx, dbConnectionPool, testVaultContractID, testRecipientA,
		VaultAllocationStatusClaimed, 150, time.Now().UTC(), "event-claim")

	require.NoError(t, err)
	assert.False(t, settled)
}

func Test_VaultModel_SettleAllocation_rejectsPendingAsATarget(t *testing.T) {
	ctx, dbConnectionPool, m := newVaultTestDB(t)

	_, err := m.SettleAllocation(ctx, dbConnectionPool, testVaultContractID, testRecipientA,
		VaultAllocationStatusPending, 150, time.Now().UTC(), "event")

	assert.ErrorContains(t, err, "requires a terminal status")
}

func Test_VaultModel_RegisterAllocation_rejectsADuplicate(t *testing.T) {
	ctx, dbConnectionPool, m := newVaultTestDB(t)
	bootstrapVault(t, ctx, dbConnectionPool, m)

	allocation := VaultAllocation{
		ContractID:       testVaultContractID,
		Recipient:        testRecipientA,
		Amount:           NewVaultAmount(10_000_000),
		RegisteredLedger: 101,
		RegisteredAt:     time.Now().UTC(),
		LastEventID:      "event-register",
	}
	require.NoError(t, m.RegisterAllocation(ctx, dbConnectionPool, allocation))

	// The contract rejects a second registration with AllocationExists, so a
	// conflict here is a real inconsistency and must not silently overwrite an
	// allocation that may already have been claimed.
	allocation.LastEventID = "event-register-again"
	err := m.RegisterAllocation(ctx, dbConnectionPool, allocation)

	assert.ErrorContains(t, err, "vault_allocations_pkey")
}

func Test_VaultModel_stateProjections(t *testing.T) {
	ctx, dbConnectionPool, m := newVaultTestDB(t)
	bootstrapVault(t, ctx, dbConnectionPool, m)

	applied(t)(m.ApplyFundedBalance(ctx, dbConnectionPool, testVaultContractID, NewVaultAmount(60_000_000), 120))
	applied(t)(m.ApplyTotalAllocated(ctx, dbConnectionPool, testVaultContractID, NewVaultAmount(10_000_000), 121))

	state, err := m.GetState(ctx, dbConnectionPool, testVaultContractID)
	require.NoError(t, err)
	assert.Equal(t, "60000000", state.Balance.String())
	assert.Equal(t, "10000000", state.TotalAllocated.String())
	assert.Equal(t, int32(121), state.SnapshotLedger)
	assert.Equal(t, "50000000", state.Available().String(), "available is balance - total_allocated")
}

func Test_VaultModel_stateProjectionsDoNotGoBackwards(t *testing.T) {
	ctx, dbConnectionPool, m := newVaultTestDB(t)
	bootstrapVault(t, ctx, dbConnectionPool, m)

	applied(t)(m.ApplyFundedBalance(ctx, dbConnectionPool, testVaultContractID, NewVaultAmount(60_000_000), 500))

	// Re-indexing from an old cursor re-presents old events. Applying one must
	// not publish a stale balance over a newer snapshot.
	applied(t)(m.ApplyFundedBalance(ctx, dbConnectionPool, testVaultContractID, NewVaultAmount(10_000_000), 200))

	state, err := m.GetState(ctx, dbConnectionPool, testVaultContractID)
	require.NoError(t, err)
	assert.Equal(t, "60000000", state.Balance.String())
	assert.Equal(t, int32(500), state.SnapshotLedger)

	// An event at exactly the snapshot ledger still applies, because several
	// events share a ledger and the later ones must not be dropped.
	applied(t)(m.ApplyFundedBalance(ctx, dbConnectionPool, testVaultContractID, NewVaultAmount(70_000_000), 500))
	state, err = m.GetState(ctx, dbConnectionPool, testVaultContractID)
	require.NoError(t, err)
	assert.Equal(t, "70000000", state.Balance.String())
}

func Test_VaultState_IsExpired(t *testing.T) {
	deadline := time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC)
	state := VaultState{Deadline: deadline}

	testCases := []struct {
		name string
		now  time.Time
		want bool
	}{
		{name: "a second before the deadline", now: deadline.Add(-time.Second), want: false},
		// The contract's claim bound is exclusive (`timestamp >= deadline`
		// rejects) and cancel's is inclusive, so the deadline instant itself is
		// already expired.
		{name: "exactly at the deadline", now: deadline, want: true},
		{name: "a second after", now: deadline.Add(time.Second), want: true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, state.IsExpired(tc.now))
		})
	}
}

func Test_VaultModel_ListAllocations_filtersAndPaginates(t *testing.T) {
	ctx, dbConnectionPool, m := newVaultTestDB(t)
	bootstrapVault(t, ctx, dbConnectionPool, m)

	// Three allocations in three states, registered in three ledgers.
	for i, recipient := range []string{testRecipientA, testRecipientB, testVaultAdmin} {
		require.NoError(t, m.RegisterAllocation(ctx, dbConnectionPool, VaultAllocation{
			ContractID:       testVaultContractID,
			Recipient:        recipient,
			Amount:           NewVaultAmount(int64(i+1) * 10_000_000),
			RegisteredLedger: int32(100 + i),
			RegisteredAt:     time.Now().UTC(),
			LastEventID:      fmt.Sprintf("event-register-%d", i),
		}))
	}
	_, err := m.SettleAllocation(ctx, dbConnectionPool, testVaultContractID, testRecipientA,
		VaultAllocationStatusClaimed, 200, time.Now().UTC(), "event-claim")
	require.NoError(t, err)
	_, err = m.SettleAllocation(ctx, dbConnectionPool, testVaultContractID, testRecipientB,
		VaultAllocationStatusCancelled, 201, time.Now().UTC(), "event-cancel")
	require.NoError(t, err)

	testCases := []struct {
		name      string
		filter    VaultAllocationFilter
		page      VaultPage
		wantCount int
		wantTotal int
		wantFirst string
	}{
		{
			name:      "no filter returns every allocation, newest ledger first",
			filter:    VaultAllocationFilter{ContractID: testVaultContractID},
			wantCount: 3, wantTotal: 3,
			wantFirst: testVaultAdmin, // registered at ledger 102
		},
		{
			name:      "by status",
			filter:    VaultAllocationFilter{ContractID: testVaultContractID, Status: VaultAllocationStatusClaimed},
			wantCount: 1, wantTotal: 1, wantFirst: testRecipientA,
		},
		{
			name:      "still pending",
			filter:    VaultAllocationFilter{ContractID: testVaultContractID, Status: VaultAllocationStatusPending},
			wantCount: 1, wantTotal: 1, wantFirst: testVaultAdmin,
		},
		{
			name:      "by recipient",
			filter:    VaultAllocationFilter{ContractID: testVaultContractID, Recipient: testRecipientB},
			wantCount: 1, wantTotal: 1, wantFirst: testRecipientB,
		},
		{
			name:      "a filter that matches nothing",
			filter:    VaultAllocationFilter{ContractID: testVaultContractID, Status: VaultAllocationStatusPending, Recipient: testRecipientA},
			wantCount: 0, wantTotal: 0,
		},
		{
			name:      "another contract's allocations are not visible",
			filter:    VaultAllocationFilter{ContractID: testVaultTokenID},
			wantCount: 0, wantTotal: 0,
		},
		{
			name:      "first page of one",
			filter:    VaultAllocationFilter{ContractID: testVaultContractID},
			page:      VaultPage{Page: 1, Limit: 1},
			wantCount: 1, wantTotal: 3, wantFirst: testVaultAdmin,
		},
		{
			name:      "second page of one",
			filter:    VaultAllocationFilter{ContractID: testVaultContractID},
			page:      VaultPage{Page: 2, Limit: 1},
			wantCount: 1, wantTotal: 3, wantFirst: testRecipientB,
		},
		{
			name:      "a page past the end is empty, not an error",
			filter:    VaultAllocationFilter{ContractID: testVaultContractID},
			page:      VaultPage{Page: 99, Limit: 20},
			wantCount: 0, wantTotal: 3,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			allocations, err := m.ListAllocations(ctx, dbConnectionPool, tc.filter, tc.page)
			require.NoError(t, err)
			assert.Len(t, allocations, tc.wantCount)

			// The count must use the same filter as the page, or the total
			// will not match the rows.
			total, err := m.CountAllocations(ctx, dbConnectionPool, tc.filter)
			require.NoError(t, err)
			assert.Equal(t, tc.wantTotal, total)

			if tc.wantFirst != "" {
				require.NotEmpty(t, allocations)
				assert.Equal(t, tc.wantFirst, allocations[0].Recipient)
			}
		})
	}
}

func Test_VaultModel_ListAllocations_paginationIsStable(t *testing.T) {
	ctx, dbConnectionPool, m := newVaultTestDB(t)
	bootstrapVault(t, ctx, dbConnectionPool, m)

	// Every allocation in the same ledger, which is what a single transaction
	// registering several recipients produces. Without the recipient tiebreak
	// the page boundary would be non-deterministic, and rows would repeat or
	// vanish between pages.
	recipients := []string{testRecipientA, testRecipientB, testVaultAdmin}
	for i, recipient := range recipients {
		require.NoError(t, m.RegisterAllocation(ctx, dbConnectionPool, VaultAllocation{
			ContractID:       testVaultContractID,
			Recipient:        recipient,
			Amount:           NewVaultAmount(10_000_000),
			RegisteredLedger: 100,
			RegisteredAt:     time.Now().UTC(),
			LastEventID:      fmt.Sprintf("event-%d", i),
		}))
	}

	var paged []string
	for page := 1; page <= 3; page++ {
		allocations, err := m.ListAllocations(ctx, dbConnectionPool,
			VaultAllocationFilter{ContractID: testVaultContractID}, VaultPage{Page: page, Limit: 1})
		require.NoError(t, err)
		require.Len(t, allocations, 1)
		paged = append(paged, allocations[0].Recipient)
	}

	assert.ElementsMatch(t, recipients, paged, "paging must visit every row exactly once")
}

func Test_VaultModel_ListEvents_filtersAndPaginates(t *testing.T) {
	ctx, dbConnectionPool, m := newVaultTestDB(t)
	bootstrapVault(t, ctx, dbConnectionPool, m)

	amount := NewVaultAmount(10_000_000)
	events := []VaultEvent{
		newVaultEvent("funded", "0000000000000000001-0000000000", 101, testVaultAdmin, &amount),
		newVaultEvent("registered", "0000000000000000002-0000000000", 102, testRecipientA, &amount),
		newVaultEvent("claimed", "0000000000000000003-0000000000", 103, testRecipientA, &amount),
	}
	for _, event := range events {
		inserted, err := m.InsertEvent(ctx, dbConnectionPool, event)
		require.NoError(t, err)
		require.True(t, inserted)
	}

	testCases := []struct {
		name      string
		filter    VaultEventFilter
		page      VaultPage
		wantCount int
		wantTotal int
		wantFirst string
	}{
		{
			name:      "newest ledger first",
			filter:    VaultEventFilter{ContractID: testVaultContractID},
			wantCount: 3, wantTotal: 3, wantFirst: "claimed",
		},
		{
			name:      "by event type",
			filter:    VaultEventFilter{ContractID: testVaultContractID, EventType: "funded"},
			wantCount: 1, wantTotal: 1, wantFirst: "funded",
		},
		{
			name:      "by address, which spans event types",
			filter:    VaultEventFilter{ContractID: testVaultContractID, Address: testRecipientA},
			wantCount: 2, wantTotal: 2, wantFirst: "claimed",
		},
		{
			name:      "type and address together",
			filter:    VaultEventFilter{ContractID: testVaultContractID, EventType: "registered", Address: testRecipientA},
			wantCount: 1, wantTotal: 1, wantFirst: "registered",
		},
		{
			name:      "an address with no events",
			filter:    VaultEventFilter{ContractID: testVaultContractID, Address: testRecipientB},
			wantCount: 0, wantTotal: 0,
		},
		{
			name:      "paginated",
			filter:    VaultEventFilter{ContractID: testVaultContractID},
			page:      VaultPage{Page: 2, Limit: 2},
			wantCount: 1, wantTotal: 3, wantFirst: "funded",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := m.ListEvents(ctx, dbConnectionPool, tc.filter, tc.page)
			require.NoError(t, err)
			assert.Len(t, got, tc.wantCount)

			total, err := m.CountEvents(ctx, dbConnectionPool, tc.filter)
			require.NoError(t, err)
			assert.Equal(t, tc.wantTotal, total)

			if tc.wantFirst != "" {
				require.NotEmpty(t, got)
				assert.Equal(t, tc.wantFirst, got[0].EventType)
			}
		})
	}
}

func Test_VaultPage_normalise(t *testing.T) {
	// A bad page size is not worth failing a read over, but an unbounded one
	// turns into a table scan served to a browser. So it is clamped.
	testCases := []struct {
		name                            string
		page                            VaultPage
		wantPage, wantLimit, wantOffset int
	}{
		{name: "the zero request gets defaults", page: VaultPage{}, wantPage: 1, wantLimit: 20, wantOffset: 0},
		{name: "page 1", page: VaultPage{Page: 1, Limit: 20}, wantPage: 1, wantLimit: 20, wantOffset: 0},
		{name: "page 3 of 50", page: VaultPage{Page: 3, Limit: 50}, wantPage: 3, wantLimit: 50, wantOffset: 100},
		{name: "page 0 clamps up to 1", page: VaultPage{Page: 0, Limit: 20}, wantPage: 1, wantLimit: 20, wantOffset: 0},
		{name: "a negative page clamps up to 1", page: VaultPage{Page: -5, Limit: 20}, wantPage: 1, wantLimit: 20, wantOffset: 0},
		{name: "a negative limit falls back to the default", page: VaultPage{Page: 1, Limit: -1}, wantPage: 1, wantLimit: 20, wantOffset: 0},
		{name: "an oversized limit clamps to the maximum", page: VaultPage{Page: 1, Limit: 100000}, wantPage: 1, wantLimit: 100, wantOffset: 0},
		{name: "the maximum limit is allowed", page: VaultPage{Page: 2, Limit: 100}, wantPage: 2, wantLimit: 100, wantOffset: 100},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			page, limit, offset := tc.page.normalise()

			assert.Equal(t, tc.wantPage, page)
			assert.Equal(t, tc.wantLimit, limit)
			assert.Equal(t, tc.wantOffset, offset)

			// And the exported view must agree with the internal one.
			normalised := NormalisedVaultPage(tc.page)
			assert.Equal(t, tc.wantPage, normalised.Page)
			assert.Equal(t, tc.wantLimit, normalised.Limit)
		})
	}
}

func Test_CountPages(t *testing.T) {
	testCases := []struct {
		name  string
		total int
		page  VaultPage
		want  int
	}{
		{name: "nothing is zero pages, not one", total: 0, page: VaultPage{Limit: 20}, want: 0},
		{name: "a negative total is zero pages", total: -1, page: VaultPage{Limit: 20}, want: 0},
		{name: "one row", total: 1, page: VaultPage{Limit: 20}, want: 1},
		{name: "exactly one full page", total: 20, page: VaultPage{Limit: 20}, want: 1},
		{name: "one row into the second page", total: 21, page: VaultPage{Limit: 20}, want: 2},
		{name: "rounds up", total: 55, page: VaultPage{Limit: 20}, want: 3},
		{name: "uses the clamped limit, not the requested one", total: 300, page: VaultPage{Limit: 100000}, want: 3},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, CountPages(tc.total, tc.page))
		})
	}
}

func Test_VaultModel_GetLastEventAt(t *testing.T) {
	ctx, dbConnectionPool, m := newVaultTestDB(t)
	bootstrapVault(t, ctx, dbConnectionPool, m)

	t.Run("no events is nil, not an error", func(t *testing.T) {
		got, err := m.GetLastEventAt(ctx, dbConnectionPool, testVaultContractID)
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	at := func(eventType, id string, ledger int32, closedAt time.Time, contractID string) VaultEvent {
		amount := NewVaultAmount(1)
		e := newVaultEvent(eventType, id, ledger, testRecipientA, &amount)
		e.LedgerClosedAt = closedAt
		e.ContractID = contractID
		return e
	}

	// Inserted out of ledger order, so "newest" must come from the ledger,
	// not from insertion order.
	for _, e := range []VaultEvent{
		at("funded", "0000000000000000201-0000000000", 201, base.Add(1*time.Minute), testVaultContractID),
		at("claimed", "0000000000000000203-0000000000", 203, base.Add(3*time.Minute), testVaultContractID),
		at("registered", "0000000000000000202-0000000000", 202, base.Add(2*time.Minute), testVaultContractID),
		// Another contract's newer event must not leak in.
		at("funded", "0000000000000000999-0000000000", 999, base.Add(time.Hour), testVaultTokenID),
	} {
		inserted, err := m.InsertEvent(ctx, dbConnectionPool, e)
		require.NoError(t, err)
		require.True(t, inserted)
	}

	t.Run("newest event by ledger, scoped to the contract", func(t *testing.T) {
		got, err := m.GetLastEventAt(ctx, dbConnectionPool, testVaultContractID)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.True(t, base.Add(3*time.Minute).Equal(*got), "got %s", got)
	})
}
