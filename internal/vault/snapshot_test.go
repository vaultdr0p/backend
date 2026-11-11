package vault

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	protocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/support/log"
	"github.com/stellar/go-stellar-sdk/xdr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stellar/stellar-disbursement-platform-backend/internal/data"
	"github.com/stellar/stellar-disbursement-platform-backend/internal/stellar"
)

// ---------------------------------------------------------------------------
// Fake contract state behind SimulateTransaction
// ---------------------------------------------------------------------------

// fakeContract is the vault and token state the fake RPC simulates against.
type fakeContract struct {
	admin     string
	token     string
	deadline  uint64
	allocated *big.Int
	claimed   *big.Int
	balance   *big.Int
	ledger    uint32

	// ledgers, when set, is consumed one per simulation and overrides ledger:
	// it scripts RPC's latest ledger moving between calls.
	ledgers []uint32
	// failOn makes the named function's simulation fail.
	failOn map[string]*stellar.SimulationError
	// override replaces the named function's return value.
	override map[string]xdr.ScVal
	// beforeSimulate runs before every simulation, to script races.
	beforeSimulate func()
}

func defaultContract() *fakeContract {
	return &fakeContract{
		admin:     adminAddress,
		token:     tokenContractID,
		deadline:  uint64(testDeadline),
		allocated: big.NewInt(30_000_000),
		claimed:   big.NewInt(5_000_000),
		balance:   big.NewInt(100_000_000),
		ledger:    1105,
	}
}

// simulatedCall is one decoded simulation request.
type simulatedCall struct {
	contractID string
	function   string
	args       xdr.ScVec
	source     string
	signatures int
}

func (f *fakeRPC) SimulateTransaction(_ context.Context, request protocol.SimulateTransactionRequest) (*stellar.SimulationResult, *stellar.SimulationError) {
	call := decodeSimulation(request.Transaction)
	f.simulations = append(f.simulations, call)

	if f.contract == nil {
		return nil, stellar.NewSimulationError(errors.New("dial tcp: connection refused"), nil)
	}
	c := f.contract
	if c.beforeSimulate != nil {
		c.beforeSimulate()
	}
	if simErr, ok := c.failOn[call.function]; ok {
		return nil, simErr
	}

	ledger := c.ledger
	if len(c.ledgers) > 0 {
		ledger, c.ledgers = c.ledgers[0], c.ledgers[1:]
	}

	var value xdr.ScVal
	if override, ok := c.override[call.function]; ok {
		value = override
	} else {
		value = c.valueOf(call)
	}

	encoded, err := xdr.MarshalBase64(value)
	if err != nil {
		panic(err)
	}
	return &stellar.SimulationResult{Response: protocol.SimulateTransactionResponse{
		LatestLedger: ledger,
		Results:      []protocol.SimulateHostFunctionResult{{ReturnValueXDR: &encoded}},
	}}, nil
}

func (c *fakeContract) valueOf(call simulatedCall) xdr.ScVal {
	switch {
	case call.contractID == vaultContractID && call.function == "get_admin":
		return mustAddressScVal(c.admin)
	case call.contractID == vaultContractID && call.function == "get_token":
		return mustAddressScVal(c.token)
	case call.contractID == vaultContractID && call.function == "get_deadline":
		v := xdr.Uint64(c.deadline)
		return xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &v}
	case call.contractID == vaultContractID && call.function == "total_allocated":
		return i128ScVal(c.allocated)
	case call.contractID == vaultContractID && call.function == "total_claimed":
		return i128ScVal(c.claimed)
	case call.contractID == c.token && call.function == "balance":
		return i128ScVal(c.balance)
	}
	panic(fmt.Sprintf("unexpected simulation %s.%s", call.contractID, call.function))
}

func decodeSimulation(envelopeXDR string) simulatedCall {
	var envelope xdr.TransactionEnvelope
	if err := xdr.SafeUnmarshalBase64(envelopeXDR, &envelope); err != nil {
		panic(err)
	}
	ops := envelope.Operations()
	invoke := ops[0].Body.MustInvokeHostFunctionOp().HostFunction.MustInvokeContract()
	contractID, err := invoke.ContractAddress.String()
	if err != nil {
		panic(err)
	}
	source := envelope.SourceAccount().ToAccountId()
	return simulatedCall{
		contractID: contractID,
		function:   string(invoke.FunctionName),
		args:       invoke.Args,
		source:     source.Address(),
		signatures: len(envelope.Signatures()),
	}
}

func mustAddressScVal(address string) xdr.ScVal {
	v, err := addressScVal(address)
	if err != nil {
		panic(err)
	}
	return v
}

func i128ScVal(n *big.Int) xdr.ScVal {
	amount, err := data.ParseVaultAmount(n.String())
	if err != nil {
		panic(err)
	}
	v, err := amountScVal(amount)
	if err != nil {
		panic(err)
	}
	return v
}

func contractError(code int) *stellar.SimulationError {
	msg := fmt.Sprintf("HostError: Error(Contract, #%d)", code)
	return stellar.NewSimulationError(errors.New(msg), &protocol.SimulateTransactionResponse{Error: msg})
}

func snapshotCategory(t *testing.T, err error) data.SnapshotErrorCategory {
	t.Helper()
	var snapErr *SnapshotError
	require.ErrorAs(t, err, &snapErr)
	return snapErr.Category
}

// ---------------------------------------------------------------------------
// ReadContractSnapshot
// ---------------------------------------------------------------------------

func Test_ReadContractSnapshot_readsEveryFieldAtOneLedger(t *testing.T) {
	rpcClient := &fakeRPC{contract: defaultContract()}

	snapshot, err := ReadContractSnapshot(context.Background(), rpcClient, vaultContractID)
	require.NoError(t, err)

	assert.Equal(t, adminAddress, snapshot.Admin)
	assert.Equal(t, tokenContractID, snapshot.Token)
	assert.Equal(t, time.Unix(testDeadline, 0).UTC(), snapshot.Deadline)
	assert.Equal(t, "30000000", snapshot.TotalAllocated.String())
	assert.Equal(t, "5000000", snapshot.TotalClaimed.String())
	assert.Equal(t, "100000000", snapshot.Balance.String())
	assert.Equal(t, uint32(1105), snapshot.Ledger)

	functions := []string{}
	for _, call := range rpcClient.simulations {
		functions = append(functions, call.contractID[:4]+"."+call.function)
		assert.Equal(t, snapshotSourceAccount, call.source, "the throwaway placeholder is the only source")
		assert.Zero(t, call.signatures, "nothing is signed")
	}
	assert.Equal(t, []string{
		vaultContractID[:4] + ".get_admin", vaultContractID[:4] + ".get_token", vaultContractID[:4] + ".get_deadline",
		vaultContractID[:4] + ".total_allocated", vaultContractID[:4] + ".total_claimed",
		tokenContractID[:4] + ".balance",
	}, functions)

	// The balance is the token's balance *of the vault*.
	balanceCall := rpcClient.simulations[5]
	require.Len(t, balanceCall.args, 1)
	holder, err := balanceCall.args[0].MustAddress().String()
	require.NoError(t, err)
	assert.Equal(t, vaultContractID, holder)
}

func Test_snapshotSourceAccount_isTheAllZeroPublicKey(t *testing.T) {
	// A public placeholder with no usable secret: the all-zero ed25519 key.
	encoded, err := strkey.Encode(strkey.VersionByteAccountID, make([]byte, 32))
	require.NoError(t, err)
	assert.Equal(t, encoded, snapshotSourceAccount)
}

func Test_ReadContractSnapshot_decodesI128BeyondInt64(t *testing.T) {
	contract := defaultContract()
	huge, ok := new(big.Int).SetString("170141183460469231731687303715884105727", 10) // i128 max
	require.True(t, ok)
	contract.balance = huge

	snapshot, err := ReadContractSnapshot(context.Background(), &fakeRPC{contract: contract}, vaultContractID)
	require.NoError(t, err)
	assert.Equal(t, huge.String(), snapshot.Balance.String())
}

func Test_ReadContractSnapshot_retriesWhenTheLedgerMoves(t *testing.T) {
	contract := defaultContract()
	// First set: ledger moves on the third call. Second set: all at 1107.
	contract.ledgers = []uint32{1105, 1105, 1106, 1107, 1107, 1107, 1107, 1107, 1107}
	rpcClient := &fakeRPC{contract: contract}

	snapshot, err := ReadContractSnapshot(context.Background(), rpcClient, vaultContractID)
	require.NoError(t, err)
	assert.Equal(t, uint32(1107), snapshot.Ledger)
	assert.Len(t, rpcClient.simulations, 3+6, "the whole set is re-read, not just the call that moved")
}

func Test_ReadContractSnapshot_failures(t *testing.T) {
	wrongType := xdr.Uint64(7)
	negative := i128ScVal(big.NewInt(-1))

	testCases := []struct {
		name     string
		contract func() *fakeContract
		want     data.SnapshotErrorCategory
	}{
		{
			name:     "RPC unreachable",
			contract: func() *fakeContract { return nil },
			want:     data.SnapshotErrorRPCUnreachable,
		},
		{
			name: "RPC fails on the last read only (partial data)",
			contract: func() *fakeContract {
				c := defaultContract()
				c.failOn = map[string]*stellar.SimulationError{
					"balance": stellar.NewSimulationError(errors.New("timeout"), nil),
				}
				return c
			},
			want: data.SnapshotErrorRPCUnreachable,
		},
		{
			name: "one read fails in simulation",
			contract: func() *fakeContract {
				c := defaultContract()
				c.failOn = map[string]*stellar.SimulationError{"total_claimed": contractError(12)}
				return c
			},
			want: data.SnapshotErrorSimulationFailed,
		},
		{
			name: "contract not initialized",
			contract: func() *fakeContract {
				c := defaultContract()
				c.failOn = map[string]*stellar.SimulationError{"get_admin": contractError(2)}
				return c
			},
			want: data.SnapshotErrorContractNotInitialized,
		},
		{
			name: "the ledger moves on every attempt",
			contract: func() *fakeContract {
				c := defaultContract()
				for i := uint32(0); i < 100; i++ {
					c.ledgers = append(c.ledgers, 2000+i)
				}
				return c
			},
			want: data.SnapshotErrorInconsistentLedgers,
		},
		{
			name: "a value of the wrong type",
			contract: func() *fakeContract {
				c := defaultContract()
				c.override = map[string]xdr.ScVal{"get_admin": {Type: xdr.ScValTypeScvU64, U64: &wrongType}}
				return c
			},
			want: data.SnapshotErrorInvalidResponse,
		},
		{
			name: "a negative amount",
			contract: func() *fakeContract {
				c := defaultContract()
				c.override = map[string]xdr.ScVal{"total_allocated": negative}
				return c
			},
			want: data.SnapshotErrorInvalidResponse,
		},
		{
			name: "no latest ledger in the response",
			contract: func() *fakeContract {
				c := defaultContract()
				c.ledger = 0
				return c
			},
			want: data.SnapshotErrorInvalidResponse,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			snapshot, err := ReadContractSnapshot(context.Background(), &fakeRPC{contract: tc.contract()}, vaultContractID)
			assert.Nil(t, snapshot, "no partial snapshot is ever returned")
			assert.Equal(t, tc.want, snapshotCategory(t, err))
		})
	}
}

func Test_SnapshotBackoff(t *testing.T) {
	for failures, want := range map[int]time.Duration{
		0: 10 * time.Second, 1: 10 * time.Second, 2: 20 * time.Second, 3: 40 * time.Second,
		5: 160 * time.Second, 6: 5 * time.Minute, 50: 5 * time.Minute,
	} {
		assert.Equal(t, want, SnapshotBackoff(failures), "after %d failures", failures)
	}
}

// ---------------------------------------------------------------------------
// The indexer's snapshot step
// ---------------------------------------------------------------------------

// fixedClock makes the backoff deterministic.
type fixedClock struct{ now time.Time }

func (c *fixedClock) Now() time.Time { return c.now }

func newSnapshotIndexerTest(t *testing.T, rpcClient *fakeRPC) (context.Context, *data.Models, *Indexer, *fixedClock) {
	t.Helper()
	ctx, models, indexer := newIndexerTest(t, rpcClient)
	clock := &fixedClock{now: time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)}
	indexer.now = clock.Now
	return ctx, models, indexer, clock
}

func Test_Indexer_Run_snapshotsAVaultCreatedBeforeTheWindow(t *testing.T) {
	// The live failure: the window holds no `initialized` event.
	rpcClient := &fakeRPC{oldestLedger: 900, latestLedger: 1100, contract: defaultContract()}
	ctx, models, indexer, _ := newSnapshotIndexerTest(t, rpcClient)

	result, err := indexer.Run(ctx)
	require.NoError(t, err)
	assert.Equal(t, SnapshotTaken, result.Snapshot)
	assert.True(t, result.Degraded)

	state, err := models.Vault.GetState(ctx, models.DBConnectionPool, vaultContractID)
	require.NoError(t, err)
	assert.Equal(t, data.VaultStateSourceSnapshot, state.Source)
	require.NotNil(t, state.BootstrapLedger)
	assert.Equal(t, int32(1105), *state.BootstrapLedger)
	assert.Equal(t, "100000000", state.Balance.String())
	assert.Equal(t, "30000000", state.TotalAllocated.String())
	assert.Equal(t, adminAddress, state.AdminAddress)

	indexerState, err := models.Vault.GetIndexerState(ctx, models.DBConnectionPool, vaultContractID)
	require.NoError(t, err)
	assert.Equal(t, data.VaultIndexerStatusDegraded, indexerState.Status)
	assert.Nil(t, indexerState.GapFromLedger, "the creation ledger is unknown, and not invented")
	require.NotNil(t, indexerState.GapToLedger)
	assert.Equal(t, int32(899), *indexerState.GapToLedger, "history ends just before the first indexed ledger")
	require.NotNil(t, indexerState.DegradedReason)
	assert.Contains(t, *indexerState.DegradedReason, "cannot enumerate")
	assert.NotContains(t, *indexerState.DegradedReason, "upper bound", "the start ledger was recorded")

	allocations, err := models.Vault.CountAllocations(ctx, models.DBConnectionPool, data.VaultAllocationFilter{ContractID: vaultContractID})
	require.NoError(t, err)
	assert.Zero(t, allocations, "recipients are never invented from a snapshot")
}

func Test_Indexer_Run_aVaultInitializedInsideTheWindowIsNeverSnapshotted(t *testing.T) {
	rpcClient := &fakeRPC{oldestLedger: 900, latestLedger: 1100, events: lifecycleEvents(), contract: defaultContract()}
	ctx, models, indexer, _ := newSnapshotIndexerTest(t, rpcClient)

	result, err := indexer.Run(ctx)
	require.NoError(t, err)
	assert.Equal(t, SnapshotNotNeeded, result.Snapshot)
	assert.Empty(t, rpcClient.simulations)

	state, err := models.Vault.GetState(ctx, models.DBConnectionPool, vaultContractID)
	require.NoError(t, err)
	assert.Equal(t, data.VaultStateSourceEvents, state.Source)
	assert.False(t, result.Degraded)
}

func Test_Indexer_Run_doesNotSnapshotBeforeReachingTheTip(t *testing.T) {
	// More events than one run reads: an `initialized` event could still be
	// ahead, so a snapshot now would be premature.
	var events []protocol.EventInfo
	for i := uint32(0); i < 30; i++ {
		events = append(events, fundedAt(1001+i, "1", "1"))
	}
	rpcClient := &fakeRPC{oldestLedger: 900, latestLedger: 1100, events: events, contract: defaultContract()}
	ctx, _, indexer, _ := newSnapshotIndexerTest(t, rpcClient)
	indexer.batchLimit = 1

	result, err := indexer.Run(ctx)
	require.NoError(t, err)
	assert.Len(t, rpcClient.requests, 1+maxBatchesPerRun, "the window probe, then the capped batches")
	assert.Equal(t, SnapshotNotNeeded, result.Snapshot)
	assert.Empty(t, rpcClient.simulations)
}

func Test_Indexer_Run_eventsAtOrBeforeTheSnapshotLedgerAreNotAppliedTwice(t *testing.T) {
	// Run 1 scans to 1100 and snapshots at 1105. Events at 1104 and 1105 are
	// already inside that snapshot; 1106 is not. Run 2 reads all three.
	contract := defaultContract()
	rpcClient := &fakeRPC{oldestLedger: 900, latestLedger: 1100, contract: contract}
	ctx, models, indexer, _ := newSnapshotIndexerTest(t, rpcClient)

	_, err := indexer.Run(ctx)
	require.NoError(t, err)

	rpcClient.latestLedger = 1110
	rpcClient.events = []protocol.EventInfo{
		registeredAt(1104, 1, recipientAddress, "10000000", "30000000"),
		settledAt(1105, "claimed", otherRecipient, "5000000"),
		settledAt(1106, "withdrawn", adminAddress, "20000000"),
	}
	result, err := indexer.Run(ctx)
	require.NoError(t, err)
	assert.Equal(t, 3, result.EventsApplied)

	state, err := models.Vault.GetState(ctx, models.DBConnectionPool, vaultContractID)
	require.NoError(t, err)
	assert.Equal(t, "5000000", state.TotalClaimed.String(), "the 1105 claim is already in the snapshot")
	assert.Equal(t, "30000000", state.TotalAllocated.String())
	assert.Equal(t, "80000000", state.Balance.String(), "only the 1106 withdrawal applies")
	assert.Equal(t, int32(1106), state.SnapshotLedger)

	// Allocations are event-derived at every ledger: the 1104 registration
	// is listed even though its total was already in the snapshot.
	allocation, err := models.Vault.GetAllocation(ctx, models.DBConnectionPool, vaultContractID, recipientAddress)
	require.NoError(t, err)
	assert.Equal(t, "10000000", allocation.Amount.String())
}

func Test_Indexer_Run_aLateInitializedEventDoesNotResetTheSnapshot(t *testing.T) {
	rpcClient := &fakeRPC{oldestLedger: 900, latestLedger: 1100, contract: defaultContract()}
	ctx, models, indexer, _ := newSnapshotIndexerTest(t, rpcClient)
	_, err := indexer.Run(ctx)
	require.NoError(t, err)

	// A resync replays from ledger 999 and RPC now serves `initialized`.
	rpcClient.events = []protocol.EventInfo{initializedAt()}
	require.NoError(t, models.Vault.AdvanceCursor(ctx, models.DBConnectionPool, vaultContractID, "", 999))
	_, err = indexer.Run(ctx)
	require.NoError(t, err)

	state, err := models.Vault.GetState(ctx, models.DBConnectionPool, vaultContractID)
	require.NoError(t, err)
	assert.Equal(t, data.VaultStateSourceSnapshot, state.Source)
	assert.Equal(t, "100000000", state.Balance.String())
}

func Test_Indexer_Run_aRowCreatedDuringTheSnapshotWins(t *testing.T) {
	contract := defaultContract()
	rpcClient := &fakeRPC{oldestLedger: 900, latestLedger: 1100, contract: contract}
	ctx, models, indexer, _ := newSnapshotIndexerTest(t, rpcClient)

	seeded := false
	contract.beforeSimulate = func() {
		if seeded {
			return
		}
		seeded = true
		_, err := models.Vault.SeedStateFromEvent(ctx, models.DBConnectionPool, data.VaultState{
			ContractID: vaultContractID, AdminAddress: adminAddress, TokenAddress: tokenContractID,
			Deadline: time.Unix(testDeadline, 0).UTC(), SnapshotLedger: 1000,
		})
		require.NoError(t, err)
	}

	result, err := indexer.Run(ctx)
	require.NoError(t, err)
	assert.Equal(t, SnapshotDiscarded, result.Snapshot)
	assert.False(t, result.Degraded)

	state, err := models.Vault.GetState(ctx, models.DBConnectionPool, vaultContractID)
	require.NoError(t, err)
	assert.Equal(t, data.VaultStateSourceEvents, state.Source)
	indexerState, err := models.Vault.GetIndexerState(ctx, models.DBConnectionPool, vaultContractID)
	require.NoError(t, err)
	assert.Equal(t, data.VaultIndexerStatusHealthy, indexerState.Status, "a discarded snapshot marks nothing")
}

func Test_Indexer_Run_snapshotFailureIsRecordedWithBackoff(t *testing.T) {
	rpcClient := &fakeRPC{oldestLedger: 900, latestLedger: 1100} // no contract: RPC unreachable
	ctx, models, indexer, clock := newSnapshotIndexerTest(t, rpcClient)

	result, err := indexer.Run(ctx)
	require.NoError(t, err, "an RPC failure during the snapshot never fails the run")
	assert.Equal(t, SnapshotFailed, result.Snapshot)

	state, err := models.Vault.GetIndexerState(ctx, models.DBConnectionPool, vaultContractID)
	require.NoError(t, err)
	assert.Equal(t, 1, state.SnapshotAttempts)
	require.NotNil(t, state.SnapshotLastError)
	assert.Equal(t, "rpc_unreachable", *state.SnapshotLastError)
	require.NotNil(t, state.SnapshotNextAttemptAt)
	assert.True(t, clock.now.Add(10*time.Second).Equal(*state.SnapshotNextAttemptAt))
	assert.Equal(t, data.VaultIndexerStatusHealthy, state.Status, "a failed snapshot writes no gap")

	_, err = models.Vault.GetState(ctx, models.DBConnectionPool, vaultContractID)
	assert.ErrorIs(t, err, data.ErrRecordNotFound, "no partial row")

	// Before the backoff elapses: no attempt at all.
	calls := len(rpcClient.simulations)
	clock.now = clock.now.Add(9 * time.Second)
	result, err = indexer.Run(ctx)
	require.NoError(t, err)
	assert.Equal(t, SnapshotDeferred, result.Snapshot)
	assert.Len(t, rpcClient.simulations, calls)

	// After it: a second failure doubles the delay.
	clock.now = clock.now.Add(time.Second)
	_, err = indexer.Run(ctx)
	require.NoError(t, err)
	state, err = models.Vault.GetIndexerState(ctx, models.DBConnectionPool, vaultContractID)
	require.NoError(t, err)
	assert.Equal(t, 2, state.SnapshotAttempts)
	assert.True(t, clock.now.Add(20*time.Second).Equal(*state.SnapshotNextAttemptAt))

	// RPC recovers: the snapshot is taken and the backoff cleared.
	rpcClient.contract = defaultContract()
	clock.now = clock.now.Add(20 * time.Second)
	result, err = indexer.Run(ctx)
	require.NoError(t, err)
	assert.Equal(t, SnapshotTaken, result.Snapshot)
	state, err = models.Vault.GetIndexerState(ctx, models.DBConnectionPool, vaultContractID)
	require.NoError(t, err)
	assert.Zero(t, state.SnapshotAttempts)
	assert.Nil(t, state.SnapshotLastError)
	assert.Nil(t, state.SnapshotNextAttemptAt)
}

func Test_Indexer_Run_anUninitializedContractGetsNoInventedSummary(t *testing.T) {
	contract := defaultContract()
	contract.failOn = map[string]*stellar.SimulationError{"get_admin": contractError(2)}
	rpcClient := &fakeRPC{oldestLedger: 900, latestLedger: 1100, contract: contract}
	ctx, models, indexer, _ := newSnapshotIndexerTest(t, rpcClient)

	result, err := indexer.Run(ctx)
	require.NoError(t, err)
	assert.Equal(t, SnapshotFailed, result.Snapshot)

	_, err = models.Vault.GetState(ctx, models.DBConnectionPool, vaultContractID)
	assert.ErrorIs(t, err, data.ErrRecordNotFound)
	state, err := models.Vault.GetIndexerState(ctx, models.DBConnectionPool, vaultContractID)
	require.NoError(t, err)
	assert.Equal(t, "contract_not_initialized", *state.SnapshotLastError)
}

func Test_Indexer_Run_legacyRowsWithoutAStartLedgerSayTheBoundIsApproximate(t *testing.T) {
	rpcClient := &fakeRPC{oldestLedger: 950, latestLedger: 1100, contract: defaultContract()}
	ctx, models, indexer, _ := newSnapshotIndexerTest(t, rpcClient)

	// Bootstrapped before start_ledger was recorded.
	_, err := models.Vault.BootstrapIndexerState(ctx, models.DBConnectionPool, data.VaultIndexerState{
		ContractID: vaultContractID, LastLedger: 949, Status: data.VaultIndexerStatusHealthy,
	})
	require.NoError(t, err)

	_, err = indexer.Run(ctx)
	require.NoError(t, err)

	state, err := models.Vault.GetIndexerState(ctx, models.DBConnectionPool, vaultContractID)
	require.NoError(t, err)
	assert.Equal(t, int32(949), *state.GapToLedger, "the retention floor stands in")
	assert.Contains(t, *state.DegradedReason, "upper bound")
}

func Test_Indexer_Run_aSummaryUpdateWithNoRowIsLoggedAndCounted(t *testing.T) {
	// Addition B: the update is still a no-op, but no longer a silent one.
	events := []protocol.EventInfo{
		fundedAt(1001, "30000000", "30000000"),
		settledAt(1002, "withdrawn", adminAddress, "1"),
		settledAt(1003, "withdrawn", adminAddress, "1"),
	}
	rpcClient := &fakeRPC{oldestLedger: 900, latestLedger: 1100, events: events}
	ctx, models, indexer, _ := newSnapshotIndexerTest(t, rpcClient)

	logs := log.DefaultLogger.StartTest(log.WarnLevel)
	result, err := indexer.Run(ctx)
	entries := logs()
	require.NoError(t, err)

	assert.Equal(t, map[EventType]int{EventTypeFunded: 1, EventTypeWithdrawn: 2}, result.SummaryUpdatesMissed)
	assert.Equal(t, 3, result.EventsApplied, "the events are still recorded")

	warnings := 0
	for _, entry := range entries {
		if strings.Contains(entry.Message, "found no vault summary row") {
			warnings++
			assert.Contains(t, entry.Message, vaultContractID)
		}
	}
	assert.Equal(t, 3, warnings, "one warning per event, naming the contract and the event type")

	_, err = models.Vault.GetState(ctx, models.DBConnectionPool, vaultContractID)
	assert.ErrorIs(t, err, data.ErrRecordNotFound, "the update behaviour is unchanged: nothing is created")
}

func Test_Indexer_Run_aSummaryUpdateWithARowIsNotCounted(t *testing.T) {
	rpcClient := &fakeRPC{oldestLedger: 900, latestLedger: 1100, events: lifecycleEvents()}
	ctx, _, indexer, _ := newSnapshotIndexerTest(t, rpcClient)

	result, err := indexer.Run(ctx)
	require.NoError(t, err)
	assert.Empty(t, result.SummaryUpdatesMissed)
}
