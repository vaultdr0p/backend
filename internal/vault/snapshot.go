package vault

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"time"

	protocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/stellar/stellar-disbursement-platform-backend/internal/data"
	"github.com/stellar/stellar-disbursement-platform-backend/internal/stellar"
)

// This file reads a vault's summary from contract state, for a vault whose
// `initialized` event is older than the RPC retention window
// (docs/VAULT_DESIGN.md §10). Everything here is read-only: transactions are
// built only to be simulated, and are never signed or submitted.

const (
	// snapshotSourceAccount is the source account on the simulated
	// transactions. Read-only simulation does not load or authorise it, so any
	// valid account id works; this is the all-zero ed25519 public key, a
	// well-known placeholder. There is no secret key for it anywhere in this
	// codebase, and none is needed.
	snapshotSourceAccount = "GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAWHF"

	// snapshotLedgerAttempts is how many times one snapshot attempt re-reads
	// the whole set when RPC's latest ledger moves between calls.
	snapshotLedgerAttempts = 3

	// SnapshotBackoffBase and SnapshotBackoffMax bound the delay between
	// failed snapshot attempts: 10s, 20s, 40s, ... capped at 5 minutes.
	SnapshotBackoffBase = 10 * time.Second
	SnapshotBackoffMax  = 5 * time.Minute

	// contractErrorNotInitialized is the vault contract's NotInitialized
	// discriminant (contracts/disbursement-vault/src/error.rs).
	contractErrorNotInitialized = 2
)

// SnapshotRPCClient is the one RPC method the snapshot reader needs.
type SnapshotRPCClient interface {
	SimulateTransaction(ctx context.Context, request protocol.SimulateTransactionRequest) (*stellar.SimulationResult, *stellar.SimulationError)
}

// ContractSnapshot is a vault's summary as the contract reports it, every
// value read at the same ledger.
type ContractSnapshot struct {
	Admin          string
	Token          string
	Deadline       time.Time
	TotalAllocated data.VaultAmount
	TotalClaimed   data.VaultAmount
	Balance        data.VaultAmount
	// Ledger is the latest ledger every read reported. The values reflect
	// state after this ledger closed.
	Ledger uint32
}

// SnapshotError is a failed snapshot attempt. Category is what gets stored;
// Err is for logs only, because RPC error text can carry a provider URL.
type SnapshotError struct {
	Category data.SnapshotErrorCategory
	Err      error
}

func (e *SnapshotError) Error() string {
	return fmt.Sprintf("vault snapshot failed (%s): %v", e.Category, e.Err)
}

func (e *SnapshotError) Unwrap() error { return e.Err }

// SnapshotBackoff is the delay before the next attempt after the given number
// of consecutive failures (counting the one that just happened).
func SnapshotBackoff(failures int) time.Duration {
	if failures <= 1 {
		return SnapshotBackoffBase
	}
	delay := SnapshotBackoffBase
	for i := 1; i < failures; i++ {
		delay *= 2
		if delay >= SnapshotBackoffMax {
			return SnapshotBackoffMax
		}
	}
	return delay
}

// ReadContractSnapshot reads the summary fields from the vault contract and
// its token, all at one ledger.
//
// The six reads are separate simulations, so each can land on a different
// ledger. Every response must report the same latestLedger; if one does not,
// the whole set is read again, up to snapshotLedgerAttempts times. A
// mixed-ledger result is never returned: it could pair a balance from after a
// withdrawal with a total from before it, which no ledger ever held.
func ReadContractSnapshot(ctx context.Context, rpcClient SnapshotRPCClient, contractID string) (*ContractSnapshot, error) {
	var lastMismatch error
	for attempt := 0; attempt < snapshotLedgerAttempts; attempt++ {
		snapshot, err := readSnapshotOnce(ctx, rpcClient, contractID)
		if err == nil {
			return snapshot, nil
		}
		var mismatch *ledgerMismatchError
		if !errors.As(err, &mismatch) {
			return nil, err
		}
		lastMismatch = err
	}

	return nil, &SnapshotError{Category: data.SnapshotErrorInconsistentLedgers, Err: lastMismatch}
}

type ledgerMismatchError struct {
	function string
	want     uint32
	got      uint32
}

func (e *ledgerMismatchError) Error() string {
	return fmt.Sprintf("%s was read at ledger %d, the others at %d", e.function, e.got, e.want)
}

// snapshotRead is one simulation in a set, pinned to the first one's ledger.
type snapshotRead struct {
	ctx       context.Context
	rpcClient SnapshotRPCClient
	ledger    uint32
}

func readSnapshotOnce(ctx context.Context, rpcClient SnapshotRPCClient, contractID string) (*ContractSnapshot, error) {
	r := &snapshotRead{ctx: ctx, rpcClient: rpcClient}

	adminVal, err := r.call(contractID, "get_admin")
	if err != nil {
		return nil, err
	}
	tokenVal, err := r.call(contractID, "get_token")
	if err != nil {
		return nil, err
	}
	deadlineVal, err := r.call(contractID, "get_deadline")
	if err != nil {
		return nil, err
	}
	allocatedVal, err := r.call(contractID, "total_allocated")
	if err != nil {
		return nil, err
	}
	claimedVal, err := r.call(contractID, "total_claimed")
	if err != nil {
		return nil, err
	}

	// Decode everything before the token read, which depends on get_token.
	admin, err := decodeAddress("get_admin", adminVal)
	if err != nil {
		return nil, err
	}
	token, err := decodeAddress("get_token", tokenVal)
	if err != nil {
		return nil, err
	}
	deadline, err := decodeDeadline(deadlineVal)
	if err != nil {
		return nil, err
	}
	totalAllocated, err := decodeAmount("total_allocated", allocatedVal)
	if err != nil {
		return nil, err
	}
	totalClaimed, err := decodeAmount("total_claimed", claimedVal)
	if err != nil {
		return nil, err
	}

	// The vault has no balance() of its own: its balance is the token's
	// SEP-41 balance for the vault's address. Going through the token's public
	// interface works for any SEP-41 token, not only a SAC.
	vaultAddress, err := addressScVal(contractID)
	if err != nil {
		return nil, &SnapshotError{Category: data.SnapshotErrorInvalidResponse, Err: err}
	}
	balanceVal, err := r.call(token, "balance", vaultAddress)
	if err != nil {
		return nil, err
	}
	balance, err := decodeAmount("balance", balanceVal)
	if err != nil {
		return nil, err
	}

	return &ContractSnapshot{
		Admin:          admin,
		Token:          token,
		Deadline:       deadline,
		TotalAllocated: totalAllocated,
		TotalClaimed:   totalClaimed,
		Balance:        balance,
		Ledger:         r.ledger,
	}, nil
}

// call simulates one read-only invocation and returns its return value.
func (r *snapshotRead) call(contractID, function string, args ...xdr.ScVal) (xdr.ScVal, error) {
	envelope, err := readOnlyEnvelope(contractID, function, args)
	if err != nil {
		return xdr.ScVal{}, &SnapshotError{Category: data.SnapshotErrorInvalidResponse, Err: err}
	}

	result, simErr := r.rpcClient.SimulateTransaction(r.ctx, protocol.SimulateTransactionRequest{Transaction: envelope})
	if simErr != nil {
		return xdr.ScVal{}, classifySimulationError(function, simErr)
	}

	response := result.Response
	if response.LatestLedger == 0 {
		return xdr.ScVal{}, &SnapshotError{
			Category: data.SnapshotErrorInvalidResponse,
			Err:      fmt.Errorf("%s: simulation reported no latest ledger", function),
		}
	}
	if r.ledger == 0 {
		r.ledger = response.LatestLedger
	} else if response.LatestLedger != r.ledger {
		return xdr.ScVal{}, &ledgerMismatchError{function: function, want: r.ledger, got: response.LatestLedger}
	}

	if len(response.Results) != 1 || response.Results[0].ReturnValueXDR == nil {
		return xdr.ScVal{}, &SnapshotError{
			Category: data.SnapshotErrorInvalidResponse,
			Err:      fmt.Errorf("%s: simulation returned %d results and no return value", function, len(response.Results)),
		}
	}

	var value xdr.ScVal
	if err := xdr.SafeUnmarshalBase64(*response.Results[0].ReturnValueXDR, &value); err != nil {
		return xdr.ScVal{}, &SnapshotError{
			Category: data.SnapshotErrorInvalidResponse,
			Err:      fmt.Errorf("%s: undecodable return value: %w", function, err),
		}
	}
	return value, nil
}

// classifySimulationError turns an RPC simulation failure into a stored
// category. The contract's NotInitialized gets its own category because no
// retry can fix it: there is no summary to read, and none is invented.
func classifySimulationError(function string, simErr *stellar.SimulationError) error {
	wrapped := fmt.Errorf("%s: %w", function, simErr)

	if simErr.Type == stellar.SimulationErrorTypeNetwork {
		return &SnapshotError{Category: data.SnapshotErrorRPCUnreachable, Err: wrapped}
	}

	raw := simErr.Error()
	if simErr.Response != nil && simErr.Response.Error != "" {
		raw = simErr.Response.Error
	}
	if match := contractErrorPattern.FindStringSubmatch(raw); match != nil {
		if code, convErr := strconv.Atoi(match[1]); convErr == nil && code == contractErrorNotInitialized {
			return &SnapshotError{Category: data.SnapshotErrorContractNotInitialized, Err: wrapped}
		}
	}

	return &SnapshotError{Category: data.SnapshotErrorSimulationFailed, Err: wrapped}
}

// readOnlyEnvelope builds an unsigned invocation for simulation only.
func readOnlyEnvelope(contractID, function string, args []xdr.ScVal) (string, error) {
	contractHash, err := contractIDToHash(contractID)
	if err != nil {
		return "", err
	}

	tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        &txnbuild.SimpleAccount{AccountID: snapshotSourceAccount, Sequence: 0},
		IncrementSequenceNum: true,
		BaseFee:              txnbuild.MinBaseFee,
		Preconditions:        txnbuild.Preconditions{TimeBounds: txnbuild.NewInfiniteTimeout()},
		Operations: []txnbuild.Operation{&txnbuild.InvokeHostFunction{
			HostFunction: xdr.HostFunction{
				Type: xdr.HostFunctionTypeHostFunctionTypeInvokeContract,
				InvokeContract: &xdr.InvokeContractArgs{
					ContractAddress: xdr.ScAddress{
						Type:       xdr.ScAddressTypeScAddressTypeContract,
						ContractId: &contractHash,
					},
					FunctionName: xdr.ScSymbol(function),
					Args:         xdr.ScVec(args),
				},
			},
		}},
	})
	if err != nil {
		return "", fmt.Errorf("building the %s read: %w", function, err)
	}

	envelope, err := tx.Base64()
	if err != nil {
		return "", fmt.Errorf("encoding the %s read: %w", function, err)
	}
	return envelope, nil
}

func invalid(function string, format string, args ...any) error {
	return &SnapshotError{
		Category: data.SnapshotErrorInvalidResponse,
		Err:      fmt.Errorf("%s: "+format, append([]any{function}, args...)...),
	}
}

func decodeAddress(function string, value xdr.ScVal) (string, error) {
	address, ok := value.GetAddress()
	if !ok {
		return "", invalid(function, "expected an address, got %s", value.Type)
	}
	encoded, err := address.String()
	if err != nil {
		return "", invalid(function, "unencodable address: %v", err)
	}
	return encoded, nil
}

func decodeDeadline(value xdr.ScVal) (time.Time, error) {
	seconds, ok := value.GetU64()
	if !ok {
		return time.Time{}, invalid("get_deadline", "expected a u64, got %s", value.Type)
	}
	if uint64(seconds) > uint64(1<<62) {
		return time.Time{}, invalid("get_deadline", "deadline %d is out of range", uint64(seconds))
	}
	return time.Unix(int64(seconds), 0).UTC(), nil
}

// decodeAmount reads an i128. A negative value is rejected: no vault total or
// token balance can be negative, and vault_state_totals_check would refuse it
// anyway — better to fail here with a reason than at the INSERT.
func decodeAmount(function string, value xdr.ScVal) (data.VaultAmount, error) {
	parts, ok := value.GetI128()
	if !ok {
		return data.VaultAmount{}, invalid(function, "expected an i128, got %s", value.Type)
	}

	// hi is signed and lo unsigned, so value = hi·2^64 + lo exactly, for
	// negative values too.
	n := new(big.Int).Add(
		new(big.Int).Lsh(big.NewInt(int64(parts.Hi)), 64),
		new(big.Int).SetUint64(uint64(parts.Lo)),
	)
	if n.Sign() < 0 {
		return data.VaultAmount{}, invalid(function, "negative amount %s", n)
	}

	amount, err := data.ParseVaultAmount(n.String())
	if err != nil {
		return data.VaultAmount{}, invalid(function, "unparseable amount: %v", err)
	}
	return amount, nil
}
