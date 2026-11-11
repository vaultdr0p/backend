package db

import (
	"context"
	"database/sql"
	"io/fs"
	"testing"

	migrate "github.com/rubenv/sql-migrate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stellar/stellar-disbursement-platform-backend/db/dbtest"
	"github.com/stellar/stellar-disbursement-platform-backend/db/migrations"
)

// The vault summary bootstrap migration must be reversible with data in it:
// a degraded vault whose gap start is unknown (NULL) cannot satisfy the old
// constraint, so the down migration widens it to 0 rather than failing.
func TestMigrate_vaultSummaryBootstrap_downWithData(t *testing.T) {
	dbt := dbtest.OpenWithoutMigrations(t)
	defer dbt.Close()
	pool, err := OpenDBConnectionPool(dbt.DSN)
	require.NoError(t, err)
	defer pool.Close()
	ctx := context.Background()

	var count int
	require.NoError(t, fs.WalkDir(migrations.SDPMigrationRouter.FS, ".", func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			count++
		}
		return err
	}))
	_, err = Migrate(dbt.DSN, migrate.Up, count, migrations.SDPMigrationRouter)
	require.NoError(t, err)

	var latest string
	require.NoError(t, pool.GetContext(ctx, &latest,
		`SELECT id FROM `+migrations.SDPMigrationRouter.TableName+` ORDER BY id DESC LIMIT 1`))
	require.Equal(t, "2026-11-11.0-vault-summary-bootstrap.sql", latest,
		"this test reverts exactly one migration, which must be the summary bootstrap")

	_, err = pool.ExecContext(ctx, `
		INSERT INTO vault_indexer_state
			(contract_id, last_cursor, last_ledger, status, gap_to_ledger, degraded_reason, start_ledger, snapshot_attempts)
		VALUES ('C_down', '', 99, 'degraded', 99, 'snapshot', 100, 2)`)
	require.NoError(t, err)
	_, err = pool.ExecContext(ctx, `
		INSERT INTO vault_state
			(contract_id, admin_address, token_address, deadline, balance, snapshot_ledger, source, bootstrap_ledger)
		VALUES ('C_down', 'G', 'C', NOW(), 42, 500, 'snapshot', 500)`)
	require.NoError(t, err)

	n, err := Migrate(dbt.DSN, migrate.Down, 1, migrations.SDPMigrationRouter)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	var gap struct {
		From sql.NullInt32 `db:"gap_from_ledger"`
		To   sql.NullInt32 `db:"gap_to_ledger"`
	}
	require.NoError(t, pool.GetContext(ctx, &gap,
		`SELECT gap_from_ledger, gap_to_ledger FROM vault_indexer_state WHERE contract_id = 'C_down'`))
	assert.Equal(t, sql.NullInt32{Int32: 0, Valid: true}, gap.From, "unknown start widened to 0, a superset of the gap")
	assert.Equal(t, sql.NullInt32{Int32: 99, Valid: true}, gap.To)

	var balance string
	require.NoError(t, pool.GetContext(ctx, &balance, `SELECT balance::text FROM vault_state WHERE contract_id = 'C_down'`))
	assert.Equal(t, "42", balance, "snapshot rows hold real contract values and survive the rollback")

	var provenanceColumns int
	require.NoError(t, pool.GetContext(ctx, &provenanceColumns, `
		SELECT COUNT(*) FROM information_schema.columns
		WHERE table_name IN ('vault_state', 'vault_indexer_state')
		  AND column_name IN ('source', 'bootstrap_ledger', 'start_ledger', 'snapshot_attempts',
		                      'snapshot_next_attempt_at', 'snapshot_last_error')`))
	assert.Zero(t, provenanceColumns)

	// The old constraint is back: a degraded row without a gap start is refused.
	_, err = pool.ExecContext(ctx, `
		INSERT INTO vault_indexer_state (contract_id, last_cursor, last_ledger, status, gap_to_ledger)
		VALUES ('C_old', '', 1, 'degraded', 1)`)
	assert.ErrorContains(t, err, "vault_indexer_state_gap_check")

	// And it applies cleanly again on top of the rolled-back data.
	n, err = Migrate(dbt.DSN, migrate.Up, 1, migrations.SDPMigrationRouter)
	require.NoError(t, err)
	require.Equal(t, 1, n)
}
