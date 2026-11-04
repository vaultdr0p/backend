package data

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
)

// i128Bits is the width of the contract's token amount type. Soroban token
// amounts are i128, so a decoded amount that does not fit is not a large
// number — it is a decoding bug or a hostile response, and is rejected rather
// than truncated.
const i128Bits = 128

var (
	// maxI128 is 2^127 - 1.
	maxI128 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), i128Bits-1), big.NewInt(1))
	// minI128 is -2^127.
	minI128 = new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), i128Bits-1))
)

// VaultAmount is an i128 token amount in stroops.
//
// It exists because neither of the obvious choices works end to end:
//
//   - int64 (Postgres BIGINT) cannot hold the range. i128::MAX is ~1.7e38
//     against int64's ~9.2e18 — nineteen orders of magnitude short — so a large
//     allocation would be silently corrupted.
//   - float64 (a JSON number) cannot represent the range exactly either;
//     IEEE-754 doubles lose integer precision above 2^53.
//
// So the value is a big.Int in Go, NUMERIC(40,0) in Postgres, and a decimal
// **string** in JSON. The string is the part API clients must honour: parsing
// one of these with JavaScript's Number would quietly round it.
type VaultAmount struct {
	// i is the value. The zero VaultAmount has a nil i and reads as 0, so a
	// freshly declared value is usable without construction.
	i *big.Int
}

// NewVaultAmount builds an amount from an int64, for test fixtures and small
// constants.
func NewVaultAmount(v int64) VaultAmount {
	return VaultAmount{i: big.NewInt(v)}
}

// ParseVaultAmount parses a decimal string into an amount, rejecting anything
// that is not a plain base-10 integer inside the i128 range. It deliberately
// does not accept "1e7", "0x10", "+1", underscores or whitespace beyond the
// edges: these come off the wire, and a lenient parser here would turn a
// malformed amount into a plausible-looking wrong one.
func ParseVaultAmount(s string) (VaultAmount, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return VaultAmount{}, fmt.Errorf("amount is empty")
	}

	i, ok := new(big.Int).SetString(trimmed, 10)
	if !ok {
		return VaultAmount{}, fmt.Errorf("amount %q is not a base-10 integer", s)
	}
	if i.Cmp(minI128) < 0 || i.Cmp(maxI128) > 0 {
		return VaultAmount{}, fmt.Errorf("amount %q is outside the i128 range", s)
	}

	return VaultAmount{i: i}, nil
}

// BigInt returns a copy of the underlying value. The copy matters: handing out
// the pointer would let a caller mutate an amount already written to a row.
func (a VaultAmount) BigInt() *big.Int {
	if a.i == nil {
		return big.NewInt(0)
	}
	return new(big.Int).Set(a.i)
}

// String renders the amount as a decimal integer, which is both the JSON
// representation and the Postgres literal.
func (a VaultAmount) String() string {
	if a.i == nil {
		return "0"
	}
	return a.i.String()
}

// IsZero reports whether the amount is zero, including the nil zero value.
func (a VaultAmount) IsZero() bool {
	return a.i == nil || a.i.Sign() == 0
}

// IsNegative reports whether the amount is below zero. Contract amounts are
// always positive, so this is an invariant check rather than a branch the
// happy path takes.
func (a VaultAmount) IsNegative() bool {
	return a.i != nil && a.i.Sign() < 0
}

// Cmp compares two amounts, with the same -1/0/+1 convention as big.Int.Cmp.
func (a VaultAmount) Cmp(other VaultAmount) int {
	return a.BigInt().Cmp(other.BigInt())
}

// Sub returns a - other. It is used for `available` (balance - total_allocated),
// which is the only arithmetic the backend does on amounts: every other value
// is read from the contract, so there is nothing to recompute and nothing to
// drift.
func (a VaultAmount) Sub(other VaultAmount) VaultAmount {
	return VaultAmount{i: new(big.Int).Sub(a.BigInt(), other.BigInt())}
}

// Value implements driver.Valuer, writing the amount as a string so lib/pq
// hands Postgres an exact NUMERIC literal. Passing a big.Int directly would
// fail, and passing an int64 would overflow.
func (a VaultAmount) Value() (driver.Value, error) {
	return a.String(), nil
}

// Scan implements sql.Scanner. lib/pq returns NUMERIC as []byte, but string,
// int64 and nil are all handled so the type survives a driver change or a
// COALESCE that collapses to a different representation.
func (a *VaultAmount) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		a.i = nil
		return nil
	case []byte:
		return a.scanString(string(v))
	case string:
		return a.scanString(v)
	case int64:
		a.i = big.NewInt(v)
		return nil
	default:
		return fmt.Errorf("cannot scan %T into VaultAmount", src)
	}
}

func (a *VaultAmount) scanString(s string) error {
	parsed, err := ParseVaultAmount(s)
	if err != nil {
		return fmt.Errorf("scanning VaultAmount: %w", err)
	}
	a.i = parsed.i
	return nil
}

// MarshalJSON emits the amount as a JSON string, per the API contract. This is
// the whole reason the type exists on the response structs rather than an
// int64.
func (a VaultAmount) MarshalJSON() ([]byte, error) {
	return json.Marshal(a.String())
}

// maxJSONNumberAmount is 2^53 - 1: JavaScript's Number.MAX_SAFE_INTEGER, the
// largest integer an IEEE-754 double represents exactly.
const maxJSONNumberAmount = int64(1)<<53 - 1

// UnmarshalJSON accepts a JSON string, which is the documented wire form, and
// also a small JSON number, so a hand-written request body with `"amount": 100`
// is not rejected on a technicality.
//
// A JSON number above Number.MAX_SAFE_INTEGER is **rejected** rather than
// accepted. Go's json.Number preserves the literal text, so nothing is lost on
// this side — but a client that serialised a 39-digit amount as a JSON number
// has almost certainly already rounded it, and there is no way to tell a
// correctly-transmitted large number from a corrupted one. Refusing it turns a
// silent wrong amount into a clear error naming the string form.
func (a *VaultAmount) UnmarshalJSON(b []byte) error {
	// Reject null explicitly. Without this, json.Unmarshal into a string
	// succeeds as a no-op and the value reads as "", producing a confusing
	// "amount is empty" rather than naming the real problem.
	if string(b) == "null" {
		return fmt.Errorf("amount must be a decimal string, got null")
	}

	var asString string
	if err := json.Unmarshal(b, &asString); err == nil {
		parsed, err := ParseVaultAmount(asString)
		if err != nil {
			return err
		}
		a.i = parsed.i
		return nil
	}

	var asNumber json.Number
	if err := json.Unmarshal(b, &asNumber); err != nil {
		return fmt.Errorf("amount must be a decimal string: %w", err)
	}
	parsed, err := ParseVaultAmount(asNumber.String())
	if err != nil {
		return fmt.Errorf("amount must be a decimal string, not a float: %w", err)
	}
	if !parsed.BigInt().IsInt64() || parsed.BigInt().Int64() > maxJSONNumberAmount {
		return fmt.Errorf("amount %s exceeds the exact range of a JSON number; send it as a decimal string", asNumber.String())
	}
	a.i = parsed.i
	return nil
}
