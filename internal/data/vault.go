package data

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/stellar/stellar-disbursement-platform-backend/db"
)

// Vault state lives in the tenant's own schema, applied by the sdp-migrations
// router. Tenant isolation is therefore structural: these queries carry no
// `WHERE tenant_id = ?` because there is no cross-tenant row to exclude — a
// connection routed to bluecorp's schema cannot see redcorp's vault at all.
//
// Everything here is a projection of vault_events except vault_events itself,
// which is the append-only source of truth. That is what makes a replay safe:
// drop the projections, replay the events, get the same state.

// VaultIndexerStatus is the health of one vault's event indexing.
type VaultIndexerStatus string

const (
	// VaultIndexerStatusHealthy means every event the vault has emitted since
	// bootstrap has been read.
	VaultIndexerStatusHealthy VaultIndexerStatus = "healthy"
	// VaultIndexerStatusDegraded means events were emitted that RPC can no
	// longer serve, so the recipient list may be incomplete. On-chain funds
	// are unaffected and any known recipient can still claim.
	VaultIndexerStatusDegraded VaultIndexerStatus = "degraded"
)

// VaultAllocationStatus mirrors the contract's AllocationStatus enum.
//
// The contract's lifecycle is strictly monotonic and one-shot per address:
// `register` fails with AllocationExists if an allocation already exists in
// *any* status, and claim and cancel both require Pending. So an address gets
// exactly one allocation ever, and only two transitions are reachable.
type VaultAllocationStatus string

const (
	VaultAllocationStatusPending   VaultAllocationStatus = "PENDING"
	VaultAllocationStatusClaimed   VaultAllocationStatus = "CLAIMED"
	VaultAllocationStatusCancelled VaultAllocationStatus = "CANCELLED"
)

// VaultIndexerState is the indexing position and health for one vault.
type VaultIndexerState struct {
	ContractID     string             `db:"contract_id" json:"-"`
	LastCursor     string             `db:"last_cursor" json:"-"`
	LastLedger     int32              `db:"last_ledger" json:"last_ledger"`
	Status         VaultIndexerStatus `db:"status" json:"status"`
	GapFromLedger  *int32             `db:"gap_from_ledger" json:"gap_from_ledger"`
	GapToLedger    *int32             `db:"gap_to_ledger" json:"gap_to_ledger"`
	DegradedReason *string            `db:"degraded_reason" json:"degraded_reason"`
	LastIndexedAt  *time.Time         `db:"last_indexed_at" json:"last_indexed_at"`
	BootstrappedAt time.Time          `db:"bootstrapped_at" json:"-"`
	CreatedAt      time.Time          `db:"created_at" json:"-"`
	UpdatedAt      time.Time          `db:"updated_at" json:"-"`
}

// VaultEvent is one row of the append-only event log.
type VaultEvent struct {
	ID             int64        `db:"id" json:"-"`
	EventID        string       `db:"event_id" json:"event_id"`
	ContractID     string       `db:"contract_id" json:"-"`
	EventType      string       `db:"event_type" json:"event_type"`
	Ledger         int32        `db:"ledger" json:"ledger"`
	LedgerClosedAt time.Time    `db:"ledger_closed_at" json:"ledger_closed_at"`
	TxHash         string       `db:"tx_hash" json:"tx_hash"`
	Address        *string      `db:"address" json:"address"`
	Amount         *VaultAmount `db:"amount" json:"amount"`
	Payload        []byte       `db:"payload" json:"-"`
	CreatedAt      time.Time    `db:"created_at" json:"-"`
}

// VaultAllocation is a recipient's current allocation.
type VaultAllocation struct {
	ContractID       string                `db:"contract_id" json:"-"`
	Recipient        string                `db:"recipient" json:"recipient"`
	Amount           VaultAmount           `db:"amount" json:"amount"`
	Status           VaultAllocationStatus `db:"status" json:"status"`
	RegisteredLedger int32                 `db:"registered_ledger" json:"registered_ledger"`
	RegisteredAt     time.Time             `db:"registered_at" json:"registered_at"`
	SettledLedger    *int32                `db:"settled_ledger" json:"settled_ledger"`
	SettledAt        *time.Time            `db:"settled_at" json:"settled_at"`
	LastEventID      string                `db:"last_event_id" json:"-"`
	CreatedAt        time.Time             `db:"created_at" json:"-"`
	UpdatedAt        time.Time             `db:"updated_at" json:"-"`
}

// VaultState is the cached contract-level snapshot behind the overview
// endpoint. It is always refreshable from the contract and never
// authoritative.
type VaultState struct {
	ContractID     string      `db:"contract_id" json:"contract_id"`
	AdminAddress   string      `db:"admin_address" json:"admin"`
	TokenAddress   string      `db:"token_address" json:"token"`
	Deadline       time.Time   `db:"deadline" json:"deadline"`
	TotalAllocated VaultAmount `db:"total_allocated" json:"total_allocated"`
	TotalClaimed   VaultAmount `db:"total_claimed" json:"total_claimed"`
	Balance        VaultAmount `db:"balance" json:"balance"`
	SnapshotLedger int32       `db:"snapshot_ledger" json:"snapshot_ledger"`
	RefreshedAt    time.Time   `db:"refreshed_at" json:"refreshed_at"`
	CreatedAt      time.Time   `db:"created_at" json:"-"`
}

// Available is balance - total_allocated: the most the admin could withdraw.
// It is computed rather than stored so it cannot drift from its inputs.
func (s VaultState) Available() VaultAmount {
	return s.Balance.Sub(s.TotalAllocated)
}

// IsExpired reports whether the claim deadline has passed. Claiming is then
// closed and cancelling is open; the contract's bounds are exclusive and
// inclusive respectively, so exactly one of the two is valid at any instant.
func (s VaultState) IsExpired(now time.Time) bool {
	return !now.Before(s.Deadline)
}

// VaultModel is the repository for all four vault tables. They are grouped
// into one model rather than four because the indexer writes across them in a
// single transaction, and splitting them would only spread that transaction
// over more objects without making any of it independently useful.
type VaultModel struct {
	dbConnectionPool db.DBConnectionPool
}

const vaultIndexerStateFields = `
	contract_id,
	last_cursor,
	last_ledger,
	status,
	gap_from_ledger,
	gap_to_ledger,
	degraded_reason,
	last_indexed_at,
	bootstrapped_at,
	created_at,
	updated_at
`

// GetIndexerState reads one vault's indexing position, returning
// ErrRecordNotFound when the vault has never been bootstrapped.
func (m VaultModel) GetIndexerState(ctx context.Context, sqlExec db.SQLExecuter, contractID string) (*VaultIndexerState, error) {
	const query = `SELECT ` + vaultIndexerStateFields + ` FROM vault_indexer_state WHERE contract_id = $1`

	var state VaultIndexerState
	if err := sqlExec.GetContext(ctx, &state, query, contractID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("vault indexer state for %s: %w", contractID, ErrRecordNotFound)
		}
		return nil, fmt.Errorf("getting vault indexer state: %w", err)
	}

	return &state, nil
}

// BootstrapIndexerState creates the indexing row for a vault the first time it
// is seen, and is a no-op if one already exists.
//
// ON CONFLICT DO NOTHING rather than an upsert: two schedulers racing on the
// same tenant must not reset a cursor that is already advancing. The existing
// row is then returned, so the caller resumes from wherever indexing actually
// is instead of from where it thought it should start.
func (m VaultModel) BootstrapIndexerState(ctx context.Context, sqlExec db.SQLExecuter, state VaultIndexerState) (*VaultIndexerState, error) {
	const query = `
		INSERT INTO vault_indexer_state
			(contract_id, last_cursor, last_ledger, status, gap_from_ledger, gap_to_ledger, degraded_reason)
		VALUES
			($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (contract_id) DO NOTHING
		RETURNING ` + vaultIndexerStateFields

	var inserted VaultIndexerState
	err := sqlExec.GetContext(ctx, &inserted, query,
		state.ContractID, state.LastCursor, state.LastLedger, state.Status,
		state.GapFromLedger, state.GapToLedger, state.DegradedReason)
	if err == nil {
		return &inserted, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("bootstrapping vault indexer state: %w", err)
	}

	// No rows returned means the row already existed. Read it back, so the
	// caller always gets the authoritative position.
	existing, err := m.GetIndexerState(ctx, sqlExec, state.ContractID)
	if err != nil {
		return nil, fmt.Errorf("reading existing vault indexer state: %w", err)
	}
	return existing, nil
}

// AdvanceCursor moves the indexing position forward.
//
// The caller must run this in the same transaction as the event inserts and
// projections it corresponds to. That is the whole correctness argument for
// the indexer: a crash mid-batch rolls back the cursor along with the
// projections, so the batch is simply re-fetched, and there is no window in
// which the cursor has moved past unapplied events.
func (m VaultModel) AdvanceCursor(ctx context.Context, sqlExec db.SQLExecuter, contractID, cursor string, ledger int32) error {
	const query = `
		UPDATE vault_indexer_state
		SET last_cursor = $1, last_ledger = $2, last_indexed_at = NOW(), updated_at = NOW()
		WHERE contract_id = $3`

	result, err := sqlExec.ExecContext(ctx, query, cursor, ledger, contractID)
	if err != nil {
		return fmt.Errorf("advancing vault cursor: %w", err)
	}
	return requireOneRow(result, "advancing vault cursor for "+contractID)
}

// MarkDegraded records that events were missed and can never be read back.
//
// The gap bounds are written together with the status because
// vault_indexer_state_gap_check rejects a degraded row without them: the
// schema will not allow a vault to be marked unhealthy without saying which
// ledgers were lost.
func (m VaultModel) MarkDegraded(ctx context.Context, sqlExec db.SQLExecuter, contractID string, gapFrom, gapTo int32, reason string) error {
	const query = `
		UPDATE vault_indexer_state
		SET status = $1,
		    gap_from_ledger = $2,
		    gap_to_ledger = $3,
		    degraded_reason = $4,
		    updated_at = NOW()
		WHERE contract_id = $5`

	result, err := sqlExec.ExecContext(ctx, query,
		VaultIndexerStatusDegraded, gapFrom, gapTo, reason, contractID)
	if err != nil {
		return fmt.Errorf("marking vault %s degraded: %w", contractID, err)
	}
	return requireOneRow(result, "marking vault "+contractID+" degraded")
}

// InsertEvent appends an event, reporting whether it was new.
//
// This is the idempotency gate. Polling is at-least-once — a timeout after RPC
// has served a batch, a retried job, or an operator resync all re-present
// events already applied — so the caller must project **only** when inserted
// is true. ON CONFLICT DO NOTHING combined with RETURNING gives exactly that
// signal: no row comes back when the event_id was already present.
func (m VaultModel) InsertEvent(ctx context.Context, sqlExec db.SQLExecuter, event VaultEvent) (inserted bool, err error) {
	const query = `
		INSERT INTO vault_events
			(event_id, contract_id, event_type, ledger, ledger_closed_at, tx_hash, address, amount, payload)
		VALUES
			($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (event_id) DO NOTHING
		RETURNING id`

	var id int64
	err = sqlExec.GetContext(ctx, &id, query,
		event.EventID, event.ContractID, event.EventType, event.Ledger,
		event.LedgerClosedAt, event.TxHash, event.Address, event.Amount, event.Payload)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inserting vault event %s: %w", event.EventID, err)
	}

	return true, nil
}

// RegisterAllocation projects a `registered` event.
//
// The insert is unconditional — no ON CONFLICT — because the contract rejects
// a second registration for an address with AllocationExists. A conflict here
// would mean either a replay (which InsertEvent already filtered out) or a
// contract whose behaviour has changed, and both deserve a loud failure rather
// than a quietly overwritten allocation.
func (m VaultModel) RegisterAllocation(ctx context.Context, sqlExec db.SQLExecuter, allocation VaultAllocation) error {
	const query = `
		INSERT INTO vault_allocations
			(contract_id, recipient, amount, status, registered_ledger, registered_at, last_event_id)
		VALUES
			($1, $2, $3, $4, $5, $6, $7)`

	_, err := sqlExec.ExecContext(ctx, query,
		allocation.ContractID, allocation.Recipient, allocation.Amount,
		VaultAllocationStatusPending, allocation.RegisteredLedger,
		allocation.RegisteredAt, allocation.LastEventID)
	if err != nil {
		return fmt.Errorf("registering vault allocation for %s: %w", allocation.Recipient, err)
	}

	return nil
}

// SettleAllocation projects a `claimed` or `cancelled` event, moving a
// PENDING allocation to its terminal state.
//
// The `status = 'PENDING'` predicate is the guard that makes the illegal
// transitions unreachable in SQL rather than only in application logic: a
// CLAIMED row can never be walked back to PENDING, and a CANCELLED one can
// never become CLAIMED. It reports whether a row moved, because zero rows is
// meaningful rather than an error — see the indexer, which treats it as a
// symptom of a retention gap (the `registered` event aged out before it was
// read) and not as a reason to fail the batch.
func (m VaultModel) SettleAllocation(
	ctx context.Context,
	sqlExec db.SQLExecuter,
	contractID, recipient string,
	status VaultAllocationStatus,
	settledLedger int32,
	settledAt time.Time,
	eventID string,
) (settled bool, err error) {
	if status == VaultAllocationStatusPending {
		return false, fmt.Errorf("settling an allocation requires a terminal status, got %s", status)
	}

	const query = `
		UPDATE vault_allocations
		SET status = $1,
		    settled_ledger = $2,
		    settled_at = $3,
		    last_event_id = $4,
		    updated_at = NOW()
		WHERE contract_id = $5 AND recipient = $6 AND status = $7`

	result, err := sqlExec.ExecContext(ctx, query,
		status, settledLedger, settledAt, eventID,
		contractID, recipient, VaultAllocationStatusPending)
	if err != nil {
		return false, fmt.Errorf("settling vault allocation for %s: %w", recipient, err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("counting settled vault allocations: %w", err)
	}

	return rows == 1, nil
}

const vaultStateFields = `
	contract_id,
	admin_address,
	token_address,
	deadline,
	total_allocated,
	total_claimed,
	balance,
	snapshot_ledger,
	refreshed_at,
	created_at
`

// UpsertState writes the cached contract snapshot.
//
// Unlike the allocation projections this is a plain upsert, because the row is
// a cache of values the contract holds authoritatively: re-reading it can only
// make it more correct, and there is no history to preserve.
func (m VaultModel) UpsertState(ctx context.Context, sqlExec db.SQLExecuter, state VaultState) error {
	const query = `
		INSERT INTO vault_state
			(contract_id, admin_address, token_address, deadline, total_allocated, total_claimed, balance, snapshot_ledger, refreshed_at)
		VALUES
			($1, $2, $3, $4, $5, $6, $7, $8, NOW())
		ON CONFLICT (contract_id) DO UPDATE SET
			admin_address   = EXCLUDED.admin_address,
			token_address   = EXCLUDED.token_address,
			deadline        = EXCLUDED.deadline,
			total_allocated = EXCLUDED.total_allocated,
			total_claimed   = EXCLUDED.total_claimed,
			balance         = EXCLUDED.balance,
			snapshot_ledger = EXCLUDED.snapshot_ledger,
			refreshed_at    = NOW()`

	_, err := sqlExec.ExecContext(ctx, query,
		state.ContractID, state.AdminAddress, state.TokenAddress, state.Deadline,
		state.TotalAllocated, state.TotalClaimed, state.Balance, state.SnapshotLedger)
	if err != nil {
		return fmt.Errorf("upserting vault state for %s: %w", state.ContractID, err)
	}

	return nil
}

// ApplyFundedBalance updates only the cached balance, from a `funded` or
// `withdrawn` event.
//
// The guard on snapshot_ledger is what keeps a replay from moving the cache
// backwards: an older event must not overwrite a newer snapshot. Without it,
// re-indexing from an old cursor would briefly publish a stale balance.
func (m VaultModel) ApplyFundedBalance(ctx context.Context, sqlExec db.SQLExecuter, contractID string, balance VaultAmount, ledger int32) error {
	const query = `
		UPDATE vault_state
		SET balance = $1, snapshot_ledger = $2, refreshed_at = NOW()
		WHERE contract_id = $3 AND snapshot_ledger <= $2`

	if _, err := sqlExec.ExecContext(ctx, query, balance, ledger, contractID); err != nil {
		return fmt.Errorf("applying vault balance for %s: %w", contractID, err)
	}

	return nil
}

// ApplyTotalAllocated updates the cached encumbrance from a `registered`
// event, under the same no-going-backwards guard as ApplyFundedBalance.
func (m VaultModel) ApplyTotalAllocated(ctx context.Context, sqlExec db.SQLExecuter, contractID string, totalAllocated VaultAmount, ledger int32) error {
	const query = `
		UPDATE vault_state
		SET total_allocated = $1, snapshot_ledger = $2, refreshed_at = NOW()
		WHERE contract_id = $3 AND snapshot_ledger <= $2`

	if _, err := sqlExec.ExecContext(ctx, query, totalAllocated, ledger, contractID); err != nil {
		return fmt.Errorf("applying vault total_allocated for %s: %w", contractID, err)
	}

	return nil
}

// GetState reads the cached snapshot, returning ErrRecordNotFound when the
// vault has not been indexed yet.
func (m VaultModel) GetState(ctx context.Context, sqlExec db.SQLExecuter, contractID string) (*VaultState, error) {
	const query = `SELECT ` + vaultStateFields + ` FROM vault_state WHERE contract_id = $1`

	var state VaultState
	if err := sqlExec.GetContext(ctx, &state, query, contractID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("vault state for %s: %w", contractID, ErrRecordNotFound)
		}
		return nil, fmt.Errorf("getting vault state: %w", err)
	}

	return &state, nil
}

// requireOneRow turns "the UPDATE matched nothing" into an explicit error.
// Used where a missing row means the vault was never bootstrapped, which is a
// caller bug rather than a state to tolerate.
func requireOneRow(result sql.Result, operation string) error {
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("counting rows affected while %s: %w", operation, err)
	}
	if rows != 1 {
		return fmt.Errorf("%s affected %d rows, expected 1: %w", operation, rows, ErrRecordNotFound)
	}
	return nil
}
