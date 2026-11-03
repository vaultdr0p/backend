package vault

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	hProtocol "github.com/stellar/go-stellar-sdk/protocols/horizon"
	protocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/stellar/stellar-disbursement-platform-backend/db"
	"github.com/stellar/stellar-disbursement-platform-backend/db/dbtest"
	"github.com/stellar/stellar-disbursement-platform-backend/internal/data"
	"github.com/stellar/stellar-disbursement-platform-backend/internal/stellar"
	stellarMocks "github.com/stellar/stellar-disbursement-platform-backend/internal/stellar/mocks"
)

const testNetworkPassphrase = "Test SDF Network ; September 2015"

// fakeAccountFetcher stands in for Horizon's account lookup, which supplies
// the signer's sequence number — the one part of building an envelope that
// only the network knows.
type fakeAccountFetcher struct {
	sequence  int64
	err       error
	requested []string
}

func (f *fakeAccountFetcher) AccountDetail(request horizonclient.AccountRequest) (hProtocol.Account, error) {
	f.requested = append(f.requested, request.AccountID)
	if f.err != nil {
		return hProtocol.Account{}, f.err
	}
	return hProtocol.Account{
		AccountID: request.AccountID,
		Sequence:  f.sequence,
	}, nil
}

// successfulSimulation is a minimal simulation response: enough resource data
// for the envelope to be rebuilt, and no auth entries.
func successfulSimulation(t *testing.T) *stellar.SimulationResult {
	t.Helper()

	// An empty-but-valid SorobanTransactionData, so the fold-back path runs
	// rather than being skipped.
	transactionData, err := xdrSorobanTransactionDataBase64()
	require.NoError(t, err)

	return &stellar.SimulationResult{
		Response: protocol.SimulateTransactionResponse{
			TransactionDataXDR: transactionData,
			MinResourceFee:     12_345,
			LatestLedger:       1000,
		},
	}
}

func newBuilderTest(t *testing.T, deadline time.Time) (context.Context, *stellarMocks.MockRPCClient, *fakeAccountFetcher, *TransactionBuilder) {
	t.Helper()

	dbt := dbtest.Open(t)
	t.Cleanup(func() { dbt.Close() })

	dbConnectionPool, err := db.OpenDBConnectionPool(dbt.DSN)
	require.NoError(t, err)
	t.Cleanup(func() { dbConnectionPool.Close() })

	models, err := data.NewModels(dbConnectionPool)
	require.NoError(t, err)

	ctx := context.Background()
	require.NoError(t, models.Vault.UpsertState(ctx, dbConnectionPool, data.VaultState{
		ContractID:     vaultContractID,
		AdminAddress:   adminAddress,
		TokenAddress:   tokenContractID,
		Deadline:       deadline,
		TotalAllocated: data.NewVaultAmount(10_000_000),
		TotalClaimed:   data.NewVaultAmount(0),
		Balance:        data.NewVaultAmount(60_000_000),
		SnapshotLedger: 1000,
	}))

	rpcClient := stellarMocks.NewMockRPCClient(t)
	horizonClient := &fakeAccountFetcher{sequence: 42}

	builder, err := NewTransactionBuilder(rpcClient, horizonClient, models, vaultContractID, testNetworkPassphrase)
	require.NoError(t, err)

	return ctx, rpcClient, horizonClient, builder
}

func Test_NewTransactionBuilder_requiresItsDependencies(t *testing.T) {
	rpcClient := &stellarMocks.MockRPCClient{}
	horizonClient := &fakeAccountFetcher{}
	models := &data.Models{}

	testCases := []struct {
		name              string
		rpcClient         stellar.RPCClient
		horizonClient     AccountFetcher
		models            *data.Models
		contractID        string
		networkPassphrase string
		wantErr           string
	}{
		{name: "no rpc client", horizonClient: horizonClient, models: models, contractID: vaultContractID, networkPassphrase: testNetworkPassphrase, wantErr: "rpcClient is required"},
		{name: "no horizon client", rpcClient: rpcClient, models: models, contractID: vaultContractID, networkPassphrase: testNetworkPassphrase, wantErr: "horizonClient is required"},
		{name: "no models", rpcClient: rpcClient, horizonClient: horizonClient, contractID: vaultContractID, networkPassphrase: testNetworkPassphrase, wantErr: "models is required"},
		{name: "no contract id", rpcClient: rpcClient, horizonClient: horizonClient, models: models, networkPassphrase: testNetworkPassphrase, wantErr: "contractID is required"},
		{name: "no network passphrase", rpcClient: rpcClient, horizonClient: horizonClient, models: models, contractID: vaultContractID, wantErr: "networkPassphrase is required"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			builder, err := NewTransactionBuilder(tc.rpcClient, tc.horizonClient, tc.models, tc.contractID, tc.networkPassphrase)

			assert.ErrorContains(t, err, tc.wantErr)
			assert.Nil(t, builder)
		})
	}
}

func Test_TransactionBuilder_Build_validation(t *testing.T) {
	// Every case here is rejected before any network call, which is the point:
	// these are the common mistakes, and a field-level error beats a
	// simulation round trip ending in an opaque host error.
	amount := data.NewVaultAmount(10_000_000)
	zero := data.NewVaultAmount(0)
	negative := data.NewVaultAmount(-1)

	testCases := []struct {
		name      string
		request   BuildRequest
		wantField string
		wantMsg   string
	}{
		{
			name:      "an unknown operation",
			request:   BuildRequest{Operation: "destroy"},
			wantField: "operation",
			wantMsg:   "fund, register, cancel, withdraw",
		},
		{
			// claim is deliberately absent: it is authorised by the recipient,
			// not the admin, so SDP has no role in building it.
			name:      "claim is not offered",
			request:   BuildRequest{Operation: "claim", Recipient: recipientAddress},
			wantField: "operation",
			wantMsg:   "fund, register, cancel, withdraw",
		},
		{
			name:      "a contract address cannot sign",
			request:   BuildRequest{Operation: OperationFund, Source: tokenContractID, Amount: &amount},
			wantField: "source",
			wantMsg:   "account address (G...)",
		},
		{
			// A secret key in a request is a credential leak, not a typo, so
			// it gets its own pointed message.
			name:      "a secret key as the source",
			request:   BuildRequest{Operation: OperationFund, Source: "SBFGFF27Y64ZUGFAIG5AMJGQODZZKV2YQKAVUUA4HAZQCM5V5LGEENW4", Amount: &amount},
			wantField: "source",
			wantMsg:   "never a secret key",
		},
		{
			name:      "register with no recipient",
			request:   BuildRequest{Operation: OperationRegister, Amount: &amount},
			wantField: "recipient",
			wantMsg:   "recipient is required",
		},
		{
			name:      "register with a malformed recipient",
			request:   BuildRequest{Operation: OperationRegister, Recipient: "nope", Amount: &amount},
			wantField: "recipient",
			wantMsg:   "Stellar account",
		},
		{
			name:      "cancel with no recipient",
			request:   BuildRequest{Operation: OperationCancel},
			wantField: "recipient",
			wantMsg:   "recipient is required",
		},
		{
			name:      "withdraw with no destination",
			request:   BuildRequest{Operation: OperationWithdraw, Amount: &amount},
			wantField: "destination",
			wantMsg:   "destination is required",
		},
		{
			name:      "fund with no amount",
			request:   BuildRequest{Operation: OperationFund},
			wantField: "amount",
			wantMsg:   "amount is required",
		},
		{
			name:      "register with no amount",
			request:   BuildRequest{Operation: OperationRegister, Recipient: recipientAddress},
			wantField: "amount",
			wantMsg:   "amount is required",
		},
		{
			name:      "a zero amount",
			request:   BuildRequest{Operation: OperationFund, Amount: &zero},
			wantField: "amount",
			wantMsg:   "greater than zero",
		},
		{
			name:      "a negative amount",
			request:   BuildRequest{Operation: OperationFund, Amount: &negative},
			wantField: "amount",
			wantMsg:   "greater than zero",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, _, horizonClient, builder := newBuilderTest(t, time.Now().Add(time.Hour))

			response, err := builder.Build(ctx, tc.request)

			require.Error(t, err)
			var validationErr *ValidationError
			require.ErrorAs(t, err, &validationErr)
			assert.Equal(t, tc.wantField, validationErr.Field)
			assert.Contains(t, validationErr.Message, tc.wantMsg)
			assert.Nil(t, response)

			// Nothing may have reached the network.
			assert.Empty(t, horizonClient.requested, "validation must happen before any network call")
		})
	}
}

func Test_TransactionBuilder_Build_defaultsTheSourceToTheAdmin(t *testing.T) {
	// Every operation offered here is admin-authorised, so a client that
	// omits the source gets the admin rather than an error.
	ctx, rpcClient, horizonClient, builder := newBuilderTest(t, time.Now().Add(time.Hour))
	rpcClient.On("SimulateTransaction", mock.Anything, mock.Anything).
		Return(successfulSimulation(t), nil).Once()

	amount := data.NewVaultAmount(10_000_000)
	response, err := builder.Build(ctx, BuildRequest{Operation: OperationFund, Amount: &amount})
	require.NoError(t, err)

	assert.Equal(t, []string{adminAddress}, horizonClient.requested)
	assert.Equal(t, adminAddress, response.Summary["signer"])
}

func Test_TransactionBuilder_Build_producesASignableEnvelope(t *testing.T) {
	ctx, rpcClient, _, builder := newBuilderTest(t, time.Now().Add(time.Hour))
	rpcClient.On("SimulateTransaction", mock.Anything, mock.Anything).
		Return(successfulSimulation(t), nil).Once()

	amount := data.NewVaultAmount(25_000_000)
	response, err := builder.Build(ctx, BuildRequest{
		Operation: OperationRegister,
		Recipient: recipientAddress,
		Amount:    &amount,
	})
	require.NoError(t, err)

	assert.Equal(t, OperationRegister, response.Operation)
	assert.Equal(t, testNetworkPassphrase, response.NetworkPassphrase)
	assert.Equal(t, "12345", response.MinResourceFee)
	require.NotEmpty(t, response.XDR)

	// The envelope must actually parse, invoke the right contract function,
	// and carry no signatures — the client supplies the only one.
	parsed, err := txnbuild.TransactionFromXDR(response.XDR)
	require.NoError(t, err)
	tx, ok := parsed.Transaction()
	require.True(t, ok, "the envelope must be a plain transaction")

	assert.Empty(t, tx.Signatures(), "the backend must never sign a vault envelope")
	assert.Equal(t, adminAddress, tx.SourceAccount().AccountID)

	operations := tx.Operations()
	require.Len(t, operations, 1)
	invoke, ok := operations[0].(*txnbuild.InvokeHostFunction)
	require.True(t, ok)
	require.NotNil(t, invoke.HostFunction.InvokeContract)
	assert.Equal(t, "register", string(invoke.HostFunction.InvokeContract.FunctionName))
	assert.Len(t, invoke.HostFunction.InvokeContract.Args, 2, "register takes a recipient and an amount")
}

func Test_TransactionBuilder_Build_summaryDescribesWhatIsEncoded(t *testing.T) {
	// The summary is built from the same values as the envelope, so what the
	// user is shown cannot drift from what they sign — which is the whole
	// point of a confirmation screen.
	deadline := time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC)
	ctx, rpcClient, _, builder := newBuilderTest(t, deadline)

	amount := data.NewVaultAmount(25_000_000)

	testCases := []struct {
		name          string
		request       BuildRequest
		wantKeys      []string
		wantEffectHas string
	}{
		{
			name:          "fund",
			request:       BuildRequest{Operation: OperationFund, Amount: &amount},
			wantKeys:      []string{"amount", "effect"},
			wantEffectHas: "into the vault",
		},
		{
			name:          "register",
			request:       BuildRequest{Operation: OperationRegister, Recipient: recipientAddress, Amount: &amount},
			wantKeys:      []string{"recipient", "amount", "effect"},
			wantEffectHas: "claimable until the deadline",
		},
		{
			name:          "cancel",
			request:       BuildRequest{Operation: OperationCancel, Recipient: recipientAddress},
			wantKeys:      []string{"recipient", "effect"},
			wantEffectHas: "Moves no tokens",
		},
		{
			name:          "withdraw",
			request:       BuildRequest{Operation: OperationWithdraw, Destination: adminAddress, Amount: &amount},
			wantKeys:      []string{"destination", "amount", "effect"},
			wantEffectHas: "unallocated vault funds",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			rpcClient.On("SimulateTransaction", mock.Anything, mock.Anything).
				Return(successfulSimulation(t), nil).Once()

			response, err := builder.Build(ctx, tc.request)
			require.NoError(t, err)

			for _, key := range append(tc.wantKeys, "operation", "vault", "token", "signer", "network", "deadline", "is_expired") {
				assert.Contains(t, response.Summary, key, "summary must carry %s", key)
			}
			assert.Contains(t, response.Summary["effect"], tc.wantEffectHas)
			assert.Equal(t, string(tc.request.Operation), response.Summary["operation"])
			assert.Equal(t, vaultContractID, response.Summary["vault"])
			assert.Equal(t, "false", response.Summary["is_expired"])

			// The summary must never contain a secret, and the network is
			// named so a wallet can refuse the wrong one.
			assert.Equal(t, testNetworkPassphrase, response.Summary["network"])
		})
	}
}

func Test_TransactionBuilder_Build_mapsContractErrors(t *testing.T) {
	// A contract rejection must arrive as the contract's own typed error, so
	// the operator sees which rule they hit rather than a host error string.
	testCases := []struct {
		name     string
		rpcError string
		wantCode int
		wantName string
		wantMsg  string
	}{
		{
			name:     "insufficient funds",
			rpcError: "host invocation failed: HostError: Error(Contract, #9)",
			wantCode: 9, wantName: "InsufficientFunds", wantMsg: "unallocated funds",
		},
		{
			name:     "deadline passed",
			rpcError: "HostError: Error(Contract, #10)",
			wantCode: 10, wantName: "DeadlinePassed", wantMsg: "deadline has passed",
		},
		{
			name:     "deadline not reached",
			rpcError: "Error(Contract, #11)",
			wantCode: 11, wantName: "DeadlineNotReached", wantMsg: "has not passed yet",
		},
		{
			name:     "allocation exists",
			rpcError: "Error(Contract, #5)",
			wantCode: 5, wantName: "AllocationExists", wantMsg: "already has an allocation",
		},
		{
			// Whitespace varies between host versions, so the pattern must
			// tolerate it rather than matching one exact rendering.
			name:     "no space after the comma",
			rpcError: "Error(Contract,#7)",
			wantCode: 7, wantName: "AlreadyClaimed", wantMsg: "already been claimed",
		},
		{
			// A code this build does not know must still be reported as a
			// contract error, not swallowed into a generic failure.
			name:     "an unrecognised contract code",
			rpcError: "Error(Contract, #99)",
			wantCode: 99, wantName: "Unknown", wantMsg: "error 99",
		},
		{
			name:     "a failure that is not a contract error",
			rpcError: "connection reset by peer",
			wantCode: 0, wantMsg: "rejected this operation during simulation",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, rpcClient, _, builder := newBuilderTest(t, time.Now().Add(time.Hour))
			rpcClient.On("SimulateTransaction", mock.Anything, mock.Anything).
				Return(nil, &stellar.SimulationError{
					Type: stellar.SimulationErrorTypeContractExecution,
					Err:  errors.New(tc.rpcError),
					Response: &protocol.SimulateTransactionResponse{
						Error: tc.rpcError,
					},
				}).Once()

			amount := data.NewVaultAmount(10_000_000)
			response, err := builder.Build(ctx, BuildRequest{Operation: OperationFund, Amount: &amount})

			require.Error(t, err)
			var contractErr *ContractError
			require.ErrorAs(t, err, &contractErr)
			assert.Equal(t, tc.wantCode, contractErr.Code)
			if tc.wantName != "" {
				assert.Equal(t, tc.wantName, contractErr.Name)
			}
			assert.Contains(t, contractErr.Message, tc.wantMsg)
			assert.Equal(t, tc.rpcError, contractErr.Raw)
			assert.Nil(t, response)
		})
	}
}

func Test_TransactionBuilder_Build_unknownSourceAccountIsAClientError(t *testing.T) {
	// An unfunded or mistyped source is the operator's mistake, not a server
	// fault, so it must not surface as a 500.
	ctx, _, horizonClient, builder := newBuilderTest(t, time.Now().Add(time.Hour))
	horizonClient.err = errors.New("horizon: problem: not_found")

	amount := data.NewVaultAmount(10_000_000)
	response, err := builder.Build(ctx, BuildRequest{Operation: OperationFund, Amount: &amount})

	require.Error(t, err)
	var validationErr *ValidationError
	require.ErrorAs(t, err, &validationErr)
	assert.Equal(t, "source", validationErr.Field)
	assert.Contains(t, validationErr.Message, "not found on the network")
	assert.Nil(t, response)
}

func Test_TransactionBuilder_Build_requiresAnIndexedVault(t *testing.T) {
	// Without the cached state there is no admin address to default to and no
	// token to describe, so the build cannot proceed.
	dbt := dbtest.Open(t)
	defer dbt.Close()

	dbConnectionPool, err := db.OpenDBConnectionPool(dbt.DSN)
	require.NoError(t, err)
	defer dbConnectionPool.Close()

	models, err := data.NewModels(dbConnectionPool)
	require.NoError(t, err)

	builder, err := NewTransactionBuilder(&stellarMocks.MockRPCClient{}, &fakeAccountFetcher{}, models, vaultContractID, testNetworkPassphrase)
	require.NoError(t, err)

	amount := data.NewVaultAmount(10_000_000)
	response, err := builder.Build(context.Background(), BuildRequest{Operation: OperationFund, Amount: &amount})

	assert.ErrorIs(t, err, data.ErrRecordNotFound)
	assert.Nil(t, response)
}

func Test_TransactionBuilder_Build_expiredVaultIsReportedInTheSummary(t *testing.T) {
	// Cancelling is only valid after the deadline, so the summary has to say
	// which side of it the vault is on — the contract makes exactly one of
	// claim and cancel valid at any instant.
	ctx, rpcClient, _, builder := newBuilderTest(t, time.Now().Add(-time.Hour))
	rpcClient.On("SimulateTransaction", mock.Anything, mock.Anything).
		Return(successfulSimulation(t), nil).Once()

	response, err := builder.Build(ctx, BuildRequest{Operation: OperationCancel, Recipient: recipientAddress})
	require.NoError(t, err)

	assert.Equal(t, "true", response.Summary["is_expired"])
}

func Test_amountScVal_handlesTheFullI128Range(t *testing.T) {
	// The reason VaultAmount exists: these values do not fit in any Go
	// integer, so the hi/lo split has to go via big.Int.
	testCases := []struct {
		name   string
		amount string
	}{
		{name: "one stroop", amount: "1"},
		{name: "one unit", amount: "10000000"},
		{name: "just inside int64", amount: "9223372036854775807"},
		{name: "just past int64", amount: "9223372036854775808"},
		{name: "i128 max", amount: "170141183460469231731687303715884105727"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			amount, err := data.ParseVaultAmount(tc.amount)
			require.NoError(t, err)

			scVal, err := amountScVal(amount)
			require.NoError(t, err)

			// Round-trip through the XDR type to prove the split is exact.
			parts, ok := scVal.GetI128()
			require.True(t, ok)

			roundTripped := i128PartsToString(parts)
			assert.Equal(t, tc.amount, roundTripped)
		})
	}
}

func Test_addressScVal(t *testing.T) {
	testCases := []struct {
		name    string
		address string
		wantErr string
	}{
		{name: "an account", address: adminAddress},
		{name: "a contract", address: tokenContractID},
		{name: "nonsense", address: "nope", wantErr: "not a Stellar address"},
		{name: "a secret key", address: "SBFGFF27Y64ZUGFAIG5AMJGQODZZKV2YQKAVUUA4HAZQCM5V5LGEENW4", wantErr: "not a Stellar address"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			scVal, err := addressScVal(tc.address)

			if tc.wantErr != "" {
				assert.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)

			address, ok := scVal.GetAddress()
			require.True(t, ok)
			assert.Equal(t, tc.address, mustAddressString(t, address))
		})
	}
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// xdrSorobanTransactionDataBase64 produces a valid, empty SorobanTransactionData
// so the simulation fold-back path runs rather than being skipped.
func xdrSorobanTransactionDataBase64() (string, error) {
	return xdr.MarshalBase64(xdr.SorobanTransactionData{
		Resources:   xdr.SorobanResources{},
		ResourceFee: 100,
	})
}

// i128PartsToString reassembles the hi/lo halves, to prove the split in
// amountScVal is exact across the whole i128 range.
func i128PartsToString(parts xdr.Int128Parts) string {
	value := new(big.Int).Lsh(big.NewInt(int64(parts.Hi)), 64)
	value.Or(value, new(big.Int).SetUint64(uint64(parts.Lo)))
	return value.String()
}

// mustAddressString renders an ScAddress back to strkey form.
func mustAddressString(t *testing.T, address xdr.ScAddress) string {
	t.Helper()

	encoded, err := address.String()
	require.NoError(t, err)
	return encoded
}
