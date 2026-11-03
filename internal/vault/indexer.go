package vault

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/stellar/go-stellar-sdk/support/log"

	"github.com/stellar/stellar-disbursement-platform-backend/db"
	"github.com/stellar/stellar-disbursement-platform-backend/internal/data"
	"github.com/stellar/stellar-disbursement-platform-backend/internal/stellar"

	protocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
)

const (
	// defaultBatchLimit is how many events one getEvents call asks for. RPC
	// caps this server-side; 200 keeps a single response small enough to apply
	// inside one transaction without a long-held write lock.
	defaultBatchLimit uint = 200

	// maxBatchesPerRun bounds the work one tick does while still letting a
	// bootstrap replay catch up quickly. Each batch commits on its own, so
	// progress survives a crash mid-run, and a tenant with a long backlog
	// cannot monopolise the scheduler.
	maxBatchesPerRun = 20

	// windowProbeMargin backs the window probe off the latest ledger.
	//
	// GetLatestLedgerSequence reports Core's ledger, but RPC's *event* index
	// can be a ledger or two behind it, and asking for events at a ledger the
	// index has not reached yet is rejected as out of range. Backing off ~10
	// minutes is far inside the ~120,960-ledger retention window, so it costs
	// nothing and removes the race.
	windowProbeMargin uint32 = 120
)

// RPCClient is the slice of internal/stellar.RPCClient the indexer uses.
// Declaring it here rather than taking the full interface keeps the indexer's
// mock small and makes its RPC surface explicit: two reads, no writes.
type RPCClient interface {
	GetEvents(ctx context.Context, request protocol.GetEventsRequest) (*protocol.GetEventsResponse, error)
	GetLatestLedgerSequence(ctx context.Context) (uint32, error)
}

// Indexer reads a vault contract's events into the current tenant's schema.
//
// It holds no tenant identity of its own. The scheduler puts the tenant in
// context and the models' connection pool is routed to that tenant's schema,
// so every read and write below — including the cursor — is per-tenant by
// construction rather than by a filter that could be forgotten.
type Indexer struct {
	rpcClient       RPCClient
	models          *data.Models
	contractID      string
	indexFromLedger uint32
	batchLimit      uint
}

// NewIndexer builds an indexer for one vault contract.
func NewIndexer(rpcClient RPCClient, models *data.Models, contractID string, indexFromLedger uint32) (*Indexer, error) {
	if rpcClient == nil {
		return nil, errors.New("rpcClient is required for NewIndexer")
	}
	if models == nil {
		return nil, errors.New("models is required for NewIndexer")
	}
	if contractID == "" {
		return nil, errors.New("contractID is required for NewIndexer")
	}

	return &Indexer{
		rpcClient:       rpcClient,
		models:          models,
		contractID:      contractID,
		indexFromLedger: indexFromLedger,
		batchLimit:      defaultBatchLimit,
	}, nil
}

// Result reports what one run did, for logging and metrics.
type Result struct {
	// EventsRead is how many events RPC returned.
	EventsRead int
	// EventsApplied is how many were new and therefore projected. The
	// difference from EventsRead is replayed events, which is the normal
	// steady state after a retry, not an error.
	EventsApplied int
	// EventsSkipped counts events whose type this build does not know.
	EventsSkipped int
	// Anomalies counts projections that found no row to update — the symptom
	// of a retention gap, where a settling event's `registered` event aged out
	// before it could be read.
	Anomalies int
	// LastLedger is how far indexing has now scanned.
	LastLedger int32
	// LatestLedger is the chain tip RPC reported.
	LatestLedger uint32
	// Degraded is true when this vault has a known, unrecoverable gap.
	Degraded bool
}

// LagLedgers is how far behind the chain tip indexing is. This is the value
// the lag metric and its alert are built on.
func (r Result) LagLedgers() uint32 {
	if r.LastLedger < 0 || uint32(r.LastLedger) >= r.LatestLedger {
		return 0
	}
	return r.LatestLedger - uint32(r.LastLedger)
}

// ledgerWindow is the range of ledgers RPC can currently serve events for.
type ledgerWindow struct {
	Oldest uint32
	Latest uint32
}

// Run indexes everything currently available, in batches.
//
// The order of operations is the whole correctness argument:
//
//  1. Establish the retention window *before* fetching, so an out-of-window
//     resume point is detected and recorded rather than retried. RPC rejects a
//     start ledger below oldestLedger, and a naive implementation would fail,
//     leave the cursor untouched, and fail identically on every subsequent
//     tick — a retry loop over data that is gone for good.
//  2. Fetch a batch from the cursor.
//  3. Apply the batch and advance the cursor in **one** transaction.
func (idx *Indexer) Run(ctx context.Context) (*Result, error) {
	window, err := idx.describeWindow(ctx)
	if err != nil {
		return nil, fmt.Errorf("describing the RPC event window: %w", err)
	}

	state, err := idx.resumePoint(ctx, window)
	if err != nil {
		return nil, fmt.Errorf("establishing the resume point: %w", err)
	}

	result := Result{
		LastLedger:   state.LastLedger,
		LatestLedger: window.Latest,
		Degraded:     state.Status == data.VaultIndexerStatusDegraded,
	}

	for batch := 0; batch < maxBatchesPerRun; batch++ {
		applied, err := idx.runBatch(ctx, state, &result)
		if err != nil {
			return nil, err
		}
		if !applied {
			break
		}

		// Re-read the position for the next batch, so each iteration resumes
		// from what was actually committed rather than from an in-memory
		// guess.
		state, err = idx.models.Vault.GetIndexerState(ctx, idx.models.DBConnectionPool, idx.contractID)
		if err != nil {
			return nil, fmt.Errorf("re-reading the vault cursor between batches: %w", err)
		}
		result.LastLedger = state.LastLedger
	}

	return &result, nil
}

// describeWindow asks RPC which ledgers it can still serve events for.
//
// It probes at `latest - windowProbeMargin` rather than parsing the
// out-of-range error from a speculative fetch. That keeps the gap decision
// deterministic: the window is known before any resume point is used, so there
// is no path on which an aged-out cursor turns into a repeating failure.
func (idx *Indexer) describeWindow(ctx context.Context) (ledgerWindow, error) {
	latest, err := idx.rpcClient.GetLatestLedgerSequence(ctx)
	if err != nil {
		return ledgerWindow{}, fmt.Errorf("getting the latest ledger: %w", err)
	}

	probeFrom := uint32(1)
	if latest > windowProbeMargin {
		probeFrom = latest - windowProbeMargin
	}

	resp, err := idx.rpcClient.GetEvents(ctx, protocol.GetEventsRequest{
		StartLedger: probeFrom,
		Filters:     idx.filters(),
		Pagination:  &protocol.PaginationOptions{Limit: 1},
		Format:      protocol.FormatJSON,
	})
	if err != nil {
		return ledgerWindow{}, fmt.Errorf("probing the event window at ledger %d: %w", probeFrom, err)
	}
	if resp.OldestLedger == 0 {
		return ledgerWindow{}, fmt.Errorf("RPC reported no oldest ledger, so the retention window is unknown")
	}

	return ledgerWindow{Oldest: resp.OldestLedger, Latest: max(resp.LatestLedger, latest)}, nil
}

// filters is the event filter for every request: this contract, contract
// events only.
//
// Filtering on the event type is what keeps failed invocations out. Contract
// events are emitted only by successful calls — a failure surfaces as a
// `diagnostic` event, which is never requested.
func (idx *Indexer) filters() []protocol.EventFilter {
	return []protocol.EventFilter{{
		EventType:   protocol.EventTypeSet{protocol.EventTypeContract: nil},
		ContractIDs: []string{idx.contractID},
	}}
}

// resumePoint returns the indexing state to resume from, bootstrapping the
// vault on first sight and marking it degraded if its resume point has aged
// out of the retention window.
func (idx *Indexer) resumePoint(ctx context.Context, window ledgerWindow) (*data.VaultIndexerState, error) {
	state, err := idx.models.Vault.GetIndexerState(ctx, idx.models.DBConnectionPool, idx.contractID)
	if err != nil && !errors.Is(err, data.ErrRecordNotFound) {
		return nil, fmt.Errorf("reading the vault indexer state: %w", err)
	}
	if errors.Is(err, data.ErrRecordNotFound) {
		return idx.bootstrap(ctx, window)
	}

	// The resume point is the first ledger not yet scanned.
	resumeFrom := uint32(state.LastLedger) + 1
	if resumeFrom >= window.Oldest {
		return state, nil
	}

	// Events were emitted in [resumeFrom, window.Oldest-1] that RPC can no
	// longer serve. Since the event log is the only way to discover
	// recipients, the identity of anyone registered in that span is
	// permanently unrecoverable — no retry can fix it, so the vault is marked
	// degraded and indexing resumes at the window edge.
	gapTo := window.Oldest - 1
	reason := fmt.Sprintf(
		"events in ledgers %d-%d fell outside the RPC retention window before they were indexed; "+
			"recipients registered in that range may be missing. On-chain funds are unaffected.",
		resumeFrom, gapTo)

	log.Ctx(ctx).Warnf("vault %s indexer degraded: %s", idx.contractID, reason)

	if err := idx.models.Vault.MarkDegraded(ctx, idx.models.DBConnectionPool,
		idx.contractID, int32(resumeFrom), int32(gapTo), reason); err != nil {
		return nil, fmt.Errorf("marking the vault degraded: %w", err)
	}

	// Resume at the window edge with no cursor, so the next fetch uses
	// StartLedger. The stored cursor is now meaningless.
	if err := idx.models.Vault.AdvanceCursor(ctx, idx.models.DBConnectionPool,
		idx.contractID, "", int32(window.Oldest)-1); err != nil {
		return nil, fmt.Errorf("resetting the cursor to the window edge: %w", err)
	}

	reset, err := idx.models.Vault.GetIndexerState(ctx, idx.models.DBConnectionPool, idx.contractID)
	if err != nil {
		return nil, fmt.Errorf("re-reading the vault indexer state after the gap reset: %w", err)
	}
	return reset, nil
}

// bootstrap creates the indexing row the first time a vault is seen.
//
// If an operator pinned VAULT_INDEX_FROM_LEDGER to a ledger that has already
// aged out, the vault starts life degraded with the gap recorded, rather than
// pretending a full replay happened.
func (idx *Indexer) bootstrap(ctx context.Context, window ledgerWindow) (*data.VaultIndexerState, error) {
	startFrom := window.Oldest
	state := data.VaultIndexerState{
		ContractID: idx.contractID,
		LastCursor: "",
		Status:     data.VaultIndexerStatusHealthy,
	}

	if idx.indexFromLedger > 0 && idx.indexFromLedger < window.Oldest {
		gapFrom := int32(idx.indexFromLedger)
		gapTo := int32(window.Oldest - 1)
		reason := fmt.Sprintf(
			"bootstrap was pinned to ledger %d, which is older than the RPC retention floor %d; "+
				"events before the floor can never be read, so recipients registered then may be missing.",
			idx.indexFromLedger, window.Oldest)

		log.Ctx(ctx).Warnf("vault %s bootstrapped degraded: %s", idx.contractID, reason)

		state.Status = data.VaultIndexerStatusDegraded
		state.GapFromLedger = &gapFrom
		state.GapToLedger = &gapTo
		state.DegradedReason = &reason
	} else if idx.indexFromLedger > 0 {
		startFrom = idx.indexFromLedger
	}

	// last_ledger is the last ledger *scanned*, so it starts one below the
	// first ledger to read.
	state.LastLedger = int32(startFrom) - 1

	bootstrapped, err := idx.models.Vault.BootstrapIndexerState(ctx, idx.models.DBConnectionPool, state)
	if err != nil {
		return nil, fmt.Errorf("bootstrapping the vault indexer state: %w", err)
	}

	log.Ctx(ctx).Infof("vault %s indexer bootstrapped from ledger %d (status %s)",
		idx.contractID, startFrom, bootstrapped.Status)

	return bootstrapped, nil
}

// runBatch fetches one page of events and applies it. It reports whether there
// may be more to read.
func (idx *Indexer) runBatch(ctx context.Context, state *data.VaultIndexerState, result *Result) (more bool, err error) {
	request := protocol.GetEventsRequest{
		Filters:    idx.filters(),
		Pagination: &protocol.PaginationOptions{Limit: idx.batchLimit},
		Format:     protocol.FormatJSON,
	}

	// A cursor and a ledger range are mutually exclusive in the RPC API, so
	// the cursor is used when there is one and StartLedger only on the first
	// read after a bootstrap or a gap reset.
	if state.LastCursor != "" {
		cursor, parseErr := protocol.ParseCursor(state.LastCursor)
		if parseErr != nil {
			return false, fmt.Errorf("stored cursor %q is unparseable: %w", state.LastCursor, parseErr)
		}
		request.Pagination.Cursor = &cursor
	} else {
		request.StartLedger = uint32(state.LastLedger) + 1
	}

	resp, err := idx.rpcClient.GetEvents(ctx, request)
	if err != nil {
		return false, fmt.Errorf("fetching vault events: %w", err)
	}

	result.EventsRead += len(resp.Events)
	if resp.LatestLedger > result.LatestLedger {
		result.LatestLedger = resp.LatestLedger
	}

	// The response cursor marks how far the scan reached, which is past the
	// last event when the window end was reached. Advancing to it is what
	// keeps an idle vault from looking like it is falling behind.
	scannedThrough := resp.Cursor
	if scannedThrough == "" {
		// Nothing to record and nowhere new to resume from; wait for the next
		// tick rather than re-reading the same range immediately.
		return false, nil
	}

	scannedCursor, err := protocol.ParseCursor(scannedThrough)
	if err != nil {
		return false, fmt.Errorf("response cursor %q is unparseable: %w", scannedThrough, err)
	}

	events, err := idx.decodeBatch(ctx, resp.Events, result)
	if err != nil {
		return false, err
	}

	// Events are applied in ledger order, and the cursor moves with them, in
	// one transaction. A crash anywhere inside rolls back both, so the batch is
	// simply re-fetched; there is no window in which the cursor has advanced
	// past unapplied events.
	err = db.RunInTransaction(ctx, idx.models.DBConnectionPool, nil, func(dbTx db.DBTransaction) error {
		for _, event := range events {
			if applyErr := idx.applyEvent(ctx, dbTx, event, result); applyErr != nil {
				return applyErr
			}
		}

		return idx.models.Vault.AdvanceCursor(ctx, dbTx, idx.contractID, scannedThrough, int32(scannedCursor.Ledger))
	})
	if err != nil {
		return false, fmt.Errorf("applying a vault event batch: %w", err)
	}

	result.LastLedger = int32(scannedCursor.Ledger)

	// A short page means the window end was reached, so there is no point
	// asking again this tick.
	return uint(len(resp.Events)) >= idx.batchLimit, nil
}

// decodeBatch decodes a page of events and sorts them into ledger order.
//
// RPC already returns them in that order; sorting anyway makes the ordering a
// property of this code rather than an assumption about the server, which
// matters because the projections are order-dependent — a `claimed` applied
// before its `registered` would find no row.
func (idx *Indexer) decodeBatch(ctx context.Context, infos []protocol.EventInfo, result *Result) ([]*Event, error) {
	events := make([]*Event, 0, len(infos))

	for _, info := range infos {
		event, err := DecodeEvent(info)
		if err != nil {
			if errors.Is(err, ErrUnknownEventType) {
				// Skip rather than fail: see ErrUnknownEventType. Failing here
				// would freeze the cursor on this event forever.
				log.Ctx(ctx).Warnf("vault %s: skipping event %s: %v", idx.contractID, info.ID, err)
				result.EventsSkipped++
				continue
			}
			return nil, fmt.Errorf("decoding vault event %s: %w", info.ID, err)
		}

		// A response carrying another contract's events means the filter was
		// not applied. Projecting them would mix two vaults' state together.
		if event.ContractID != idx.contractID {
			return nil, fmt.Errorf("event %s belongs to contract %s, not %s",
				event.EventID, event.ContractID, idx.contractID)
		}

		events = append(events, event)
	}

	sort.SliceStable(events, func(i, j int) bool {
		return events[i].SortKey().Cmp(events[j].SortKey()) < 0
	})

	return events, nil
}

// applyEvent records an event and projects it, but projects **only** if the
// insert was new.
//
// That conditional is the idempotency guarantee. Polling is at-least-once, so
// a timeout after RPC served a batch, a retried job, or an operator resync all
// re-present events already applied; projecting one twice would double-count a
// balance or resurrect a cancelled allocation.
func (idx *Indexer) applyEvent(ctx context.Context, dbTx db.DBTransaction, event *Event, result *Result) error {
	row := data.VaultEvent{
		EventID:        event.EventID,
		ContractID:     event.ContractID,
		EventType:      string(event.Type),
		Ledger:         event.Ledger,
		LedgerClosedAt: event.LedgerClosedAt,
		TxHash:         event.TxHash,
		Address:        &event.Address,
		Amount:         event.Amount,
		Payload:        event.Payload,
	}

	inserted, err := idx.models.Vault.InsertEvent(ctx, dbTx, row)
	if err != nil {
		return fmt.Errorf("recording vault event: %w", err)
	}
	if !inserted {
		return nil
	}

	if err := idx.project(ctx, dbTx, event, result); err != nil {
		return fmt.Errorf("projecting %s event %s: %w", event.Type, event.EventID, err)
	}

	result.EventsApplied++
	return nil
}

// project applies one event to the allocation and state projections.
func (idx *Indexer) project(ctx context.Context, dbTx db.DBTransaction, event *Event, result *Result) error {
	vaultModel := idx.models.Vault

	switch event.Type {
	case EventTypeInitialized:
		if event.Deadline == nil {
			return errors.New("initialized event has no deadline")
		}
		// Seeds the snapshot. Totals start at zero because initialize runs
		// before any fund or register, so there is nothing yet to carry over.
		if err := vaultModel.UpsertState(ctx, dbTx, data.VaultState{
			ContractID:     event.ContractID,
			AdminAddress:   event.Address,
			TokenAddress:   event.Token,
			Deadline:       *event.Deadline,
			SnapshotLedger: event.Ledger,
		}); err != nil {
			return fmt.Errorf("seeding the vault state: %w", err)
		}
		return nil

	case EventTypeFunded:
		if event.Balance == nil {
			return errors.New("funded event has no balance")
		}
		// funded publishes the resulting balance, so this is an absolute set
		// rather than a delta.
		if err := vaultModel.ApplyFundedBalance(ctx, dbTx, event.ContractID, *event.Balance, event.Ledger); err != nil {
			return fmt.Errorf("applying the funded balance: %w", err)
		}
		return nil

	case EventTypeRegistered:
		if event.Amount == nil || event.TotalAllocated == nil {
			return errors.New("registered event is missing an amount or total")
		}
		if err := vaultModel.RegisterAllocation(ctx, dbTx, data.VaultAllocation{
			ContractID:       event.ContractID,
			Recipient:        event.Address,
			Amount:           *event.Amount,
			RegisteredLedger: event.Ledger,
			RegisteredAt:     event.LedgerClosedAt,
			LastEventID:      event.EventID,
		}); err != nil {
			return fmt.Errorf("registering the allocation: %w", err)
		}
		if err := vaultModel.ApplyTotalAllocated(ctx, dbTx, event.ContractID, *event.TotalAllocated, event.Ledger); err != nil {
			return fmt.Errorf("applying the registered total: %w", err)
		}
		return nil

	case EventTypeClaimed:
		if err := idx.settle(ctx, dbTx, event, data.VaultAllocationStatusClaimed, result); err != nil {
			return err
		}
		if err := vaultModel.ApplyClaim(ctx, dbTx, event.ContractID, *event.Amount, event.Ledger); err != nil {
			return fmt.Errorf("applying the claim: %w", err)
		}
		return nil

	case EventTypeCancelled:
		if err := idx.settle(ctx, dbTx, event, data.VaultAllocationStatusCancelled, result); err != nil {
			return err
		}
		if err := vaultModel.ApplyCancellation(ctx, dbTx, event.ContractID, *event.Amount, event.Ledger); err != nil {
			return fmt.Errorf("applying the cancellation: %w", err)
		}
		return nil

	case EventTypeWithdrawn:
		if event.Amount == nil {
			return errors.New("withdrawn event has no amount")
		}
		if err := vaultModel.ApplyWithdrawal(ctx, dbTx, event.ContractID, *event.Amount, event.Ledger); err != nil {
			return fmt.Errorf("applying the withdrawal: %w", err)
		}
		return nil

	default:
		return fmt.Errorf("%q: %w", event.Type, ErrUnknownEventType)
	}
}

// settle moves an allocation to a terminal state, tolerating a missing row.
//
// A missing row means the `registered` event aged out of the retention window
// before it was read. Failing here would wedge the batch over data the network
// no longer has, so the anomaly is counted and logged and indexing continues —
// the vault is already flagged degraded, so the incomplete list is reported
// rather than hidden.
func (idx *Indexer) settle(
	ctx context.Context,
	dbTx db.DBTransaction,
	event *Event,
	status data.VaultAllocationStatus,
	result *Result,
) error {
	if event.Amount == nil {
		return fmt.Errorf("%s event has no amount", event.Type)
	}

	settled, err := idx.models.Vault.SettleAllocation(ctx, dbTx,
		event.ContractID, event.Address, status, event.Ledger, event.LedgerClosedAt, event.EventID)
	if err != nil {
		return fmt.Errorf("settling the allocation: %w", err)
	}
	if !settled {
		log.Ctx(ctx).Warnf(
			"vault %s: %s event %s found no pending allocation for %s; "+
				"its registration is likely outside the RPC retention window",
			idx.contractID, event.Type, event.EventID, event.Address)
		result.Anomalies++
	}

	return nil
}

// Compile-time check that internal/stellar's client satisfies the narrower
// interface the indexer declares, so the two cannot drift apart silently.
var _ RPCClient = (stellar.RPCClient)(nil)
