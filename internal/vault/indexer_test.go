package vault

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	protocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stellar/stellar-disbursement-platform-backend/db"
	"github.com/stellar/stellar-disbursement-platform-backend/db/dbtest"
	"github.com/stellar/stellar-disbursement-platform-backend/internal/data"
)

// ---------------------------------------------------------------------------
// Test RPC server
// ---------------------------------------------------------------------------

// fakeRPC is a scripted RPCClient.
//
// It is hand-written rather than generated because the indexer's behaviour
// depends on the *relationship* between the retention window and the stored
// cursor, and expressing "this window, these events, served from this cursor"
// is far clearer as a small fake than as a pile of per-call mock
// expectations.
type fakeRPC struct {
	oldestLedger uint32
	latestLedger uint32

	// events is the whole history the server knows, in ledger order.
	events []protocol.EventInfo

	// errOnGetEvents, when set, fails every GetEvents call.
	errOnGetEvents error
	// errOnLatestLedger, when set, fails every GetLatestLedgerSequence call.
	errOnLatestLedger error

	// requests records every GetEvents request, so tests can assert on what
	// was actually asked for — the filter, the cursor, the format.
	requests []protocol.GetEventsRequest

	// contract is the state SimulateTransaction reads (snapshot_test.go).
	// Nil makes every simulation fail as if RPC were unreachable.
	contract *fakeContract
	// simulations records every simulated read.
	simulations []simulatedCall
}

func (f *fakeRPC) GetLatestLedgerSequence(context.Context) (uint32, error) {
	if f.errOnLatestLedger != nil {
		return 0, f.errOnLatestLedger
	}
	return f.latestLedger, nil
}

func (f *fakeRPC) GetEvents(_ context.Context, request protocol.GetEventsRequest) (*protocol.GetEventsResponse, error) {
	f.requests = append(f.requests, request)

	if f.errOnGetEvents != nil {
		return nil, f.errOnGetEvents
	}

	// Mirror the real server's refusal to serve a start ledger below the
	// retention floor. A test that marks a vault degraded is only meaningful
	// if the alternative would actually have failed.
	if request.StartLedger != 0 && request.StartLedger < f.oldestLedger {
		return nil, fmt.Errorf("startLedger must be within the ledger range: %d - %d", f.oldestLedger, f.latestLedger)
	}

	limit := request.Pagination.Limit
	if limit == 0 {
		limit = 100
	}

	var selected []protocol.EventInfo
	for _, event := range f.events {
		if !f.serves(request, event) {
			continue
		}
		selected = append(selected, event)
		if uint(len(selected)) >= limit {
			break
		}
	}

	resp := &protocol.GetEventsResponse{
		Events:       selected,
		LatestLedger: f.latestLedger,
		OldestLedger: f.oldestLedger,
	}

	// The real server returns the point the scan reached. When the window end
	// is reached that is past the last event, which is what lets an idle
	// vault's cursor keep moving.
	if len(selected) > 0 && uint(len(selected)) >= limit {
		resp.Cursor = selected[len(selected)-1].ID
	} else {
		resp.Cursor = protocol.Cursor{Ledger: f.latestLedger, Tx: 0, Op: 0, Event: 0}.String()
	}

	return resp, nil
}

// serves decides whether one event falls after the request's resume point.
func (f *fakeRPC) serves(request protocol.GetEventsRequest, event protocol.EventInfo) bool {
	if request.Pagination != nil && request.Pagination.Cursor != nil {
		eventCursor, err := protocol.ParseCursor(event.ID)
		if err != nil {
			return false
		}
		return eventCursor.Cmp(*request.Pagination.Cursor) > 0
	}
	return uint32(event.Ledger) >= request.StartLedger
}

// ---------------------------------------------------------------------------
// Event builders
// ---------------------------------------------------------------------------

// cursorFor builds the event id RPC would assign, so the ids the tests use
// parse into real cursors and sort the way the ledger ordered them.
func cursorFor(ledger uint32, tx, op, event uint32) string {
	return protocol.Cursor{Ledger: ledger, Tx: tx, Op: op, Event: event}.String()
}

func eventAt(ledger uint32, tx uint32, name, address, value string) protocol.EventInfo {
	return protocol.EventInfo{
		EventType:       protocol.EventTypeContract,
		Ledger:          int32(ledger),
		LedgerClosedAt:  time.Unix(1_793_000_000+int64(ledger)*5, 0).UTC().Format(time.RFC3339),
		ContractID:      vaultContractID,
		ID:              cursorFor(ledger, tx, 0, 0),
		TxIndex:         tx,
		TransactionHash: fmt.Sprintf("%064d", ledger*1000+tx),
		TopicJSON: []json.RawMessage{
			json.RawMessage(`{"symbol":"` + name + `"}`),
			json.RawMessage(`{"address":"` + address + `"}`),
		},
		ValueJSON: json.RawMessage(value),
	}
}

// testDeadline is the claim deadline every fixture vault uses: comfortably in
// the future, so a fixture is never accidentally expired.
const testDeadline int64 = 1_793_900_000

// initializedAt builds the contract's one-off initialize event. The ledger and
// deadline are fixed rather than parameters: it is always the vault's first
// event, and no test varies the deadline.
func initializedAt() protocol.EventInfo {
	return eventAt(1000, 1, "initialized", adminAddress,
		fmt.Sprintf(`{"map":[{"key":{"symbol":"deadline"},"val":{"u64":"%d"}},`+
			`{"key":{"symbol":"token"},"val":{"address":"%s"}}]}`, testDeadline, tokenContractID))
}

func fundedAt(ledger uint32, amount, balance string) protocol.EventInfo {
	return eventAt(ledger, 2, "funded", adminAddress,
		fmt.Sprintf(`{"map":[{"key":{"symbol":"amount"},"val":{"i128":"%s"}},`+
			`{"key":{"symbol":"balance"},"val":{"i128":"%s"}}]}`, amount, balance))
}

func registeredAt(ledger uint32, tx uint32, recipient, amount, totalAllocated string) protocol.EventInfo {
	return eventAt(ledger, tx, "registered", recipient,
		fmt.Sprintf(`{"map":[{"key":{"symbol":"amount"},"val":{"i128":"%s"}},`+
			`{"key":{"symbol":"total_allocated"},"val":{"i128":"%s"}}]}`, amount, totalAllocated))
}

// settledAt builds a claimed, cancelled or withdrawn event: the three that
// carry only an amount.
func settledAt(ledger uint32, name, recipient, amount string) protocol.EventInfo {
	return eventAt(ledger, 1, name, recipient,
		fmt.Sprintf(`{"map":[{"key":{"symbol":"amount"},"val":{"i128":"%s"}}]}`, amount))
}

// lifecycleEvents is a full vault history: initialize, fund, two
// registrations, one claim, one cancellation, one withdrawal.
func lifecycleEvents() []protocol.EventInfo {
	return []protocol.EventInfo{
		initializedAt(),
		fundedAt(1001, "30000000", "30000000"),
		registeredAt(1002, 1, recipientAddress, "10000000", "10000000"),
		registeredAt(1002, 2, otherRecipient, "20000000", "30000000"),
		settledAt(1003, "claimed", recipientAddress, "10000000"),
		settledAt(1004, "cancelled", otherRecipient, "20000000"),
		settledAt(1005, "withdrawn", adminAddress, "20000000"),
	}
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

func newIndexerTest(t *testing.T, rpcClient RPCClient) (context.Context, *data.Models, *Indexer) {
	t.Helper()

	dbt := dbtest.Open(t)
	t.Cleanup(func() { dbt.Close() })

	dbConnectionPool, err := db.OpenDBConnectionPool(dbt.DSN)
	require.NoError(t, err)
	t.Cleanup(func() { dbConnectionPool.Close() })

	models, err := data.NewModels(dbConnectionPool)
	require.NoError(t, err)

	indexer, err := NewIndexer(rpcClient, models, vaultContractID, 0)
	require.NoError(t, err)

	return context.Background(), models, indexer
}

// ---------------------------------------------------------------------------
// Construction
// ---------------------------------------------------------------------------

func Test_NewIndexer_requiresItsDependencies(t *testing.T) {
	models := &data.Models{}

	testCases := []struct {
		name       string
		rpcClient  RPCClient
		models     *data.Models
		contractID string
		wantErr    string
	}{
		{name: "no rpc client", models: models, contractID: vaultContractID, wantErr: "rpcClient is required"},
		{name: "no models", rpcClient: &fakeRPC{}, contractID: vaultContractID, wantErr: "models is required"},
		{name: "no contract id", rpcClient: &fakeRPC{}, models: models, wantErr: "contractID is required"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			indexer, err := NewIndexer(tc.rpcClient, tc.models, tc.contractID, 0)

			assert.ErrorContains(t, err, tc.wantErr)
			assert.Nil(t, indexer)
		})
	}
}

// ---------------------------------------------------------------------------
// The happy path
// ---------------------------------------------------------------------------

func Test_Indexer_Run_projectsAFullLifecycle(t *testing.T) {
	rpcClient := &fakeRPC{oldestLedger: 900, latestLedger: 1100, events: lifecycleEvents()}
	ctx, models, indexer := newIndexerTest(t, rpcClient)

	result, err := indexer.Run(ctx)
	require.NoError(t, err)

	assert.Equal(t, 7, result.EventsRead)
	assert.Equal(t, 7, result.EventsApplied)
	assert.Equal(t, 0, result.EventsSkipped)
	assert.Equal(t, 0, result.Anomalies, "a complete history must produce no anomalies")
	assert.False(t, result.Degraded)

	// vault_state must match what the contract would report.
	state, err := models.Vault.GetState(ctx, models.DBConnectionPool, vaultContractID)
	require.NoError(t, err)
	assert.Equal(t, adminAddress, state.AdminAddress)
	assert.Equal(t, tokenContractID, state.TokenAddress)
	assert.Equal(t, time.Unix(testDeadline, 0).UTC(), state.Deadline.UTC())
	// 30 funded, 10 claimed out, 20 withdrawn out.
	assert.Equal(t, "0", state.Balance.String())
	assert.Equal(t, "10000000", state.TotalClaimed.String())
	// Both allocations settled, so nothing is still encumbered.
	assert.Equal(t, "0", state.TotalAllocated.String())

	// And the allocations must be in their terminal states.
	claimed, err := models.Vault.GetAllocation(ctx, models.DBConnectionPool, vaultContractID, recipientAddress)
	require.NoError(t, err)
	assert.Equal(t, data.VaultAllocationStatusClaimed, claimed.Status)
	assert.Equal(t, "10000000", claimed.Amount.String())
	require.NotNil(t, claimed.SettledLedger)
	assert.Equal(t, int32(1003), *claimed.SettledLedger)

	cancelled, err := models.Vault.GetAllocation(ctx, models.DBConnectionPool, vaultContractID, otherRecipient)
	require.NoError(t, err)
	assert.Equal(t, data.VaultAllocationStatusCancelled, cancelled.Status)
	assert.Equal(t, "20000000", cancelled.Amount.String())

	// Every event must be in the append-only log.
	total, err := models.Vault.CountEvents(ctx, models.DBConnectionPool, data.VaultEventFilter{ContractID: vaultContractID})
	require.NoError(t, err)
	assert.Equal(t, 7, total)
}

func Test_Indexer_Run_asksRPCForTheRightThing(t *testing.T) {
	rpcClient := &fakeRPC{oldestLedger: 900, latestLedger: 1100, events: lifecycleEvents()}
	ctx, _, indexer := newIndexerTest(t, rpcClient)

	_, err := indexer.Run(ctx)
	require.NoError(t, err)

	require.NotEmpty(t, rpcClient.requests)
	for i, request := range rpcClient.requests {
		// xdrFormat=json is what populates topicJson/valueJson; without it the
		// decoder has nothing to read.
		assert.Equal(t, protocol.FormatJSON, request.Format, "request %d", i)

		require.Len(t, request.Filters, 1, "request %d", i)
		filter := request.Filters[0]
		assert.Equal(t, []string{vaultContractID}, filter.ContractIDs, "request %d", i)

		// Filtering to contract events is what keeps failed invocations out:
		// a failure surfaces as a diagnostic event, which must never be
		// requested and never projected.
		_, hasContract := filter.EventType[protocol.EventTypeContract]
		assert.True(t, hasContract, "request %d must filter to contract events", i)
		assert.Len(t, filter.EventType, 1, "request %d", i)
	}
}

func Test_Indexer_Run_advancesTheCursorOnAnIdleVault(t *testing.T) {
	// A vault that has emitted nothing must still have its cursor move, or it
	// would look like the indexer is falling behind and eventually trip the
	// lag alert for no reason.
	rpcClient := &fakeRPC{oldestLedger: 900, latestLedger: 1100}
	ctx, models, indexer := newIndexerTest(t, rpcClient)

	result, err := indexer.Run(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, result.EventsRead)
	assert.Equal(t, int32(1100), result.LastLedger)
	assert.Zero(t, result.LagLedgers())

	state, err := models.Vault.GetIndexerState(ctx, models.DBConnectionPool, vaultContractID)
	require.NoError(t, err)
	assert.Equal(t, int32(1100), state.LastLedger)
	assert.Equal(t, data.VaultIndexerStatusHealthy, state.Status)
}

func Test_Indexer_Run_idleRunsAdvanceLastIndexedAt(t *testing.T) {
	// GET /vault/{id}/status reports "stalled / no_recent_commit" when
	// last_indexed_at is older than VAULT_STATUS_STALL_SECONDS. That rule is
	// only sound if a run that reads *no* events still commits and moves the
	// timestamp; otherwise every quiet vault would be reported as stalled.
	testCases := []struct {
		name       string
		nextLatest uint32
	}{
		{name: "chain tip moved, no events", nextLatest: 1110},
		{name: "chain tip unchanged, no events", nextLatest: 1100},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			rpcClient := &fakeRPC{oldestLedger: 900, latestLedger: 1100}
			ctx, models, indexer := newIndexerTest(t, rpcClient)

			_, err := indexer.Run(ctx)
			require.NoError(t, err)

			// Backdate the timestamp so "advanced" cannot be confused with
			// "set moments ago by the first run".
			_, err = models.DBConnectionPool.ExecContext(ctx,
				`UPDATE vault_indexer_state SET last_indexed_at = NOW() - INTERVAL '1 hour' WHERE contract_id = $1`,
				vaultContractID)
			require.NoError(t, err)
			before, err := models.Vault.GetIndexerState(ctx, models.DBConnectionPool, vaultContractID)
			require.NoError(t, err)
			require.NotNil(t, before.LastIndexedAt)

			rpcClient.latestLedger = tc.nextLatest
			result, err := indexer.Run(ctx)
			require.NoError(t, err)
			require.Zero(t, result.EventsRead, "this test is about runs that read nothing")

			after, err := models.Vault.GetIndexerState(ctx, models.DBConnectionPool, vaultContractID)
			require.NoError(t, err)
			require.NotNil(t, after.LastIndexedAt)
			assert.True(t, after.LastIndexedAt.After(before.LastIndexedAt.Add(59*time.Minute)),
				"an idle run must move last_indexed_at forward: before=%s after=%s",
				before.LastIndexedAt, after.LastIndexedAt)
			assert.WithinDuration(t, time.Now(), *after.LastIndexedAt, time.Minute)
			assert.Equal(t, int32(tc.nextLatest), after.LastLedger)
		})
	}
}

// ---------------------------------------------------------------------------
// Idempotency and replay
// ---------------------------------------------------------------------------

func Test_Indexer_Run_steadyStateReadsNothingNew(t *testing.T) {
	// Once caught up, a second tick must re-read nothing: the cursor has
	// advanced past the whole history. This is cheapness, not idempotency —
	// the idempotency gate is exercised by the replay test below, where the
	// events really are re-delivered.
	rpcClient := &fakeRPC{oldestLedger: 900, latestLedger: 1100, events: lifecycleEvents()}
	ctx, models, indexer := newIndexerTest(t, rpcClient)

	_, err := indexer.Run(ctx)
	require.NoError(t, err)

	second, err := indexer.Run(ctx)
	require.NoError(t, err)
	assert.Zero(t, second.EventsRead, "a caught-up indexer must not re-fetch the history")
	assert.Zero(t, second.EventsApplied)

	total, err := models.Vault.CountEvents(ctx, models.DBConnectionPool, data.VaultEventFilter{ContractID: vaultContractID})
	require.NoError(t, err)
	assert.Equal(t, 7, total)
}

func Test_Indexer_Run_replayFromARewoundCursorIsIdempotent(t *testing.T) {
	// The real idempotency test, and the one the resync endpoint depends on:
	// rewind the cursor so the whole history is genuinely re-delivered, then
	// assert nothing is applied twice and every total is unchanged.
	//
	// Removing the `if !inserted { return }` guard in applyEvent makes this
	// fail — verified by mutation, so the assertions below are load-bearing
	// rather than trivially satisfied.
	rpcClient := &fakeRPC{oldestLedger: 900, latestLedger: 1100, events: lifecycleEvents()}
	ctx, models, indexer := newIndexerTest(t, rpcClient)

	_, err := indexer.Run(ctx)
	require.NoError(t, err)
	before, err := models.Vault.GetState(ctx, models.DBConnectionPool, vaultContractID)
	require.NoError(t, err)

	// Rewind to the window floor, exactly as a resync would.
	require.NoError(t, models.Vault.AdvanceCursor(ctx, models.DBConnectionPool, vaultContractID, "", 899))

	result, err := indexer.Run(ctx)
	require.NoError(t, err)
	require.Equal(t, 7, result.EventsRead, "the whole history must actually be re-read")
	assert.Zero(t, result.EventsApplied, "but nothing may be applied twice")

	after, err := models.Vault.GetState(ctx, models.DBConnectionPool, vaultContractID)
	require.NoError(t, err)
	assert.Equal(t, before.Balance.String(), after.Balance.String())
	assert.Equal(t, before.TotalClaimed.String(), after.TotalClaimed.String())
	assert.Equal(t, before.TotalAllocated.String(), after.TotalAllocated.String())

	total, err := models.Vault.CountEvents(ctx, models.DBConnectionPool, data.VaultEventFilter{ContractID: vaultContractID})
	require.NoError(t, err)
	assert.Equal(t, 7, total, "no event may be recorded twice")

	// And the allocations must not have been resurrected out of their
	// terminal states.
	claimed, err := models.Vault.GetAllocation(ctx, models.DBConnectionPool, vaultContractID, recipientAddress)
	require.NoError(t, err)
	assert.Equal(t, data.VaultAllocationStatusClaimed, claimed.Status)
}

// ---------------------------------------------------------------------------
// Ordering and transactionality
// ---------------------------------------------------------------------------

func Test_Indexer_Run_appliesEventsInLedgerOrder(t *testing.T) {
	// RPC returns events in ledger order, but the projections are
	// order-dependent — a `claimed` applied before its `registered` finds no
	// row — so the indexer sorts rather than trusting the server. Feeding a
	// deliberately shuffled history proves the sort is load-bearing.
	shuffled := []protocol.EventInfo{
		settledAt(1003, "claimed", recipientAddress, "10000000"),
		registeredAt(1002, 1, recipientAddress, "10000000", "10000000"),
		fundedAt(1001, "30000000", "30000000"),
		initializedAt(),
	}
	rpcClient := &fakeRPC{oldestLedger: 900, latestLedger: 1100, events: shuffled}
	ctx, models, indexer := newIndexerTest(t, rpcClient)

	result, err := indexer.Run(ctx)
	require.NoError(t, err)
	assert.Equal(t, 4, result.EventsApplied)
	assert.Equal(t, 0, result.Anomalies, "ordering must mean the claim finds its registration")

	allocation, err := models.Vault.GetAllocation(ctx, models.DBConnectionPool, vaultContractID, recipientAddress)
	require.NoError(t, err)
	assert.Equal(t, data.VaultAllocationStatusClaimed, allocation.Status)
}

func Test_Indexer_Run_cursorAndEventsCommitTogether(t *testing.T) {
	// A poisoned event fails the batch. The cursor must roll back with the
	// projections, or the batch would be skipped on the next tick and those
	// events lost forever.
	//
	// A `registered` event for a recipient who already has an allocation
	// violates the primary key, which is the cheapest way to make the
	// transaction fail partway through a multi-event batch.
	events := []protocol.EventInfo{
		initializedAt(),
		fundedAt(1001, "30000000", "30000000"),
		registeredAt(1002, 1, recipientAddress, "10000000", "10000000"),
		registeredAt(1002, 2, recipientAddress, "10000000", "20000000"),
	}
	rpcClient := &fakeRPC{oldestLedger: 900, latestLedger: 1100, events: events}
	ctx, models, indexer := newIndexerTest(t, rpcClient)

	_, err := indexer.Run(ctx)
	require.Error(t, err)
	assert.ErrorContains(t, err, "vault_allocations_pkey")

	// Nothing from the failed batch may have landed.
	total, err := models.Vault.CountEvents(ctx, models.DBConnectionPool, data.VaultEventFilter{ContractID: vaultContractID})
	require.NoError(t, err)
	assert.Zero(t, total, "no event may survive a rolled-back batch")

	_, err = models.Vault.GetState(ctx, models.DBConnectionPool, vaultContractID)
	assert.ErrorIs(t, err, data.ErrRecordNotFound, "the initialized projection must have rolled back too")

	// And the cursor must not have moved past the events that were never
	// applied.
	state, err := models.Vault.GetIndexerState(ctx, models.DBConnectionPool, vaultContractID)
	require.NoError(t, err)
	assert.Equal(t, int32(899), state.LastLedger, "the cursor must stay at the bootstrap position")
	assert.Empty(t, state.LastCursor)
}

func Test_Indexer_Run_paginatesThroughABacklog(t *testing.T) {
	// A bootstrap replay has to cross many pages. Each page commits on its
	// own, so progress survives a crash mid-run.
	var events []protocol.EventInfo
	events = append(events, initializedAt())
	events = append(events, fundedAt(1001, "1000000000", "1000000000"))

	// 25 registrations, which at a batch limit of 4 spans several pages.
	recipients := []string{recipientAddress, otherRecipient, adminAddress}
	for i := 0; i < 25; i++ {
		// Distinct recipients are needed for distinct allocations, so reuse
		// the three known addresses across distinct ledgers and only register
		// each once by using a withdrawal for the rest.
		if i < len(recipients) {
			events = append(events, registeredAt(uint32(1002+i), 1, recipients[i], "10000000", fmt.Sprint((i+1)*10000000)))
			continue
		}
		events = append(events, settledAt(uint32(1002+i), "withdrawn", adminAddress, "1"))
	}

	rpcClient := &fakeRPC{oldestLedger: 900, latestLedger: 1100, events: events}
	ctx, models, indexer := newIndexerTest(t, rpcClient)
	indexer.batchLimit = 4

	result, err := indexer.Run(ctx)
	require.NoError(t, err)
	assert.Equal(t, 27, result.EventsApplied)
	assert.Greater(t, len(rpcClient.requests), 4, "a backlog must take several pages")

	total, err := models.Vault.CountEvents(ctx, models.DBConnectionPool, data.VaultEventFilter{ContractID: vaultContractID})
	require.NoError(t, err)
	assert.Equal(t, 27, total)
}

// ---------------------------------------------------------------------------
// The retention gap
// ---------------------------------------------------------------------------

func Test_Indexer_Run_marksDegradedWhenTheResumePointAgedOut(t *testing.T) {
	rpcClient := &fakeRPC{oldestLedger: 900, latestLedger: 1100, events: lifecycleEvents()}
	ctx, models, indexer := newIndexerTest(t, rpcClient)

	// Index normally, then simulate the indexer having been down long enough
	// for its position to fall out of the window.
	_, err := indexer.Run(ctx)
	require.NoError(t, err)
	require.NoError(t, models.Vault.AdvanceCursor(ctx, models.DBConnectionPool, vaultContractID, "", 500))

	rpcClient.oldestLedger = 800
	rpcClient.latestLedger = 130_000

	result, err := indexer.Run(ctx)
	require.NoError(t, err, "a gap must never surface as an error; it is a state, not a failure")
	assert.True(t, result.Degraded)

	state, err := models.Vault.GetIndexerState(ctx, models.DBConnectionPool, vaultContractID)
	require.NoError(t, err)
	assert.Equal(t, data.VaultIndexerStatusDegraded, state.Status)
	require.NotNil(t, state.GapFromLedger)
	require.NotNil(t, state.GapToLedger)
	// The gap runs from the first unscanned ledger to the ledger below the
	// retention floor.
	assert.Equal(t, int32(501), *state.GapFromLedger)
	assert.Equal(t, int32(799), *state.GapToLedger)
	require.NotNil(t, state.DegradedReason)
	assert.Contains(t, *state.DegradedReason, "retention window")
	assert.Contains(t, *state.DegradedReason, "On-chain funds are unaffected")
}

func Test_Indexer_Run_aGapDoesNotRetryLoop(t *testing.T) {
	// The failure this guards against: RPC rejects a start ledger below the
	// retention floor, so an implementation that fetched first and asked
	// questions later would fail, leave the cursor untouched, and fail
	// identically forever — a retry loop over data that is gone.
	rpcClient := &fakeRPC{oldestLedger: 100_000, latestLedger: 130_000, events: nil}
	ctx, models, indexer := newIndexerTest(t, rpcClient)

	// Bootstrap at a position far behind the floor.
	_, err := models.Vault.BootstrapIndexerState(ctx, models.DBConnectionPool, data.VaultIndexerState{
		ContractID: vaultContractID,
		LastCursor: "",
		LastLedger: 500,
		Status:     data.VaultIndexerStatusHealthy,
	})
	require.NoError(t, err)

	for run := 1; run <= 3; run++ {
		result, err := indexer.Run(ctx)
		require.NoError(t, err, "run %d must not error", run)
		assert.True(t, result.Degraded, "run %d", run)

		state, stateErr := models.Vault.GetIndexerState(ctx, models.DBConnectionPool, vaultContractID)
		require.NoError(t, stateErr)

		// After the first run the position is at or past the floor, so the gap
		// is recorded once and indexing proceeds normally from then on.
		assert.GreaterOrEqual(t, state.LastLedger, int32(rpcClient.oldestLedger)-1, "run %d", run)
	}

	// The gap bounds must still describe the original loss, not be rewritten
	// to an empty range by later runs.
	state, err := models.Vault.GetIndexerState(ctx, models.DBConnectionPool, vaultContractID)
	require.NoError(t, err)
	require.NotNil(t, state.GapFromLedger)
	assert.Equal(t, int32(501), *state.GapFromLedger)
	assert.Equal(t, int32(99_999), *state.GapToLedger)
}

func Test_Indexer_Run_bootstrapPinnedBelowTheFloorStartsDegraded(t *testing.T) {
	// VAULT_INDEX_FROM_LEDGER pinned to a ledger that has already aged out.
	// Starting life "healthy" would claim a full replay happened when it
	// could not have.
	rpcClient := &fakeRPC{oldestLedger: 100_000, latestLedger: 130_000, events: nil}
	ctx, models, _ := newIndexerTest(t, rpcClient)

	indexer, err := NewIndexer(rpcClient, models, vaultContractID, 50_000)
	require.NoError(t, err)

	result, err := indexer.Run(ctx)
	require.NoError(t, err)
	assert.True(t, result.Degraded)

	state, err := models.Vault.GetIndexerState(ctx, models.DBConnectionPool, vaultContractID)
	require.NoError(t, err)
	assert.Equal(t, data.VaultIndexerStatusDegraded, state.Status)
	require.NotNil(t, state.GapFromLedger)
	assert.Equal(t, int32(50_000), *state.GapFromLedger)
	assert.Equal(t, int32(99_999), *state.GapToLedger)
	assert.Contains(t, *state.DegradedReason, "retention floor")
}

func Test_Indexer_Run_bootstrapPinnedInsideTheWindowStartsThere(t *testing.T) {
	rpcClient := &fakeRPC{oldestLedger: 900, latestLedger: 1100, events: lifecycleEvents()}
	ctx, models, _ := newIndexerTest(t, rpcClient)

	// Start after the initialize and fund events.
	indexer, err := NewIndexer(rpcClient, models, vaultContractID, 1002)
	require.NoError(t, err)

	result, err := indexer.Run(ctx)
	require.NoError(t, err)
	assert.False(t, result.Degraded)
	assert.Equal(t, 5, result.EventsRead, "the two events before the pinned ledger are not read")
}

func Test_Indexer_Run_countsASettlementWithNoRegistrationAsAnAnomaly(t *testing.T) {
	// The concrete shape of a retention gap: the `registered` event aged out,
	// but the `claimed` event is still in the window. The allocation cannot be
	// reconstructed — the contract exposes no way to enumerate recipients — so
	// this must be counted and reported, not silently dropped and not allowed
	// to wedge the batch.
	events := []protocol.EventInfo{
		initializedAt(),
		fundedAt(1001, "30000000", "30000000"),
		settledAt(1003, "claimed", recipientAddress, "10000000"),
	}
	rpcClient := &fakeRPC{oldestLedger: 900, latestLedger: 1100, events: events}
	ctx, models, indexer := newIndexerTest(t, rpcClient)

	result, err := indexer.Run(ctx)
	require.NoError(t, err, "a missing registration must not fail the batch")
	assert.Equal(t, 3, result.EventsApplied)
	assert.Equal(t, 1, result.Anomalies)

	// The claim still moved the money, so the cached totals must reflect it.
	state, err := models.Vault.GetState(ctx, models.DBConnectionPool, vaultContractID)
	require.NoError(t, err)
	assert.Equal(t, "10000000", state.TotalClaimed.String())
	assert.Equal(t, "20000000", state.Balance.String())
	// total_allocated would have gone negative; the floor keeps the row legal
	// rather than failing vault_state_totals_check and wedging the indexer.
	assert.Equal(t, "0", state.TotalAllocated.String())
}

// ---------------------------------------------------------------------------
// Forward compatibility and failure handling
// ---------------------------------------------------------------------------

func Test_Indexer_Run_skipsAnUnknownEventTypeRatherThanWedging(t *testing.T) {
	// A contract upgrade adding a seventh event must not freeze the cursor on
	// it. Everything the build does understand has to keep flowing.
	events := []protocol.EventInfo{
		initializedAt(),
		eventAt(1001, 1, "topped_up", adminAddress, `{"map":[{"key":{"symbol":"amount"},"val":{"i128":"1"}}]}`),
		fundedAt(1002, "30000000", "30000000"),
	}
	rpcClient := &fakeRPC{oldestLedger: 900, latestLedger: 1100, events: events}
	ctx, models, indexer := newIndexerTest(t, rpcClient)

	result, err := indexer.Run(ctx)
	require.NoError(t, err)
	assert.Equal(t, 3, result.EventsRead)
	assert.Equal(t, 2, result.EventsApplied)
	assert.Equal(t, 1, result.EventsSkipped)

	// The cursor must have moved past the unknown event.
	state, err := models.Vault.GetIndexerState(ctx, models.DBConnectionPool, vaultContractID)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, state.LastLedger, int32(1002))

	// And the event after it must have been applied.
	vaultState, err := models.Vault.GetState(ctx, models.DBConnectionPool, vaultContractID)
	require.NoError(t, err)
	assert.Equal(t, "30000000", vaultState.Balance.String())
}

func Test_Indexer_Run_rejectsAnotherContractsEvents(t *testing.T) {
	// If the contract filter were not applied, two vaults' state would be
	// mixed together. Better to fail loudly than to project the wrong vault.
	foreign := initializedAt()
	foreign.ContractID = tokenContractID
	foreign.ID = cursorFor(1000, 9, 0, 0)

	rpcClient := &fakeRPC{oldestLedger: 900, latestLedger: 1100, events: []protocol.EventInfo{foreign}}
	ctx, _, indexer := newIndexerTest(t, rpcClient)

	_, err := indexer.Run(ctx)

	assert.ErrorContains(t, err, "belongs to contract")
}

func Test_Indexer_Run_propagatesRPCFailures(t *testing.T) {
	testCases := []struct {
		name      string
		rpcClient *fakeRPC
		wantErr   string
	}{
		{
			name:      "the latest ledger is unavailable",
			rpcClient: &fakeRPC{errOnLatestLedger: errors.New("connection refused")},
			wantErr:   "getting the latest ledger",
		},
		{
			name:      "getEvents fails",
			rpcClient: &fakeRPC{latestLedger: 1100, errOnGetEvents: errors.New("502 bad gateway")},
			wantErr:   "probing the event window",
		},
		{
			name: "RPC reports no retention floor, so the window is unknown",
			// oldestLedger 0 means the window cannot be reasoned about, and
			// guessing would risk silently skipping events.
			rpcClient: &fakeRPC{latestLedger: 1100, oldestLedger: 0},
			wantErr:   "retention window is unknown",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, _, indexer := newIndexerTest(t, tc.rpcClient)

			result, err := indexer.Run(ctx)

			assert.ErrorContains(t, err, tc.wantErr)
			assert.Nil(t, result)
		})
	}
}

func Test_Indexer_Run_rejectsAMalformedEvent(t *testing.T) {
	// Decode failures other than an unknown type are real faults, and must
	// stop the batch rather than silently dropping state-changing events.
	broken := fundedAt(1001, "30000000", "30000000")
	broken.ValueJSON = json.RawMessage(`{"map":[{"key":{"symbol":"amount"},"val":{"i128":"1"}}]}`)

	rpcClient := &fakeRPC{oldestLedger: 900, latestLedger: 1100, events: []protocol.EventInfo{broken}}
	ctx, _, indexer := newIndexerTest(t, rpcClient)

	_, err := indexer.Run(ctx)

	assert.ErrorContains(t, err, `missing field "balance"`)
}

// ---------------------------------------------------------------------------
// Lag
// ---------------------------------------------------------------------------

func Test_Result_LagLedgers(t *testing.T) {
	testCases := []struct {
		name   string
		result Result
		want   uint32
	}{
		{name: "caught up", result: Result{LastLedger: 1000, LatestLedger: 1000}, want: 0},
		{name: "one ledger behind", result: Result{LastLedger: 999, LatestLedger: 1000}, want: 1},
		{name: "half the retention window behind", result: Result{LastLedger: 1000, LatestLedger: 61_480}, want: 60_480},
		// Indexing cannot be ahead of the tip, but a stale LatestLedger from
		// an earlier response must not produce a huge negative-turned-enormous
		// number.
		{name: "ahead of the tip reads as caught up", result: Result{LastLedger: 1001, LatestLedger: 1000}, want: 0},
		{name: "an unset last ledger", result: Result{LastLedger: -1, LatestLedger: 1000}, want: 0},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.result.LagLedgers())
		})
	}
}

// ---------------------------------------------------------------------------
// Per-vault isolation
// ---------------------------------------------------------------------------

func Test_Indexer_Run_vaultsAreIndexedIndependently(t *testing.T) {
	// Tenancy itself is structural — each tenant's rows live in its own
	// schema — so what is worth testing here is that two *vaults* sharing a
	// schema keep separate cursors and separate health. One degraded vault
	// must not drag the other down.
	ctx, models, _ := newIndexerTest(t, &fakeRPC{})

	healthyRPC := &fakeRPC{oldestLedger: 900, latestLedger: 1100, events: lifecycleEvents()}
	healthy, err := NewIndexer(healthyRPC, models, vaultContractID, 0)
	require.NoError(t, err)

	// A second vault, whose history has aged out entirely.
	const secondVault = "CBSZNLGKNYH456ZCGA2XLKSOHKFM43GXYMTYS6QPIB4VD3D4G3IHKRSX"
	degradedRPC := &fakeRPC{oldestLedger: 100_000, latestLedger: 130_000}
	degraded, err := NewIndexer(degradedRPC, models, secondVault, 1_000)
	require.NoError(t, err)

	healthyResult, err := healthy.Run(ctx)
	require.NoError(t, err)
	degradedResult, err := degraded.Run(ctx)
	require.NoError(t, err)

	assert.False(t, healthyResult.Degraded)
	assert.True(t, degradedResult.Degraded)

	healthyState, err := models.Vault.GetIndexerState(ctx, models.DBConnectionPool, vaultContractID)
	require.NoError(t, err)
	assert.Equal(t, data.VaultIndexerStatusHealthy, healthyState.Status)

	degradedState, err := models.Vault.GetIndexerState(ctx, models.DBConnectionPool, secondVault)
	require.NoError(t, err)
	assert.Equal(t, data.VaultIndexerStatusDegraded, degradedState.Status)

	// The healthy vault's allocations must not be attributed to the other.
	count, err := models.Vault.CountAllocations(ctx, models.DBConnectionPool,
		data.VaultAllocationFilter{ContractID: secondVault})
	require.NoError(t, err)
	assert.Zero(t, count)
}
