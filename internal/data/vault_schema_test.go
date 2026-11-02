package data

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stellar/stellar-disbursement-platform-backend/db"
	"github.com/stellar/stellar-disbursement-platform-backend/db/dbtest"
)

// i128::MAX in stroops. The reason vault amount columns are NUMERIC(40,0) and
// not BIGINT: this overflows int64 (~9.2e18) by 19 orders of magnitude.
const i128Max = "170141183460469231731687303715884105727"

func Test_VaultSchema_TablesExist(t *testing.T) {
	dbt := dbtest.Open(t)
	defer dbt.Close()

	ctx := context.Background()
	dbConnectionPool, err := db.OpenDBConnectionPool(dbt.DSN)
	require.NoError(t, err)
	defer dbConnectionPool.Close()

	for _, table := range []string{
		"vault_indexer_state",
		"vault_events",
		"vault_allocations",
		"vault_state",
	} {
		t.Run(table, func(t *testing.T) {
			var exists bool
			err := dbConnectionPool.GetContext(ctx, &exists,
				`SELECT EXISTS (
					SELECT 1 FROM information_schema.tables
					WHERE table_schema = current_schema() AND table_name = $1
				)`, table)
			require.NoError(t, err)
			assert.True(t, exists, "table %s should be created by the migration", table)
		})
	}
}

func Test_VaultSchema_AmountsHoldI128Max(t *testing.T) {
	dbt := dbtest.Open(t)
	defer dbt.Close()

	ctx := context.Background()
	dbConnectionPool, err := db.OpenDBConnectionPool(dbt.DSN)
	require.NoError(t, err)
	defer dbConnectionPool.Close()

	_, err = dbConnectionPool.ExecContext(ctx,
		`INSERT INTO vault_allocations
			(contract_id, recipient, amount, status, registered_ledger, registered_at, last_event_id)
		 VALUES ('C1', 'G1', `+i128Max+`, 'PENDING', 1, NOW(), 'e1')`)
	require.NoError(t, err, "NUMERIC(40,0) must hold i128::MAX without loss")

	var roundTripped string
	err = dbConnectionPool.GetContext(ctx, &roundTripped,
		`SELECT amount::text FROM vault_allocations WHERE contract_id = 'C1' AND recipient = 'G1'`)
	require.NoError(t, err)
	assert.Equal(t, i128Max, roundTripped, "amount must round-trip exactly, with no precision loss")
}

func Test_VaultSchema_AllocationConstraints(t *testing.T) {
	dbt := dbtest.Open(t)
	defer dbt.Close()

	ctx := context.Background()
	dbConnectionPool, err := db.OpenDBConnectionPool(dbt.DSN)
	require.NoError(t, err)
	defer dbConnectionPool.Close()

	testCases := []struct {
		name      string
		query     string
		wantError bool
	}{
		{
			name: "🟢 PENDING without settlement columns",
			query: `INSERT INTO vault_allocations
				(contract_id, recipient, amount, status, registered_ledger, registered_at, last_event_id)
				VALUES ('C1', 'G_pending', 100, 'PENDING', 1, NOW(), 'e1')`,
		},
		{
			name: "🟢 CLAIMED with settlement columns",
			query: `INSERT INTO vault_allocations
				(contract_id, recipient, amount, status, registered_ledger, registered_at, settled_ledger, settled_at, last_event_id)
				VALUES ('C1', 'G_claimed', 100, 'CLAIMED', 1, NOW(), 5, NOW(), 'e2')`,
		},
		{
			name: "🟢 CANCELLED with settlement columns",
			query: `INSERT INTO vault_allocations
				(contract_id, recipient, amount, status, registered_ledger, registered_at, settled_ledger, settled_at, last_event_id)
				VALUES ('C1', 'G_cancelled', 100, 'CANCELLED', 1, NOW(), 5, NOW(), 'e3')`,
		},
		{
			name: "🔴 PENDING must not carry settlement columns",
			query: `INSERT INTO vault_allocations
				(contract_id, recipient, amount, status, registered_ledger, registered_at, settled_ledger, settled_at, last_event_id)
				VALUES ('C1', 'G_bad1', 100, 'PENDING', 1, NOW(), 5, NOW(), 'e4')`,
			wantError: true,
		},
		{
			name: "🔴 CLAIMED must carry settlement columns",
			query: `INSERT INTO vault_allocations
				(contract_id, recipient, amount, status, registered_ledger, registered_at, last_event_id)
				VALUES ('C1', 'G_bad2', 100, 'CLAIMED', 1, NOW(), 'e5')`,
			wantError: true,
		},
		{
			name: "🔴 unknown status is rejected",
			query: `INSERT INTO vault_allocations
				(contract_id, recipient, amount, status, registered_ledger, registered_at, last_event_id)
				VALUES ('C1', 'G_bad3', 100, 'SETTLED', 1, NOW(), 'e6')`,
			wantError: true,
		},
		{
			name: "🔴 zero amount is rejected",
			query: `INSERT INTO vault_allocations
				(contract_id, recipient, amount, status, registered_ledger, registered_at, last_event_id)
				VALUES ('C1', 'G_bad4', 0, 'PENDING', 1, NOW(), 'e7')`,
			wantError: true,
		},
		{
			name: "🔴 negative amount is rejected",
			query: `INSERT INTO vault_allocations
				(contract_id, recipient, amount, status, registered_ledger, registered_at, last_event_id)
				VALUES ('C1', 'G_bad5', -1, 'PENDING', 1, NOW(), 'e8')`,
			wantError: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := dbConnectionPool.ExecContext(ctx, tc.query)
			if tc.wantError {
				assert.Error(t, err, "constraint should have rejected this row")
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func Test_VaultSchema_IndexerStateGapConstraint(t *testing.T) {
	dbt := dbtest.Open(t)
	defer dbt.Close()

	ctx := context.Background()
	dbConnectionPool, err := db.OpenDBConnectionPool(dbt.DSN)
	require.NoError(t, err)
	defer dbConnectionPool.Close()

	testCases := []struct {
		name      string
		query     string
		wantError bool
	}{
		{
			name: "🟢 healthy without gap bounds",
			query: `INSERT INTO vault_indexer_state (contract_id, last_cursor, last_ledger, status)
				VALUES ('C_ok1', '0-0', 10, 'healthy')`,
		},
		{
			name: "🟢 degraded with both gap bounds",
			query: `INSERT INTO vault_indexer_state
				(contract_id, last_cursor, last_ledger, status, gap_from_ledger, gap_to_ledger, degraded_reason)
				VALUES ('C_ok2', '0-0', 10, 'degraded', 10, 20, 'retention gap')`,
		},
		{
			name: "🔴 degraded without gap bounds",
			query: `INSERT INTO vault_indexer_state (contract_id, last_cursor, last_ledger, status)
				VALUES ('C_bad1', '0-0', 10, 'degraded')`,
			wantError: true,
		},
		{
			name: "🔴 degraded with only one gap bound",
			query: `INSERT INTO vault_indexer_state
				(contract_id, last_cursor, last_ledger, status, gap_from_ledger)
				VALUES ('C_bad2', '0-0', 10, 'degraded', 10)`,
			wantError: true,
		},
		{
			name: "🔴 healthy must not carry gap bounds",
			query: `INSERT INTO vault_indexer_state
				(contract_id, last_cursor, last_ledger, status, gap_from_ledger, gap_to_ledger)
				VALUES ('C_bad3', '0-0', 10, 'healthy', 10, 20)`,
			wantError: true,
		},
		{
			name: "🔴 unknown status is rejected",
			query: `INSERT INTO vault_indexer_state (contract_id, last_cursor, last_ledger, status)
				VALUES ('C_bad4', '0-0', 10, 'stalled')`,
			wantError: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := dbConnectionPool.ExecContext(ctx, tc.query)
			if tc.wantError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// The UNIQUE constraint on event_id is what makes replay safe: re-presenting an
// already-applied event must not insert a second row.
func Test_VaultSchema_EventIDIsIdempotencyKey(t *testing.T) {
	dbt := dbtest.Open(t)
	defer dbt.Close()

	ctx := context.Background()
	dbConnectionPool, err := db.OpenDBConnectionPool(dbt.DSN)
	require.NoError(t, err)
	defer dbConnectionPool.Close()

	insert := `INSERT INTO vault_events
		(event_id, contract_id, event_type, ledger, ledger_closed_at, tx_hash, payload)
		VALUES ('ev-1', 'C1', 'claimed', 1, NOW(), 'abc', '{}')`

	_, err = dbConnectionPool.ExecContext(ctx, insert)
	require.NoError(t, err)

	_, err = dbConnectionPool.ExecContext(ctx, insert)
	assert.Error(t, err, "a duplicate event_id must be rejected")

	// This is the form the indexer actually uses: a replayed event is a no-op
	// rather than an error.
	res, err := dbConnectionPool.ExecContext(ctx, insert+` ON CONFLICT (event_id) DO NOTHING`)
	require.NoError(t, err)
	rows, err := res.RowsAffected()
	require.NoError(t, err)
	assert.Zero(t, rows, "ON CONFLICT DO NOTHING must report no rows inserted on replay")

	var count int
	err = dbConnectionPool.GetContext(ctx, &count,
		`SELECT COUNT(*) FROM vault_events WHERE event_id = 'ev-1'`)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "exactly one row must exist after a replay")
}

func Test_VaultSchema_EventTypeConstraint(t *testing.T) {
	dbt := dbtest.Open(t)
	defer dbt.Close()

	ctx := context.Background()
	dbConnectionPool, err := db.OpenDBConnectionPool(dbt.DSN)
	require.NoError(t, err)
	defer dbConnectionPool.Close()

	// Every event the contract can emit must be accepted.
	for i, eventType := range []string{
		"initialized", "funded", "registered", "claimed", "cancelled", "withdrawn",
	} {
		t.Run("🟢 "+eventType, func(t *testing.T) {
			_, err := dbConnectionPool.ExecContext(ctx,
				`INSERT INTO vault_events
					(event_id, contract_id, event_type, ledger, ledger_closed_at, tx_hash, payload)
				 VALUES ($1, 'C1', $2, $3, NOW(), 'abc', '{}')`,
				"ok-"+eventType, eventType, i+1)
			assert.NoError(t, err)
		})
	}

	t.Run("🔴 unknown event type is rejected", func(t *testing.T) {
		_, err := dbConnectionPool.ExecContext(ctx,
			`INSERT INTO vault_events
				(event_id, contract_id, event_type, ledger, ledger_closed_at, tx_hash, payload)
			 VALUES ('bad-1', 'C1', 'upgraded', 1, NOW(), 'abc', '{}')`)
		assert.Error(t, err)
	})
}
