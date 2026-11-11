package data

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stellar/stellar-disbursement-platform-backend/db"
)

// Tests for the summary snapshot (docs/VAULT_DESIGN.md §10): provenance, the
// bootstrap_ledger guard, missing-row reporting and the snapshot backoff.

const snapshotLedger int32 = 500

func snapshotState() VaultState {
	return VaultState{
		ContractID:     testVaultContractID,
		AdminAddress:   testVaultAdmin,
		TokenAddress:   testVaultTokenID,
		Deadline:       time.Now().Add(24 * time.Hour).UTC().Truncate(time.Microsecond),
		TotalAllocated: NewVaultAmount(30_000_000),
		TotalClaimed:   NewVaultAmount(5_000_000),
		Balance:        NewVaultAmount(100_000_000),
		SnapshotLedger: snapshotLedger,
	}
}

func bootstrapIndexerOnly(t *testing.T, ctx context.Context, sqlExec db.SQLExecuter, m VaultModel) {
	t.Helper()
	start := int32(101)
	_, err := m.BootstrapIndexerState(ctx, sqlExec, VaultIndexerState{
		ContractID:  testVaultContractID,
		LastCursor:  "",
		LastLedger:  100,
		Status:      VaultIndexerStatusHealthy,
		StartLedger: &start,
	})
	require.NoError(t, err)
}

func Test_VaultModel_BootstrapIndexerState_recordsTheStartLedger(t *testing.T) {
	ctx, pool, m := newVaultTestDB(t)
	bootstrapIndexerOnly(t, ctx, pool, m)

	state, err := m.GetIndexerState(ctx, pool, testVaultContractID)
	require.NoError(t, err)
	require.NotNil(t, state.StartLedger)
	assert.Equal(t, int32(101), *state.StartLedger)
	assert.Equal(t, 0, state.SnapshotAttempts)
	assert.Nil(t, state.SnapshotNextAttemptAt)
	assert.Nil(t, state.SnapshotLastError)
}

func Test_VaultModel_InsertSnapshotState(t *testing.T) {
	t.Run("creates a row marked as a snapshot at its ledger", func(t *testing.T) {
		ctx, pool, m := newVaultTestDB(t)

		inserted, err := m.InsertSnapshotState(ctx, pool, snapshotState())
		require.NoError(t, err)
		assert.True(t, inserted)

		state, err := m.GetState(ctx, pool, testVaultContractID)
		require.NoError(t, err)
		assert.Equal(t, VaultStateSourceSnapshot, state.Source)
		require.NotNil(t, state.BootstrapLedger)
		assert.Equal(t, snapshotLedger, *state.BootstrapLedger)
		assert.Equal(t, snapshotLedger, state.SnapshotLedger)
		assert.Equal(t, "100000000", state.Balance.String())
		assert.Equal(t, "30000000", state.TotalAllocated.String())
		assert.Equal(t, "5000000", state.TotalClaimed.String())
	})

	t.Run("is idempotent", func(t *testing.T) {
		ctx, pool, m := newVaultTestDB(t)

		_, err := m.InsertSnapshotState(ctx, pool, snapshotState())
		require.NoError(t, err)
		again := snapshotState()
		again.Balance = NewVaultAmount(1)
		again.SnapshotLedger = 900
		inserted, err := m.InsertSnapshotState(ctx, pool, again)
		require.NoError(t, err)
		assert.False(t, inserted)

		state, err := m.GetState(ctx, pool, testVaultContractID)
		require.NoError(t, err)
		assert.Equal(t, "100000000", state.Balance.String(), "a second snapshot must not overwrite the first")
		assert.Equal(t, snapshotLedger, *state.BootstrapLedger)
	})

	t.Run("never overwrites an event-derived row", func(t *testing.T) {
		ctx, pool, m := newVaultTestDB(t)
		seeded, err := m.SeedStateFromEvent(ctx, pool, VaultState{
			ContractID: testVaultContractID, AdminAddress: testVaultAdmin, TokenAddress: testVaultTokenID,
			Deadline: time.Now().Add(time.Hour).UTC(), SnapshotLedger: 100,
		})
		require.NoError(t, err)
		require.True(t, seeded)

		inserted, err := m.InsertSnapshotState(ctx, pool, snapshotState())
		require.NoError(t, err)
		assert.False(t, inserted)

		state, err := m.GetState(ctx, pool, testVaultContractID)
		require.NoError(t, err)
		assert.Equal(t, VaultStateSourceEvents, state.Source)
		assert.Nil(t, state.BootstrapLedger)
		assert.Equal(t, "0", state.Balance.String())
	})
}

func Test_VaultModel_SeedStateFromEvent_doesNotResetASnapshot(t *testing.T) {
	// A resync can replay `initialized` after a snapshot exists. Its totals
	// are zero; applying it would wipe out the real balances.
	ctx, pool, m := newVaultTestDB(t)
	_, err := m.InsertSnapshotState(ctx, pool, snapshotState())
	require.NoError(t, err)

	seeded, err := m.SeedStateFromEvent(ctx, pool, VaultState{
		ContractID: testVaultContractID, AdminAddress: testVaultAdmin, TokenAddress: testVaultTokenID,
		Deadline: time.Now().UTC(), SnapshotLedger: 10,
	})
	require.NoError(t, err)
	assert.False(t, seeded)

	state, err := m.GetState(ctx, pool, testVaultContractID)
	require.NoError(t, err)
	assert.Equal(t, VaultStateSourceSnapshot, state.Source)
	assert.Equal(t, "100000000", state.Balance.String())
}

// stateUpdates is every event update, applied with a fixed amount at a ledger.
var stateUpdates = []struct {
	name  string
	apply func(m VaultModel, ctx context.Context, sqlExec db.SQLExecuter, ledger int32) (StateUpdate, error)
	// check reports whether the update's effect is visible in state.
	check func(state *VaultState) bool
}{
	{
		name: "funded",
		apply: func(m VaultModel, ctx context.Context, sqlExec db.SQLExecuter, ledger int32) (StateUpdate, error) {
			return m.ApplyFundedBalance(ctx, sqlExec, testVaultContractID, NewVaultAmount(123), ledger)
		},
		check: func(s *VaultState) bool { return s.Balance.String() == "123" },
	},
	{
		name: "registered",
		apply: func(m VaultModel, ctx context.Context, sqlExec db.SQLExecuter, ledger int32) (StateUpdate, error) {
			return m.ApplyTotalAllocated(ctx, sqlExec, testVaultContractID, NewVaultAmount(123), ledger)
		},
		check: func(s *VaultState) bool { return s.TotalAllocated.String() == "123" },
	},
	{
		name: "claimed",
		apply: func(m VaultModel, ctx context.Context, sqlExec db.SQLExecuter, ledger int32) (StateUpdate, error) {
			return m.ApplyClaim(ctx, sqlExec, testVaultContractID, NewVaultAmount(1_000_000), ledger)
		},
		check: func(s *VaultState) bool { return s.TotalClaimed.String() == "6000000" },
	},
	{
		name: "cancelled",
		apply: func(m VaultModel, ctx context.Context, sqlExec db.SQLExecuter, ledger int32) (StateUpdate, error) {
			return m.ApplyCancellation(ctx, sqlExec, testVaultContractID, NewVaultAmount(1_000_000), ledger)
		},
		check: func(s *VaultState) bool { return s.TotalAllocated.String() == "29000000" },
	},
	{
		name: "withdrawn",
		apply: func(m VaultModel, ctx context.Context, sqlExec db.SQLExecuter, ledger int32) (StateUpdate, error) {
			return m.ApplyWithdrawal(ctx, sqlExec, testVaultContractID, NewVaultAmount(1_000_000), ledger)
		},
		check: func(s *VaultState) bool { return s.Balance.String() == "99000000" },
	},
}

func Test_VaultModel_stateUpdates_respectTheSnapshotLedger(t *testing.T) {
	for _, update := range stateUpdates {
		for _, tc := range []struct {
			ledger    int32
			wantApply bool
		}{
			{ledger: snapshotLedger - 1, wantApply: false},
			// Equal is excluded: the snapshot reads state after this ledger
			// closed, so its events are already in it.
			{ledger: snapshotLedger, wantApply: false},
			{ledger: snapshotLedger + 1, wantApply: true},
		} {
			t.Run(update.name, func(t *testing.T) {
				ctx, pool, m := newVaultTestDB(t)
				_, err := m.InsertSnapshotState(ctx, pool, snapshotState())
				require.NoError(t, err)

				result, err := update.apply(m, ctx, pool, tc.ledger)
				require.NoError(t, err)
				assert.True(t, result.RowExists)
				assert.Equal(t, tc.wantApply, result.Applied, "ledger %d vs snapshot %d", tc.ledger, snapshotLedger)

				state, err := m.GetState(ctx, pool, testVaultContractID)
				require.NoError(t, err)
				assert.Equal(t, tc.wantApply, update.check(state))
				require.NotNil(t, state.BootstrapLedger, "bootstrap_ledger is immutable")
				assert.Equal(t, snapshotLedger, *state.BootstrapLedger)
			})
		}
	}
}

func Test_VaultModel_stateUpdates_eventRowsAreUnaffectedByTheSnapshotGuard(t *testing.T) {
	// An event-derived row has no bootstrap_ledger, so deltas at a ledger
	// below snapshot_ledger still apply, exactly as before.
	for _, update := range stateUpdates[2:] {
		t.Run(update.name, func(t *testing.T) {
			ctx, pool, m := newVaultTestDB(t)
			seed := snapshotState()
			_, err := m.SeedStateFromEvent(ctx, pool, seed)
			require.NoError(t, err)

			result, err := update.apply(m, ctx, pool, snapshotLedger-100)
			require.NoError(t, err)
			assert.True(t, result.Applied)
		})
	}
}

func Test_VaultModel_stateUpdates_reportAMissingSummaryRow(t *testing.T) {
	for _, update := range stateUpdates {
		t.Run(update.name, func(t *testing.T) {
			ctx, pool, m := newVaultTestDB(t)

			result, err := update.apply(m, ctx, pool, 1000)
			require.NoError(t, err, "a missing row is reported, not an error")
			assert.Equal(t, StateUpdate{Applied: false, RowExists: false}, result)

			_, err = m.GetState(ctx, pool, testVaultContractID)
			assert.ErrorIs(t, err, ErrRecordNotFound, "the update must not create a row")
		})
	}
}

func Test_VaultModel_RecordSnapshotFailure(t *testing.T) {
	ctx, pool, m := newVaultTestDB(t)
	bootstrapIndexerOnly(t, ctx, pool, m)

	first := time.Now().Add(10 * time.Second).UTC().Truncate(time.Microsecond)
	require.NoError(t, m.RecordSnapshotFailure(ctx, pool, testVaultContractID, SnapshotErrorRPCUnreachable, first))
	second := time.Now().Add(20 * time.Second).UTC().Truncate(time.Microsecond)
	require.NoError(t, m.RecordSnapshotFailure(ctx, pool, testVaultContractID, SnapshotErrorInconsistentLedgers, second))

	state, err := m.GetIndexerState(ctx, pool, testVaultContractID)
	require.NoError(t, err)
	assert.Equal(t, 2, state.SnapshotAttempts)
	require.NotNil(t, state.SnapshotLastError)
	assert.Equal(t, string(SnapshotErrorInconsistentLedgers), *state.SnapshotLastError)
	require.NotNil(t, state.SnapshotNextAttemptAt)
	assert.True(t, second.Equal(*state.SnapshotNextAttemptAt))

	t.Run("raw error text is refused by the schema", func(t *testing.T) {
		err := m.RecordSnapshotFailure(ctx, pool, testVaultContractID,
			SnapshotErrorCategory("dial tcp https://rpc.example/?key=secret: refused"), second)
		assert.ErrorContains(t, err, "vault_indexer_state_snapshot_error_check")
	})

	t.Run("an unknown vault is an error", func(t *testing.T) {
		err := m.RecordSnapshotFailure(ctx, pool, "CUNKNOWN", SnapshotErrorRPCUnreachable, second)
		assert.ErrorIs(t, err, ErrRecordNotFound)
	})
}

func Test_VaultModel_MarkSnapshotBootstrapped(t *testing.T) {
	t.Run("marks degraded with an unknown gap start and clears the backoff", func(t *testing.T) {
		ctx, pool, m := newVaultTestDB(t)
		bootstrapIndexerOnly(t, ctx, pool, m)
		require.NoError(t, m.RecordSnapshotFailure(ctx, pool, testVaultContractID,
			SnapshotErrorRPCUnreachable, time.Now().UTC()))

		require.NoError(t, m.MarkSnapshotBootstrapped(ctx, pool, testVaultContractID, 100, "history before 101 missing"))

		state, err := m.GetIndexerState(ctx, pool, testVaultContractID)
		require.NoError(t, err)
		assert.Equal(t, VaultIndexerStatusDegraded, state.Status)
		assert.Nil(t, state.GapFromLedger, "the creation ledger is unknown; it must not be invented")
		require.NotNil(t, state.GapToLedger)
		assert.Equal(t, int32(100), *state.GapToLedger)
		require.NotNil(t, state.DegradedReason)
		assert.Equal(t, "history before 101 missing", *state.DegradedReason)
		assert.Equal(t, 0, state.SnapshotAttempts)
		assert.Nil(t, state.SnapshotLastError)
		assert.Nil(t, state.SnapshotNextAttemptAt)
	})

	t.Run("keeps a later existing gap end", func(t *testing.T) {
		ctx, pool, m := newVaultTestDB(t)
		bootstrapIndexerOnly(t, ctx, pool, m)
		require.NoError(t, m.MarkDegraded(ctx, pool, testVaultContractID, 200, 300, "retention gap"))

		require.NoError(t, m.MarkSnapshotBootstrapped(ctx, pool, testVaultContractID, 100, "snapshot"))

		state, err := m.GetIndexerState(ctx, pool, testVaultContractID)
		require.NoError(t, err)
		assert.Nil(t, state.GapFromLedger)
		assert.Equal(t, int32(300), *state.GapToLedger)
	})
}

func Test_VaultModel_MarkDegraded_keepsAnUnknownGapStart(t *testing.T) {
	ctx, pool, m := newVaultTestDB(t)
	bootstrapIndexerOnly(t, ctx, pool, m)
	require.NoError(t, m.MarkSnapshotBootstrapped(ctx, pool, testVaultContractID, 100, "snapshot"))

	// A later retention gap must not replace "unknown start" with a number,
	// which would claim the earlier history is not missing.
	require.NoError(t, m.MarkDegraded(ctx, pool, testVaultContractID, 400, 450, "retention gap"))

	state, err := m.GetIndexerState(ctx, pool, testVaultContractID)
	require.NoError(t, err)
	assert.Nil(t, state.GapFromLedger)
	assert.Equal(t, int32(450), *state.GapToLedger)
	assert.Equal(t, "retention gap", *state.DegradedReason)

	t.Run("a healthy vault still records both bounds", func(t *testing.T) {
		ctx, pool, m := newVaultTestDB(t)
		bootstrapIndexerOnly(t, ctx, pool, m)
		require.NoError(t, m.MarkDegraded(ctx, pool, testVaultContractID, 400, 450, "retention gap"))

		state, err := m.GetIndexerState(ctx, pool, testVaultContractID)
		require.NoError(t, err)
		require.NotNil(t, state.GapFromLedger)
		assert.Equal(t, int32(400), *state.GapFromLedger)
		assert.Equal(t, int32(450), *state.GapToLedger)
	})
}
