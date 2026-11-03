package vault

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	hProtocol "github.com/stellar/go-stellar-sdk/protocols/horizon"
	protocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/stellar/stellar-disbursement-platform-backend/internal/data"
	"github.com/stellar/stellar-disbursement-platform-backend/internal/stellar"
)

// This file builds unsigned, pre-simulated transaction envelopes for vault
// operations.
//
// The backend never signs and never submits. It has no vault signing key and
// no field that accepts one: the browser signs the envelope with the admin's
// wallet and submits it itself. Simulating here is what makes that usable —
// an operation the contract would reject is reported as a typed error *before*
// the user is asked to sign, rather than failing on-chain after they have.

// Operation is a vault contract function a client can be asked to sign.
type Operation string

const (
	OperationFund     Operation = "fund"
	OperationRegister Operation = "register"
	OperationCancel   Operation = "cancel"
	OperationWithdraw Operation = "withdraw"
)

// Claim is deliberately absent. A claim is authorised by the *recipient*, not
// the vault admin, and a recipient is not an SDP dashboard user — they claim
// from their own wallet against the contract directly. Offering to build their
// envelope here would imply SDP has a role in a transaction it cannot
// authorise.

// BuildRequest is a request to build one envelope.
type BuildRequest struct {
	Operation Operation `json:"operation"`
	// Source is the account that will sign. It defaults to the vault admin
	// from the cached state, since every operation here is admin-authorised.
	Source string `json:"source"`
	// Recipient is required for register and cancel.
	Recipient string `json:"recipient"`
	// Destination is required for withdraw.
	Destination string `json:"destination"`
	// Amount is required for fund, register and withdraw, in stroops.
	Amount *data.VaultAmount `json:"amount"`
}

// BuildResponse is an envelope ready for a wallet to sign.
type BuildResponse struct {
	XDR               string            `json:"xdr"`
	NetworkPassphrase string            `json:"network_passphrase"`
	Operation         Operation         `json:"operation"`
	MinResourceFee    string            `json:"min_resource_fee"`
	Summary           map[string]string `json:"summary"`
}

// ValidationError is a request the contract would certainly reject, caught
// before a network round trip.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

// ContractError is a simulation failure carrying the contract's own typed
// error, so the client can show the operator what the contract objected to
// rather than a generic failure.
type ContractError struct {
	// Code is the #[contracterror] discriminant, or 0 when the simulation
	// failed for a reason that is not a contract error.
	Code int
	// Name is the error variant, when the code is recognised.
	Name string
	// Message is the human-readable explanation.
	Message string
	// Raw is the simulation error as RPC reported it.
	Raw string
}

func (e *ContractError) Error() string {
	if e.Code > 0 {
		return fmt.Sprintf("contract error %d (%s): %s", e.Code, e.Name, e.Message)
	}
	return e.Message
}

// contractErrors maps the contract's #[contracterror] enum onto operator-
// facing prose. The codes are documented as stable and append-only in the
// contract's error.rs, which is what makes keying on the number safe.
var contractErrors = map[int]struct{ Name, Message string }{
	1:  {"AlreadyInitialized", "This vault has already been initialised."},
	2:  {"NotInitialized", "This vault has not been initialised yet."},
	3:  {"InvalidAmount", "The amount must be greater than zero."},
	4:  {"InvalidDeadline", "The deadline must be in the future."},
	5:  {"AllocationExists", "This recipient already has an allocation. Each address can be registered only once."},
	6:  {"AllocationNotFound", "This recipient has no allocation in this vault."},
	7:  {"AlreadyClaimed", "This allocation has already been claimed."},
	8:  {"AllocationCancelled", "This allocation was cancelled."},
	9:  {"InsufficientFunds", "The vault does not hold enough unallocated funds for this operation."},
	10: {"DeadlinePassed", "The claim deadline has passed, so this is no longer allowed."},
	11: {"DeadlineNotReached", "The claim deadline has not passed yet, so allocations cannot be cancelled."},
	12: {"MathOverflow", "The amount is too large for this vault's balances."},
}

// contractErrorPattern matches the host's rendering of a contract error, e.g.
// "HostError: Error(Contract, #9)".
var contractErrorPattern = regexp.MustCompile(`Error\(Contract,\s*#(\d+)\)`)

// AccountFetcher reads an account's current sequence number.
//
// Declared as an interface over the one Horizon call needed so the builder can
// be tested without a network, and so the dependency is visible: building an
// envelope requires the signer's sequence, which only the network knows.
type AccountFetcher interface {
	AccountDetail(request horizonclient.AccountRequest) (hProtocol.Account, error)
}

// TransactionBuilder builds unsigned vault envelopes.
type TransactionBuilder struct {
	rpcClient         stellar.RPCClient
	horizonClient     AccountFetcher
	models            *data.Models
	contractID        string
	networkPassphrase string
}

// NewTransactionBuilder builds a transaction builder.
func NewTransactionBuilder(
	rpcClient stellar.RPCClient,
	horizonClient AccountFetcher,
	models *data.Models,
	contractID string,
	networkPassphrase string,
) (*TransactionBuilder, error) {
	if rpcClient == nil {
		return nil, errors.New("rpcClient is required for NewTransactionBuilder")
	}
	if horizonClient == nil {
		return nil, errors.New("horizonClient is required for NewTransactionBuilder")
	}
	if models == nil {
		return nil, errors.New("models is required for NewTransactionBuilder")
	}
	if contractID == "" {
		return nil, errors.New("contractID is required for NewTransactionBuilder")
	}
	if networkPassphrase == "" {
		return nil, errors.New("networkPassphrase is required for NewTransactionBuilder")
	}

	return &TransactionBuilder{
		rpcClient:         rpcClient,
		horizonClient:     horizonClient,
		models:            models,
		contractID:        contractID,
		networkPassphrase: networkPassphrase,
	}, nil
}

// Build validates, assembles and simulates one vault operation.
func (b *TransactionBuilder) Build(ctx context.Context, request BuildRequest) (*BuildResponse, error) {
	state, err := b.models.Vault.GetState(ctx, b.models.DBConnectionPool, b.contractID)
	if err != nil {
		return nil, fmt.Errorf("reading vault state: %w", err)
	}

	if err := b.normalise(&request, state); err != nil {
		return nil, err
	}

	args, summary, err := b.invocationArgs(request, state)
	if err != nil {
		return nil, err
	}

	envelope, minResourceFee, err := b.assemble(ctx, request, args)
	if err != nil {
		return nil, err
	}

	return &BuildResponse{
		XDR:               envelope,
		NetworkPassphrase: b.networkPassphrase,
		Operation:         request.Operation,
		MinResourceFee:    minResourceFee,
		Summary:           summary,
	}, nil
}

// normalise fills defaults and rejects what the contract certainly would.
//
// The pre-flight checks are not duplicated contract logic for its own sake:
// they turn the common mistakes into an immediate field-level error instead of
// a simulation round trip followed by an opaque host error.
func (b *TransactionBuilder) normalise(request *BuildRequest, state *data.VaultState) error {
	request.Source = strings.TrimSpace(request.Source)
	request.Recipient = strings.TrimSpace(request.Recipient)
	request.Destination = strings.TrimSpace(request.Destination)

	switch request.Operation {
	case OperationFund, OperationRegister, OperationCancel, OperationWithdraw:
	default:
		return &ValidationError{
			Field:   "operation",
			Message: "operation must be one of fund, register, cancel, withdraw",
		}
	}

	// Every operation here is admin-authorised, so the admin is the default
	// signer.
	if request.Source == "" {
		request.Source = state.AdminAddress
	}
	// A contract address cannot sign a transaction envelope, and a secret key
	// must never appear in a request at all.
	if !strkey.IsValidEd25519PublicKey(request.Source) {
		message := "source must be a Stellar account address (G...)"
		if strings.HasPrefix(request.Source, "S") {
			message = "source must be a public account address (G...), never a secret key"
		}
		return &ValidationError{Field: "source", Message: message}
	}

	if request.Operation == OperationRegister || request.Operation == OperationCancel {
		if request.Recipient == "" {
			return &ValidationError{Field: "recipient", Message: fmt.Sprintf("recipient is required for %s", request.Operation)}
		}
		if !isStellarAddress(request.Recipient) {
			return &ValidationError{Field: "recipient", Message: "recipient must be a Stellar account (G...) or contract (C...) address"}
		}
	}

	if request.Operation == OperationWithdraw {
		if request.Destination == "" {
			return &ValidationError{Field: "destination", Message: "destination is required for withdraw"}
		}
		if !isStellarAddress(request.Destination) {
			return &ValidationError{Field: "destination", Message: "destination must be a Stellar account (G...) or contract (C...) address"}
		}
	}

	needsAmount := request.Operation == OperationFund ||
		request.Operation == OperationRegister ||
		request.Operation == OperationWithdraw
	if needsAmount {
		if request.Amount == nil {
			return &ValidationError{Field: "amount", Message: fmt.Sprintf("amount is required for %s", request.Operation)}
		}
		if request.Amount.IsZero() || request.Amount.IsNegative() {
			return &ValidationError{Field: "amount", Message: "amount must be greater than zero"}
		}
	}

	return nil
}

// invocationArgs encodes the contract call arguments and the signing summary.
//
// The summary is what the client shows before asking for a signature. It is
// built here rather than in the browser so that what is described and what is
// encoded come from the same values — a summary assembled separately from the
// envelope can drift from it, which is exactly the confusion a confirmation
// screen is supposed to prevent.
func (b *TransactionBuilder) invocationArgs(request BuildRequest, state *data.VaultState) (xdr.ScVec, map[string]string, error) {
	summary := map[string]string{
		"operation":  string(request.Operation),
		"vault":      b.contractID,
		"token":      state.TokenAddress,
		"signer":     request.Source,
		"network":    b.networkPassphrase,
		"deadline":   state.Deadline.UTC().Format("2006-01-02 15:04:05 UTC"),
		"is_expired": strconv.FormatBool(state.IsExpired(time.Now())),
	}

	var args xdr.ScVec

	switch request.Operation {
	case OperationFund:
		amount, err := amountScVal(*request.Amount)
		if err != nil {
			return nil, nil, err
		}
		args = xdr.ScVec{amount}
		summary["amount"] = request.Amount.String()
		summary["effect"] = fmt.Sprintf("Transfer %s stroops from %s into the vault.",
			request.Amount, request.Source)

	case OperationRegister:
		recipient, err := addressScVal(request.Recipient)
		if err != nil {
			return nil, nil, err
		}
		amount, err := amountScVal(*request.Amount)
		if err != nil {
			return nil, nil, err
		}
		args = xdr.ScVec{recipient, amount}
		summary["recipient"] = request.Recipient
		summary["amount"] = request.Amount.String()
		summary["effect"] = fmt.Sprintf("Allocate %s stroops to %s, claimable until the deadline.",
			request.Amount, request.Recipient)

	case OperationCancel:
		recipient, err := addressScVal(request.Recipient)
		if err != nil {
			return nil, nil, err
		}
		args = xdr.ScVec{recipient}
		summary["recipient"] = request.Recipient
		summary["effect"] = fmt.Sprintf(
			"Cancel %s's unclaimed allocation and release it back to the vault's withdrawable balance. Moves no tokens.",
			request.Recipient)

	case OperationWithdraw:
		destination, err := addressScVal(request.Destination)
		if err != nil {
			return nil, nil, err
		}
		amount, err := amountScVal(*request.Amount)
		if err != nil {
			return nil, nil, err
		}
		args = xdr.ScVec{destination, amount}
		summary["destination"] = request.Destination
		summary["amount"] = request.Amount.String()
		summary["effect"] = fmt.Sprintf("Send %s stroops of unallocated vault funds to %s.",
			request.Amount, request.Destination)

	default:
		// Unreachable: normalise rejects anything else first.
		return nil, nil, &ValidationError{Field: "operation", Message: "unsupported operation"}
	}

	return args, summary, nil
}

// assemble builds the envelope, simulates it, and folds the simulation's
// resource footprint and authorisation entries back in.
//
// Without that fold-back the envelope would be unsubmittable: Soroban requires
// the transaction to declare its resource usage, and only simulation can
// compute it.
func (b *TransactionBuilder) assemble(ctx context.Context, request BuildRequest, args xdr.ScVec) (string, string, error) {
	contractID, err := contractIDToHash(b.contractID)
	if err != nil {
		return "", "", err
	}

	account, err := b.horizonClient.AccountDetail(horizonclient.AccountRequest{AccountID: request.Source})
	if err != nil {
		// A source account that does not exist on the network is a client
		// error, not a server fault: the operator typed an unfunded address.
		return "", "", &ValidationError{
			Field:   "source",
			Message: fmt.Sprintf("source account %s was not found on the network", request.Source),
		}
	}

	operation := &txnbuild.InvokeHostFunction{
		SourceAccount: request.Source,
		HostFunction: xdr.HostFunction{
			Type: xdr.HostFunctionTypeHostFunctionTypeInvokeContract,
			InvokeContract: &xdr.InvokeContractArgs{
				ContractAddress: xdr.ScAddress{
					Type:       xdr.ScAddressTypeScAddressTypeContract,
					ContractId: &contractID,
				},
				FunctionName: xdr.ScSymbol(request.Operation),
				Args:         args,
			},
		},
	}

	tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        &account,
		IncrementSequenceNum: true,
		BaseFee:              txnbuild.MinBaseFee,
		Preconditions: txnbuild.Preconditions{
			// Five minutes: long enough for a wallet confirmation, short
			// enough that a forgotten envelope expires rather than being
			// submitted much later against changed vault state.
			TimeBounds: txnbuild.NewTimeout(300),
		},
		Operations: []txnbuild.Operation{operation},
	})
	if err != nil {
		return "", "", fmt.Errorf("building the vault transaction: %w", err)
	}

	unsimulated, err := tx.Base64()
	if err != nil {
		return "", "", fmt.Errorf("encoding the vault transaction: %w", err)
	}

	result, simErr := b.rpcClient.SimulateTransaction(ctx, protocol.SimulateTransactionRequest{
		Transaction: unsimulated,
	})
	if simErr != nil {
		return "", "", asContractError(simErr)
	}

	if err := applySimulation(operation, result.Response); err != nil {
		return "", "", err
	}

	// Rebuild with the resource footprint and auth entries attached. The
	// sequence is taken from the first build rather than incremented again,
	// so the simulated and returned envelopes describe the same transaction.
	simulated, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount: &txnbuild.SimpleAccount{
			AccountID: account.AccountID,
			Sequence:  tx.SequenceNumber(),
		},
		IncrementSequenceNum: false,
		BaseFee:              txnbuild.MinBaseFee + minResourceFeeOf(result.Response),
		Preconditions:        txnbuild.Preconditions{TimeBounds: txnbuild.NewTimeout(300)},
		Operations:           []txnbuild.Operation{operation},
	})
	if err != nil {
		return "", "", fmt.Errorf("rebuilding the simulated vault transaction: %w", err)
	}

	envelope, err := simulated.Base64()
	if err != nil {
		return "", "", fmt.Errorf("encoding the simulated vault transaction: %w", err)
	}

	return envelope, strconv.FormatInt(minResourceFeeOf(result.Response), 10), nil
}

// applySimulation attaches the simulation's resource footprint and the
// authorisation entries it computed.
func applySimulation(operation *txnbuild.InvokeHostFunction, response protocol.SimulateTransactionResponse) error {
	if response.TransactionDataXDR != "" {
		var transactionData xdr.SorobanTransactionData
		if err := xdr.SafeUnmarshalBase64(response.TransactionDataXDR, &transactionData); err != nil {
			return fmt.Errorf("unmarshalling simulated transaction data: %w", err)
		}
		operation.Ext = xdr.TransactionExt{V: 1, SorobanData: &transactionData}
	}

	// Exactly one result is expected, because the envelope carries exactly one
	// operation. More than one means the response does not describe what was
	// sent.
	if len(response.Results) == 0 {
		return nil
	}
	if len(response.Results) != 1 {
		return fmt.Errorf("simulation returned %d results for a single operation", len(response.Results))
	}

	authXDR := response.Results[0].AuthXDR
	if authXDR == nil {
		return nil
	}

	entries := make([]xdr.SorobanAuthorizationEntry, 0, len(*authXDR))
	for i, raw := range *authXDR {
		var entry xdr.SorobanAuthorizationEntry
		if err := xdr.SafeUnmarshalBase64(raw, &entry); err != nil {
			return fmt.Errorf("unmarshalling simulated auth entry %d: %w", i, err)
		}
		entries = append(entries, entry)
	}
	operation.Auth = entries

	return nil
}

func minResourceFeeOf(response protocol.SimulateTransactionResponse) int64 {
	return response.MinResourceFee
}

// asContractError turns a simulation failure into the contract's own typed
// error when it is one, so the client can explain what the contract objected
// to instead of showing a host error string.
func asContractError(simErr *stellar.SimulationError) error {
	raw := ""
	if simErr.Response != nil {
		raw = simErr.Response.Error
	}
	if raw == "" {
		raw = simErr.Error()
	}

	if match := contractErrorPattern.FindStringSubmatch(raw); match != nil {
		code, convErr := strconv.Atoi(match[1])
		if convErr == nil {
			if known, ok := contractErrors[code]; ok {
				return &ContractError{Code: code, Name: known.Name, Message: known.Message, Raw: raw}
			}
			return &ContractError{
				Code:    code,
				Name:    "Unknown",
				Message: fmt.Sprintf("The contract rejected this operation with error %d.", code),
				Raw:     raw,
			}
		}
	}

	return &ContractError{
		Message: "The vault contract rejected this operation during simulation.",
		Raw:     raw,
	}
}

// amountScVal encodes an i128 stroop amount.
func amountScVal(amount data.VaultAmount) (xdr.ScVal, error) {
	value := amount.BigInt()

	// Split into the high and low 64-bit halves the XDR form uses. Going via
	// big.Int rather than int64 is the point of the whole VaultAmount type:
	// these values do not fit in any Go integer.
	lo := new(big.Int).And(value, new(big.Int).SetUint64(^uint64(0)))
	hi := new(big.Int).Rsh(value, 64)

	parts := xdr.Int128Parts{
		Hi: xdr.Int64(hi.Int64()),
		Lo: xdr.Uint64(lo.Uint64()),
	}

	scVal, err := xdr.NewScVal(xdr.ScValTypeScvI128, parts)
	if err != nil {
		return xdr.ScVal{}, fmt.Errorf("encoding amount %s as i128: %w", amount, err)
	}

	return scVal, nil
}

// addressScVal encodes an account or contract address.
func addressScVal(address string) (xdr.ScVal, error) {
	scAddress, err := newScAddress(address)
	if err != nil {
		return xdr.ScVal{}, err
	}

	scVal, err := xdr.NewScVal(xdr.ScValTypeScvAddress, scAddress)
	if err != nil {
		return xdr.ScVal{}, fmt.Errorf("encoding address %s: %w", address, err)
	}

	return scVal, nil
}

func newScAddress(address string) (xdr.ScAddress, error) {
	if strkey.IsValidEd25519PublicKey(address) {
		accountID, err := xdr.AddressToAccountId(address)
		if err != nil {
			return xdr.ScAddress{}, fmt.Errorf("decoding account address %s: %w", address, err)
		}
		return xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &accountID}, nil
	}

	if strkey.IsValidContractAddress(address) {
		hash, err := contractIDToHash(address)
		if err != nil {
			return xdr.ScAddress{}, err
		}
		return xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &hash}, nil
	}

	return xdr.ScAddress{}, fmt.Errorf("%s is not a Stellar address", address)
}

func contractIDToHash(contractID string) (xdr.ContractId, error) {
	decoded, err := strkey.Decode(strkey.VersionByteContract, contractID)
	if err != nil {
		return xdr.ContractId{}, fmt.Errorf("decoding contract id %s: %w", contractID, err)
	}

	var hash xdr.ContractId
	if len(decoded) != len(hash) {
		return xdr.ContractId{}, fmt.Errorf("contract id %s decoded to %d bytes, expected %d", contractID, len(decoded), len(hash))
	}
	copy(hash[:], decoded)

	return hash, nil
}
