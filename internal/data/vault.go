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
	// StartLedger is the first ledger the indexer scanned for this vault. Nil
	// for vaults bootstrapped before it was recorded.
	StartLedger *int32 `db:"start_ledger" json:"-"`
	// Snapshot backoff (docs/VAULT_DESIGN.md §10.6). SnapshotLastError is a
	// SnapshotErrorCategory, never raw error text.
	SnapshotAttempts      int        `db:"snapshot_attempts" json:"-"`
	SnapshotNextAttemptAt *time.Time `db:"snapshot_next_attempt_at" json:"-"`
	SnapshotLastError     *string    `db:"snapshot_last_error" json:"-"`
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
	// Source says how the row was created; BootstrapLedger is set only for a
	// snapshot, and is the ledger its reads were taken at.
	Source          VaultStateSource `db:"source" json:"source"`
	BootstrapLedger *int32           `db:"bootstrap_ledger" json:"bootstrap_ledger"`
}

// VaultStateSource is where a vault_state row came from.
type VaultStateSource string

const (
	// VaultStateSourceEvents means the row was seeded by the `initialized`
	// event and every later event was applied to it.
	VaultStateSourceEvents VaultStateSource = "events"
	// VaultStateSourceSnapshot means the row was read from contract state by
	// simulation, because `initialized` was older than the RPC retention window.
	VaultStateSourceSnapshot VaultStateSource = "snapshot"
)

// StateUpdate reports what one event's update to vault_state did.
type StateUpdate struct {
	// Applied is true when the row was changed.
	Applied bool
	// RowExists is false when there is no summary row at all, so the update
	// could not apply. That is the symptom of a vault whose `initialized`
	// event was never indexed; the caller logs and counts it.
	RowExists bool
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
	updated_at,
	start_ledger,
	snapshot_attempts,
	snapshot_next_attempt_at,
	snapshot_last_error
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
			(contract_id, last_cursor, last_ledger, status, gap_from_ledger, gap_to_ledger, degraded_reason, start_ledger)
		VALUES
			($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (contract_id) DO NOTHING
		RETURNING ` + vaultIndexerStateFields

	var inserted VaultIndexerState
	err := sqlExec.GetContext(ctx, &inserted, query,
		state.ContractID, state.LastCursor, state.LastLedger, state.Status,
		state.GapFromLedger, state.GapToLedger, state.DegradedReason, state.StartLedger)
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
//
// An already-recorded *unknown* gap start (NULL, from a summary snapshot) is
// kept, and the gap end only moves forward: the earlier history is still
// missing, and overwriting NULL with a number would claim it is not.
func (m VaultModel) MarkDegraded(ctx context.Context, sqlExec db.SQLExecuter, contractID string, gapFrom, gapTo int32, reason string) error {
	const query = `
		UPDATE vault_indexer_state
		SET gap_from_ledger = CASE
		        WHEN status = $1 AND gap_from_ledger IS NULL THEN NULL
		        ELSE $2::integer
		    END,
		    gap_to_ledger = CASE
		        WHEN status = $1 AND gap_from_ledger IS NULL THEN GREATEST(gap_to_ledger, $3::integer)
		        ELSE $3::integer
		    END,
		    status = $1,
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
	created_at,
	source,
	bootstrap_ledger
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

// SeedStateFromEvent creates the summary from the `initialized` event.
//
// Insert-only: `initialize` runs once per contract and precedes every other
// event, so an existing row is always at least as new as this event. In
// practice that row is a snapshot taken before a resync replayed the event,
// and overwriting it would reset its totals to zero.
func (m VaultModel) SeedStateFromEvent(ctx context.Context, sqlExec db.SQLExecuter, state VaultState) (inserted bool, err error) {
	const query = `
		INSERT INTO vault_state
			(contract_id, admin_address, token_address, deadline, total_allocated, total_claimed, balance, snapshot_ledger, refreshed_at, source)
		VALUES
			($1, $2, $3, $4, $5, $6, $7, $8, NOW(), 'events')
		ON CONFLICT (contract_id) DO NOTHING`

	result, err := sqlExec.ExecContext(ctx, query,
		state.ContractID, state.AdminAddress, state.TokenAddress, state.Deadline,
		state.TotalAllocated, state.TotalClaimed, state.Balance, state.SnapshotLedger)
	if err != nil {
		return false, fmt.Errorf("seeding vault state for %s: %w", state.ContractID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("counting rows seeded for %s: %w", state.ContractID, err)
	}
	return rows == 1, nil
}

// InsertSnapshotState creates the summary from contract state read at
// state.SnapshotLedger (docs/VAULT_DESIGN.md §10).
//
// ON CONFLICT DO NOTHING: a snapshot only ever fills an absent row. If events
// created the row first, they win and the snapshot is discarded.
func (m VaultModel) InsertSnapshotState(ctx context.Context, sqlExec db.SQLExecuter, state VaultState) (inserted bool, err error) {
	const query = `
		INSERT INTO vault_state
			(contract_id, admin_address, token_address, deadline, total_allocated, total_claimed, balance,
			 snapshot_ledger, refreshed_at, source, bootstrap_ledger)
		VALUES
			($1, $2, $3, $4, $5, $6, $7, $8, NOW(), 'snapshot', $8)
		ON CONFLICT (contract_id) DO NOTHING`

	result, err := sqlExec.ExecContext(ctx, query,
		state.ContractID, state.AdminAddress, state.TokenAddress, state.Deadline,
		state.TotalAllocated, state.TotalClaimed, state.Balance, state.SnapshotLedger)
	if err != nil {
		return false, fmt.Errorf("inserting vault snapshot for %s: %w", state.ContractID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("counting rows inserted for %s: %w", state.ContractID, err)
	}
	return rows == 1, nil
}

// applyStateUpdate runs one event's UPDATE of vault_state and reports whether
// it applied and whether a summary row existed at all.
//
// Every event update shares one guard, the snapshot guard: a row read from
// contract state at bootstrap_ledger already reflects every event at or before
// that ledger, so those events must not be applied again. It is strictly
// greater-than because simulation reads state *after* that ledger closed.
// Event-derived rows have bootstrap_ledger NULL and are unaffected.
//
// The CTE's second SELECT sees the table as of the start of the statement,
// which is exactly the question asked: was there a row to update?
func applyStateUpdate(
	ctx context.Context, sqlExec db.SQLExecuter, operation, contractID string, ledger int32,
	set, extraGuard string, args ...any,
) (StateUpdate, error) {
	query := `
		WITH updated AS (
			UPDATE vault_state
			SET ` + set + `, refreshed_at = NOW()
			WHERE contract_id = $1
			  AND (bootstrap_ledger IS NULL OR $2 > bootstrap_ledger)` + extraGuard + `
			RETURNING 1
		)
		SELECT
			EXISTS (SELECT 1 FROM updated) AS applied,
			EXISTS (SELECT 1 FROM vault_state WHERE contract_id = $1) AS row_exists`

	var outcome struct {
		Applied   bool `db:"applied"`
		RowExists bool `db:"row_exists"`
	}
	params := append([]any{contractID, ledger}, args...)
	if err := sqlExec.GetContext(ctx, &outcome, query, params...); err != nil {
		return StateUpdate{}, fmt.Errorf("%s for %s: %w", operation, contractID, err)
	}

	return StateUpdate{Applied: outcome.Applied, RowExists: outcome.RowExists}, nil
}

// ApplyFundedBalance updates only the cached balance, from a `funded` event.
//
// The guard on snapshot_ledger is what keeps a replay from moving the cache
// backwards: an older event must not overwrite a newer snapshot. Without it,
// re-indexing from an old cursor would briefly publish a stale balance.
func (m VaultModel) ApplyFundedBalance(ctx context.Context, sqlExec db.SQLExecuter, contractID string, balance VaultAmount, ledger int32) (StateUpdate, error) {
	return applyStateUpdate(ctx, sqlExec, "applying vault balance", contractID, ledger,
		`balance = $3, snapshot_ledger = $2`, ` AND snapshot_ledger <= $2`, balance)
}

// ApplyTotalAllocated updates the cached encumbrance from a `registered`
// event, under the same no-going-backwards guard as ApplyFundedBalance.
func (m VaultModel) ApplyTotalAllocated(ctx context.Context, sqlExec db.SQLExecuter, contractID string, totalAllocated VaultAmount, ledger int32) (StateUpdate, error) {
	return applyStateUpdate(ctx, sqlExec, "applying vault total_allocated", contractID, ledger,
		`total_allocated = $3, snapshot_ledger = $2`, ` AND snapshot_ledger <= $2`, totalAllocated)
}

// The three methods below apply a *delta* rather than an absolute value,
// because the contract's claimed, cancelled and withdrawn events carry only an
// amount — unlike funded and registered, which publish the resulting balance
// and encumbrance. Deriving is exact here (a claim moves precisely `amount`)
// and is safe only because InsertEvent gates each one to at most once per
// event_id; applying a delta twice would silently corrupt the totals. For a
// snapshot row, the bootstrap_ledger guard in applyStateUpdate is the other
// half of that: a delta already inside the snapshot is not applied at all.
//
// Each decrement is floored at zero with GREATEST. That is not defensive
// noise: vault_state_totals_check rejects a negative total, so after a
// retention gap — a `claimed` event whose `registered` event aged out — an
// unclamped decrement would fail the batch forever and wedge the indexer over
// data the network no longer has. Clamping keeps indexing alive; the vault is
// already flagged degraded, and the indexer counts the anomaly, so the
// inconsistency is reported rather than hidden.
//
// snapshot_ledger moves with GREATEST too, so a replay of older events cannot
// drag it backwards.

// ApplyClaim moves an amount from allocated to claimed, and out of the
// balance, from a `claimed` event.
func (m VaultModel) ApplyClaim(ctx context.Context, sqlExec db.SQLExecuter, contractID string, amount VaultAmount, ledger int32) (StateUpdate, error) {
	return applyStateUpdate(ctx, sqlExec, "applying vault claim", contractID, ledger,
		`total_claimed   = total_claimed + $3,
		    total_allocated = GREATEST(0, total_allocated - $3),
		    balance         = GREATEST(0, balance - $3),
		    snapshot_ledger = GREATEST(snapshot_ledger, $2)`, "", amount)
}

// ApplyCancellation releases an encumbrance from a `cancelled` event. No
// tokens move, so the balance is untouched.
func (m VaultModel) ApplyCancellation(ctx context.Context, sqlExec db.SQLExecuter, contractID string, amount VaultAmount, ledger int32) (StateUpdate, error) {
	return applyStateUpdate(ctx, sqlExec, "applying vault cancellation", contractID, ledger,
		`total_allocated = GREATEST(0, total_allocated - $3),
		    snapshot_ledger = GREATEST(snapshot_ledger, $2)`, "", amount)
}

// ApplyWithdrawal reduces the balance from a `withdrawn` event. The contract
// caps a withdrawal at balance - total_allocated, so this can never consume
// tokens owed to a pending allocation.
func (m VaultModel) ApplyWithdrawal(ctx context.Context, sqlExec db.SQLExecuter, contractID string, amount VaultAmount, ledger int32) (StateUpdate, error) {
	return applyStateUpdate(ctx, sqlExec, "applying vault withdrawal", contractID, ledger,
		`balance         = GREATEST(0, balance - $3),
		    snapshot_ledger = GREATEST(snapshot_ledger, $2)`, "", amount)
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

// SnapshotErrorCategory is the stored reason a summary snapshot failed. It is
// a closed set, enforced by vault_indexer_state_snapshot_error_check, so raw
// RPC error text (which can carry a provider URL or key) is never persisted.
type SnapshotErrorCategory string

const (
	SnapshotErrorRPCUnreachable         SnapshotErrorCategory = "rpc_unreachable"
	SnapshotErrorContractNotInitialized SnapshotErrorCategory = "contract_not_initialized"
	SnapshotErrorInconsistentLedgers    SnapshotErrorCategory = "inconsistent_ledgers"
	SnapshotErrorSimulationFailed       SnapshotErrorCategory = "simulation_failed"
	SnapshotErrorInvalidResponse        SnapshotErrorCategory = "invalid_response"
)

// RecordSnapshotFailure counts a failed summary snapshot and schedules the
// next attempt.
func (m VaultModel) RecordSnapshotFailure(
	ctx context.Context, sqlExec db.SQLExecuter, contractID string, category SnapshotErrorCategory, nextAttemptAt time.Time,
) error {
	const query = `
		UPDATE vault_indexer_state
		SET snapshot_attempts = snapshot_attempts + 1,
		    snapshot_last_error = $1,
		    snapshot_next_attempt_at = $2,
		    updated_at = NOW()
		WHERE contract_id = $3`

	result, err := sqlExec.ExecContext(ctx, query, category, nextAttemptAt, contractID)
	if err != nil {
		return fmt.Errorf("recording a vault snapshot failure for %s: %w", contractID, err)
	}
	return requireOneRow(result, "recording a vault snapshot failure for "+contractID)
}

// MarkSnapshotBootstrapped records a successful summary snapshot: it clears
// the backoff and marks the vault degraded, because a snapshot means the
// vault's history before gapTo+1 was never indexed and recipients registered
// then are unknown.
//
// The gap start is NULL: the contract's creation ledger cannot be read from
// RPC. An existing gap end is kept if it is later, so an earlier retention gap
// is not shrunk.
func (m VaultModel) MarkSnapshotBootstrapped(ctx context.Context, sqlExec db.SQLExecuter, contractID string, gapTo int32, reason string) error {
	const query = `
		UPDATE vault_indexer_state
		SET status = $1,
		    gap_from_ledger = NULL,
		    gap_to_ledger = GREATEST(COALESCE(gap_to_ledger, $2), $2),
		    degraded_reason = $3,
		    snapshot_attempts = 0,
		    snapshot_last_error = NULL,
		    snapshot_next_attempt_at = NULL,
		    updated_at = NOW()
		WHERE contract_id = $4`

	result, err := sqlExec.ExecContext(ctx, query, VaultIndexerStatusDegraded, gapTo, reason, contractID)
	if err != nil {
		return fmt.Errorf("marking vault %s snapshot-bootstrapped: %w", contractID, err)
	}
	return requireOneRow(result, "marking vault "+contractID+" snapshot-bootstrapped")
}
