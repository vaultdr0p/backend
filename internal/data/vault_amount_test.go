package data

import (
	"encoding/json"
	"math/big"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The boundaries of the contract's amount type, as decimal strings. These are
// the values BIGINT cannot hold and a JSON number cannot represent.
const (
	i128MaxStr = "170141183460469231731687303715884105727"  // 2^127 - 1
	i128MinStr = "-170141183460469231731687303715884105728" // -2^127
)

func Test_ParseVaultAmount(t *testing.T) {
	testCases := []struct {
		name    string
		input   string
		want    string
		wantErr string
	}{
		{name: "zero", input: "0", want: "0"},
		{name: "one stroop", input: "1", want: "1"},
		{name: "one unit", input: "10000000", want: "10000000"},
		{name: "i128 max", input: i128MaxStr, want: i128MaxStr},
		{name: "i128 min", input: i128MinStr, want: i128MinStr},
		{name: "surrounding whitespace is trimmed", input: "  42  ", want: "42"},
		{name: "a negative amount parses", input: "-5", want: "-5"},

		{name: "empty", input: "", wantErr: "amount is empty"},
		{name: "whitespace only", input: "   ", wantErr: "amount is empty"},
		// Everything below is a shape that would otherwise parse into a
		// plausible but wrong number.
		{name: "scientific notation", input: "1e7", wantErr: "not a base-10 integer"},
		{name: "hexadecimal", input: "0x10", wantErr: "not a base-10 integer"},
		{name: "underscore separators", input: "1_000", wantErr: "not a base-10 integer"},
		{name: "a decimal point", input: "1.5", wantErr: "not a base-10 integer"},
		{name: "letters", input: "abc", wantErr: "not a base-10 integer"},
		{name: "internal whitespace", input: "1 000", wantErr: "not a base-10 integer"},
		{name: "one over i128 max", input: "170141183460469231731687303715884105728", wantErr: "outside the i128 range"},
		{name: "one under i128 min", input: "-170141183460469231731687303715884105729", wantErr: "outside the i128 range"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseVaultAmount(tc.input)

			if tc.wantErr != "" {
				assert.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.String())
		})
	}
}

func Test_VaultAmount_zeroValueIsUsable(t *testing.T) {
	// A VaultAmount inside a freshly declared struct has a nil big.Int. Every
	// method must treat that as zero rather than panicking, because sqlx
	// produces exactly that shape before Scan runs.
	var a VaultAmount

	assert.Equal(t, "0", a.String())
	assert.True(t, a.IsZero())
	assert.False(t, a.IsNegative())
	assert.Equal(t, big.NewInt(0), a.BigInt())

	v, err := a.Value()
	require.NoError(t, err)
	assert.Equal(t, "0", v)
}

func Test_VaultAmount_BigIntReturnsACopy(t *testing.T) {
	// Handing out the internal pointer would let a caller mutate an amount
	// that has already been written to a row.
	a := NewVaultAmount(100)

	got := a.BigInt()
	got.SetInt64(999)

	assert.Equal(t, "100", a.String())
}

func Test_VaultAmount_SubAndCmp(t *testing.T) {
	max, err := ParseVaultAmount(i128MaxStr)
	require.NoError(t, err)

	testCases := []struct {
		name    string
		a, b    VaultAmount
		wantSub string
		wantCmp int
	}{
		{name: "balance minus allocation", a: NewVaultAmount(100), b: NewVaultAmount(40), wantSub: "60", wantCmp: 1},
		{name: "equal", a: NewVaultAmount(40), b: NewVaultAmount(40), wantSub: "0", wantCmp: 0},
		// `available` can legitimately go negative if the indexer's cached
		// snapshot is mid-refresh, so the type must not clamp it.
		{name: "over-allocated goes negative", a: NewVaultAmount(40), b: NewVaultAmount(100), wantSub: "-60", wantCmp: -1},
		{name: "no overflow at i128 max", a: max, b: NewVaultAmount(1), wantSub: "170141183460469231731687303715884105726", wantCmp: 1},
		{name: "zero values", a: VaultAmount{}, b: VaultAmount{}, wantSub: "0", wantCmp: 0},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.wantSub, tc.a.Sub(tc.b).String())
			assert.Equal(t, tc.wantCmp, tc.a.Cmp(tc.b))
		})
	}
}

func Test_VaultAmount_Scan(t *testing.T) {
	testCases := []struct {
		name    string
		src     any
		want    string
		wantErr string
	}{
		// lib/pq hands NUMERIC back as []byte; the others are here so the type
		// survives a driver change or a COALESCE that collapses the shape.
		{name: "bytes, as lib/pq returns NUMERIC", src: []byte(i128MaxStr), want: i128MaxStr},
		{name: "string", src: "12345", want: "12345"},
		{name: "int64", src: int64(42), want: "42"},
		{name: "nil reads as zero", src: nil, want: "0"},

		{name: "float is rejected", src: 1.5, wantErr: "cannot scan float64"},
		{name: "garbage bytes are rejected", src: []byte("not a number"), wantErr: "not a base-10 integer"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var a VaultAmount
			err := a.Scan(tc.src)

			if tc.wantErr != "" {
				assert.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, a.String())
		})
	}
}

func Test_VaultAmount_JSONIsAlwaysAString(t *testing.T) {
	// The API contract says amounts are strings. A JSON number here would be
	// rounded by any JavaScript client, which is the whole failure this type
	// exists to prevent.
	type payload struct {
		Amount VaultAmount `json:"amount"`
	}

	max, err := ParseVaultAmount(i128MaxStr)
	require.NoError(t, err)

	b, err := json.Marshal(payload{Amount: max})
	require.NoError(t, err)
	assert.JSONEq(t, `{"amount":"`+i128MaxStr+`"}`, string(b))

	// And the zero value must not marshal as null.
	b, err = json.Marshal(payload{})
	require.NoError(t, err)
	assert.JSONEq(t, `{"amount":"0"}`, string(b))
}

func Test_VaultAmount_UnmarshalJSON(t *testing.T) {
	testCases := []struct {
		name    string
		input   string
		want    string
		wantErr string
	}{
		{name: "a decimal string", input: `"10000000"`, want: "10000000"},
		{name: "i128 max as a string", input: `"` + i128MaxStr + `"`, want: i128MaxStr},
		// A small integer typed by hand into a request body is accepted rather
		// than rejected on a technicality.
		{name: "a small JSON number", input: `100`, want: "100"},

		{name: "a float", input: `1.5`, wantErr: "not a float"},
		{name: "scientific notation", input: `1e7`, wantErr: "not a float"},
		{name: "a string that is not a number", input: `"abc"`, wantErr: "not a base-10 integer"},
		{name: "null", input: `null`, wantErr: "got null"},
		{name: "a bool", input: `true`, wantErr: "amount must be a decimal string"},
		// A JSON number at the edge of exact double representation is still
		// accepted; one past it is not. Go's json.Number keeps the literal
		// text, so nothing is lost here — but a client that sent a 39-digit
		// amount as a JSON number has almost certainly already rounded it, and
		// there is no way to distinguish that from a correct one.
		{name: "Number.MAX_SAFE_INTEGER", input: "9007199254740991", want: "9007199254740991"},
		{name: "one past Number.MAX_SAFE_INTEGER", input: "9007199254740992", wantErr: "exceeds the exact range of a JSON number"},
		{name: "i128 max as a JSON number", input: i128MaxStr, wantErr: "exceeds the exact range of a JSON number"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var a VaultAmount
			err := json.Unmarshal([]byte(tc.input), &a)

			if tc.wantErr != "" {
				assert.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, a.String())
		})
	}
}

func Test_VaultAmount_roundTripsThroughJSONAndSQL(t *testing.T) {
	for _, s := range []string{"0", "1", "10000000", i128MaxStr, i128MinStr} {
		t.Run(s, func(t *testing.T) {
			original, err := ParseVaultAmount(s)
			require.NoError(t, err)

			// JSON round trip.
			b, err := json.Marshal(original)
			require.NoError(t, err)
			var fromJSON VaultAmount
			require.NoError(t, json.Unmarshal(b, &fromJSON))
			assert.Equal(t, s, fromJSON.String())

			// SQL round trip: Value produces what Scan consumes.
			v, err := original.Value()
			require.NoError(t, err)
			var fromSQL VaultAmount
			require.NoError(t, fromSQL.Scan(v))
			assert.Equal(t, s, fromSQL.String())
		})
	}
}
