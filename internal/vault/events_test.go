package vault

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	protocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Addresses used throughout. These are the real testnet accounts the fixture
// was generated with, so strkey's checksum validation runs against genuine
// values rather than 56-character strings that merely look right.
const (
	adminAddress     = "GAODBHVR63Z56MVQRBEJSYM2H5423LJ4WAPUUBOFG4JYY72S6ROKVZRX"
	recipientAddress = "GCNMZYNV3NVFLK25K3KLZ2LM6HAAD4BVYHULQX746XERD6HQRGGJ6SLD"
	otherRecipient   = "GD6KZDL3Q7DO5AV3FHR44INMTTWPRMHZVLHKGCTCIQ23OM22FHYXBLBF"
	vaultContractID  = "CB5HTYC4GZW67ET23SACDCWCCDQ3AKU4TYX7SAUQJSKS6LLQLYA4HG53"
	tokenContractID  = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
)

// loadFixtureEvents reads testdata/events.json: verbatim `getEvents` output
// from soroban-testnet.stellar.org, captured by driving the deployed
// disbursement-vault contract through initialize → fund → register → claim →
// cancel → withdraw.
//
// The point of testing against captured output rather than hand-written JSON
// is that a hand-written fixture only proves the decoder agrees with my
// assumptions about the encoding. This proves it agrees with the server.
func loadFixtureEvents(t *testing.T) map[EventType]protocol.EventInfo {
	t.Helper()

	raw, err := os.ReadFile("testdata/events.json")
	require.NoError(t, err)

	var infos []protocol.EventInfo
	require.NoError(t, json.Unmarshal(raw, &infos))
	require.Len(t, infos, 6, "fixture must cover every event the contract emits")

	byType := make(map[EventType]protocol.EventInfo, len(infos))
	for _, info := range infos {
		decoded, err := DecodeEvent(info)
		require.NoError(t, err)
		byType[decoded.Type] = info
	}
	require.Len(t, byType, 6, "fixture must not contain two events of the same type")

	return byType
}

func Test_DecodeEvent_realRPCFixture(t *testing.T) {
	fixture := loadFixtureEvents(t)

	testCases := []struct {
		name   string
		assert func(t *testing.T, e *Event)
	}{
		{
			name: string(EventTypeInitialized),
			assert: func(t *testing.T, e *Event) {
				// initialized is the only event with no amount, and the only
				// one carrying the token and deadline.
				assert.Equal(t, adminAddress, e.Address)
				assert.Nil(t, e.Amount)
				assert.Equal(t, tokenContractID, e.Token)
				require.NotNil(t, e.Deadline)
				assert.Equal(t, time.Unix(1793717578, 0).UTC(), *e.Deadline)
			},
		},
		{
			name: string(EventTypeFunded),
			assert: func(t *testing.T, e *Event) {
				assert.Equal(t, adminAddress, e.Address)
				require.NotNil(t, e.Amount)
				assert.Equal(t, "20000000", e.Amount.String())
				require.NotNil(t, e.Balance)
				assert.Equal(t, "20000000", e.Balance.String())
			},
		},
		{
			name: string(EventTypeRegistered),
			assert: func(t *testing.T, e *Event) {
				assert.Equal(t, otherRecipient, e.Address)
				require.NotNil(t, e.Amount)
				assert.Equal(t, "10000000", e.Amount.String())
				require.NotNil(t, e.TotalAllocated)
				assert.Equal(t, "10000000", e.TotalAllocated.String())
			},
		},
		{
			name: string(EventTypeClaimed),
			assert: func(t *testing.T, e *Event) {
				assert.Equal(t, recipientAddress, e.Address)
				require.NotNil(t, e.Amount)
				assert.Equal(t, "10000000", e.Amount.String())
				// claimed carries only an amount.
				assert.Nil(t, e.Balance)
				assert.Nil(t, e.TotalAllocated)
			},
		},
		{
			name: string(EventTypeCancelled),
			assert: func(t *testing.T, e *Event) {
				assert.Equal(t, otherRecipient, e.Address)
				require.NotNil(t, e.Amount)
				assert.Equal(t, "10000000", e.Amount.String())
			},
		},
		{
			name: string(EventTypeWithdrawn),
			assert: func(t *testing.T, e *Event) {
				assert.Equal(t, adminAddress, e.Address)
				require.NotNil(t, e.Amount)
				assert.Equal(t, "20000000", e.Amount.String())
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			info, ok := fixture[EventType(tc.name)]
			require.True(t, ok)

			decoded, err := DecodeEvent(info)
			require.NoError(t, err)

			// Common fields, asserted for every type.
			assert.Equal(t, EventType(tc.name), decoded.Type)
			assert.Equal(t, info.ID, decoded.EventID)
			assert.Equal(t, info.ContractID, decoded.ContractID)
			assert.Equal(t, info.Ledger, decoded.Ledger)
			assert.Equal(t, info.TransactionHash, decoded.TxHash)
			assert.False(t, decoded.LedgerClosedAt.IsZero())
			assert.Equal(t, time.UTC, decoded.LedgerClosedAt.Location())

			tc.assert(t, decoded)
		})
	}
}

func Test_DecodeEvent_payloadPreservesTheOriginal(t *testing.T) {
	// The payload is the recovery path: if a projection turns out to be wrong,
	// it has to be fixable by replaying from the database, because past the
	// RPC retention window the event cannot be fetched again.
	fixture := loadFixtureEvents(t)
	info := fixture[EventTypeRegistered]

	decoded, err := DecodeEvent(info)
	require.NoError(t, err)

	var payload eventPayload
	require.NoError(t, json.Unmarshal(decoded.Payload, &payload))

	require.Len(t, payload.Topic, 2)
	assert.JSONEq(t, string(info.TopicJSON[0]), string(payload.Topic[0]))
	assert.JSONEq(t, string(info.TopicJSON[1]), string(payload.Topic[1]))
	assert.JSONEq(t, string(info.ValueJSON), string(payload.Value))
	assert.Equal(t, info.TxIndex, payload.TxIdx)
	assert.Equal(t, info.OpIndex, payload.OpIdx)

	// And the payload must be valid JSONB input, since it goes into a jsonb
	// column.
	assert.True(t, json.Valid(decoded.Payload))
}

// newEventInfo builds a well-formed `registered` event, so each rejection test
// below can change exactly one thing. Building from a known-good value keeps
// the tests honest: a test that passes because the input was malformed in two
// ways proves nothing about the check it names.
func newEventInfo(t *testing.T, mutate func(*protocol.EventInfo)) protocol.EventInfo {
	t.Helper()

	info := protocol.EventInfo{
		EventType:       protocol.EventTypeContract,
		Ledger:          5538736,
		LedgerClosedAt:  "2026-11-03T14:48:40Z",
		ContractID:      vaultContractID,
		ID:              "0023788689981259776-0000000000",
		OpIndex:         0,
		TxIndex:         20,
		TransactionHash: "49fb32966ed813d2a0b857c070470d2ad5e51ee667486a85f4da877d66d83567",
		TopicJSON: []json.RawMessage{
			json.RawMessage(`{"symbol":"registered"}`),
			json.RawMessage(`{"address":"` + recipientAddress + `"}`),
		},
		ValueJSON: json.RawMessage(`{"map":[` +
			`{"key":{"symbol":"amount"},"val":{"i128":"10000000"}},` +
			`{"key":{"symbol":"total_allocated"},"val":{"i128":"10000000"}}]}`),
	}

	// Guard the baseline, so a mutation test cannot pass for the wrong reason.
	_, err := DecodeEvent(info)
	require.NoError(t, err, "baseline event must decode")

	if mutate != nil {
		mutate(&info)
	}
	return info
}

func Test_DecodeEvent_rejections(t *testing.T) {
	testCases := []struct {
		name    string
		mutate  func(*protocol.EventInfo)
		wantErr string
	}{
		{
			name:    "a diagnostic event, which means the filter was built wrong",
			mutate:  func(i *protocol.EventInfo) { i.EventType = protocol.EventTypeDiagnostic },
			wantErr: `expected a "contract" event, got "diagnostic"`,
		},
		{
			name:    "no event id, so it could not be deduplicated",
			mutate:  func(i *protocol.EventInfo) { i.ID = "" },
			wantErr: "event has no id",
		},
		{
			name:    "an event id that is not a cursor",
			mutate:  func(i *protocol.EventInfo) { i.ID = "not-a-cursor" },
			wantErr: "is not a cursor",
		},
		{
			name:    "a contract id that is not a contract address",
			mutate:  func(i *protocol.EventInfo) { i.ContractID = adminAddress },
			wantErr: "is not a contract address",
		},
		{
			name:    "one topic instead of two",
			mutate:  func(i *protocol.EventInfo) { i.TopicJSON = i.TopicJSON[:1] },
			wantErr: "expected 2 topics, got 1",
		},
		{
			name:    "three topics",
			mutate:  func(i *protocol.EventInfo) { i.TopicJSON = append(i.TopicJSON, json.RawMessage(`{"symbol":"extra"}`)) },
			wantErr: "expected 2 topics, got 3",
		},
		{
			name: "no topicJson at all, which is what xdrFormat=base64 returns",
			mutate: func(i *protocol.EventInfo) {
				i.TopicJSON = nil
				i.TopicXDR = []string{"AAAADwAAAApyZWdpc3RlcmVk"}
			},
			wantErr: "xdrFormat=json",
		},
		{
			name:    "an event name that is an address, not a symbol",
			mutate:  func(i *protocol.EventInfo) { i.TopicJSON[0] = json.RawMessage(`{"address":"` + adminAddress + `"}`) },
			wantErr: "event name topic: expected a symbol, got address",
		},
		{
			name:    "an address topic that is a symbol",
			mutate:  func(i *protocol.EventInfo) { i.TopicJSON[1] = json.RawMessage(`{"symbol":"nope"}`) },
			wantErr: "event address topic: expected an address, got symbol",
		},
		{
			name: "an address topic that fails its checksum",
			mutate: func(i *protocol.EventInfo) {
				// Last two characters transposed: right length and alphabet,
				// wrong checksum.
				bad := recipientAddress[:54] + recipientAddress[55:56] + recipientAddress[54:55]
				i.TopicJSON[1] = json.RawMessage(`{"address":"` + bad + `"}`)
			},
			wantErr: "is not a Stellar address",
		},
		{
			name:    "an unparseable ledgerClosedAt",
			mutate:  func(i *protocol.EventInfo) { i.LedgerClosedAt = "yesterday" },
			wantErr: "parsing ledgerClosedAt",
		},
		{
			name:    "no valueJson",
			mutate:  func(i *protocol.EventInfo) { i.ValueJSON = nil },
			wantErr: "xdrFormat=json",
		},
		{
			name:    "event data that is a bare i128 rather than a map",
			mutate:  func(i *protocol.EventInfo) { i.ValueJSON = json.RawMessage(`{"i128":"1"}`) },
			wantErr: "expected event data to be a map, got i128",
		},
		{
			name: "a data key that is not a symbol",
			mutate: func(i *protocol.EventInfo) {
				i.ValueJSON = json.RawMessage(`{"map":[{"key":{"i128":"1"},"val":{"i128":"1"}}]}`)
			},
			wantErr: "event data key 0: expected a symbol, got i128",
		},
		{
			name: "a duplicate data key, which the host cannot produce",
			mutate: func(i *protocol.EventInfo) {
				i.ValueJSON = json.RawMessage(`{"map":[` +
					`{"key":{"symbol":"amount"},"val":{"i128":"1"}},` +
					`{"key":{"symbol":"amount"},"val":{"i128":"2"}}]}`)
			},
			wantErr: `duplicate key "amount"`,
		},
		{
			name: "a missing required field",
			mutate: func(i *protocol.EventInfo) {
				i.ValueJSON = json.RawMessage(`{"map":[{"key":{"symbol":"amount"},"val":{"i128":"1"}}]}`)
			},
			wantErr: `missing field "total_allocated"`,
		},
		{
			name: "an amount that is a u64 rather than an i128",
			mutate: func(i *protocol.EventInfo) {
				i.ValueJSON = json.RawMessage(`{"map":[` +
					`{"key":{"symbol":"amount"},"val":{"u64":"1"}},` +
					`{"key":{"symbol":"total_allocated"},"val":{"i128":"1"}}]}`)
			},
			wantErr: `field "amount": expected an i128, got u64`,
		},
		{
			name: "an amount outside the i128 range",
			mutate: func(i *protocol.EventInfo) {
				i.ValueJSON = json.RawMessage(`{"map":[` +
					`{"key":{"symbol":"amount"},"val":{"i128":"170141183460469231731687303715884105728"}},` +
					`{"key":{"symbol":"total_allocated"},"val":{"i128":"1"}}]}`)
			},
			wantErr: "outside the i128 range",
		},
		{
			name: "a negative amount, which the contract never emits",
			mutate: func(i *protocol.EventInfo) {
				i.ValueJSON = json.RawMessage(`{"map":[` +
					`{"key":{"symbol":"amount"},"val":{"i128":"-1"}},` +
					`{"key":{"symbol":"total_allocated"},"val":{"i128":"1"}}]}`)
			},
			wantErr: "amount -1 is negative",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			decoded, err := DecodeEvent(newEventInfo(t, tc.mutate))

			assert.ErrorContains(t, err, tc.wantErr)
			assert.Nil(t, decoded)
		})
	}
}

func Test_DecodeEvent_unknownTypeIsSkippable(t *testing.T) {
	// An unrecognised event must be reported as ErrUnknownEventType
	// specifically, because the indexer keys its skip-rather-than-fail
	// behaviour on that sentinel. If this degraded into a generic error, a
	// contract upgrade adding a seventh event would wedge the cursor
	// permanently: every poll would re-fetch the same event and fail again.
	info := newEventInfo(t, func(i *protocol.EventInfo) {
		i.TopicJSON[0] = json.RawMessage(`{"symbol":"topped_up"}`)
	})

	decoded, err := DecodeEvent(info)

	require.ErrorIs(t, err, ErrUnknownEventType)
	assert.ErrorContains(t, err, `"topped_up"`)
	assert.Nil(t, decoded)
}

func Test_DecodeEvent_initializedFieldValidation(t *testing.T) {
	newInitialized := func(t *testing.T, value string) protocol.EventInfo {
		t.Helper()
		return protocol.EventInfo{
			EventType:       protocol.EventTypeContract,
			Ledger:          5538746,
			LedgerClosedAt:  "2026-11-03T14:49:30Z",
			ContractID:      vaultContractID,
			ID:              "0023788801650364416-0000000000",
			TransactionHash: "0f2a",
			TopicJSON: []json.RawMessage{
				json.RawMessage(`{"symbol":"initialized"}`),
				json.RawMessage(`{"address":"` + adminAddress + `"}`),
			},
			ValueJSON: json.RawMessage(value),
		}
	}

	testCases := []struct {
		name    string
		value   string
		wantErr string
	}{
		{
			name: "a token that is an account, not a contract",
			value: `{"map":[{"key":{"symbol":"deadline"},"val":{"u64":"1793717578"}},` +
				`{"key":{"symbol":"token"},"val":{"address":"` + adminAddress + `"}}]}`,
			wantErr: "is not a contract address",
		},
		{
			name: "a deadline that is an i128, not a u64",
			value: `{"map":[{"key":{"symbol":"deadline"},"val":{"i128":"1793717578"}},` +
				`{"key":{"symbol":"token"},"val":{"address":"` + tokenContractID + `"}}]}`,
			wantErr: `field "deadline": expected a u64, got i128`,
		},
		{
			name: "a zero deadline",
			value: `{"map":[{"key":{"symbol":"deadline"},"val":{"u64":"0"}},` +
				`{"key":{"symbol":"token"},"val":{"address":"` + tokenContractID + `"}}]}`,
			wantErr: "not a positive unix timestamp",
		},
		{
			name: "a deadline too large to be a timestamp",
			value: `{"map":[{"key":{"symbol":"deadline"},"val":{"u64":"18446744073709551615"}},` +
				`{"key":{"symbol":"token"},"val":{"address":"` + tokenContractID + `"}}]}`,
			wantErr: "not a representable unix timestamp",
		},
		{
			name:    "a missing token",
			value:   `{"map":[{"key":{"symbol":"deadline"},"val":{"u64":"1793717578"}}]}`,
			wantErr: `missing field "token"`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			decoded, err := DecodeEvent(newInitialized(t, tc.value))

			assert.ErrorContains(t, err, tc.wantErr)
			assert.Nil(t, decoded)
		})
	}
}

func Test_scNumber_acceptsStringsAndNumbers(t *testing.T) {
	// RPC emits wide integers as strings. A bare number is accepted too, so a
	// non-reference RPC implementation that emits `{"u32": 7}` still decodes —
	// but the amount parser still rejects anything non-integral, which is
	// where precision loss would actually bite.
	testCases := []struct {
		name    string
		input   string
		want    string
		wantErr string
	}{
		{name: "a decimal string", input: `"10000000"`, want: "10000000"},
		{name: "a wide value as a string", input: `"170141183460469231731687303715884105727"`, want: "170141183460469231731687303715884105727"},
		{name: "a bare number", input: `7`, want: "7"},
		{name: "a bool", input: `true`, wantErr: "expected a numeric ScVal"},
		{name: "an object", input: `{}`, wantErr: "expected a numeric ScVal"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var n scNumber
			err := json.Unmarshal([]byte(tc.input), &n)

			if tc.wantErr != "" {
				assert.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, n.String())
		})
	}
}

func Test_Event_SortKey(t *testing.T) {
	// Events must be applied in ledger order. The key comes out of the event
	// id, which is the (ledger, tx, op, event) cursor.
	testCases := []struct {
		name    string
		eventID string
		want    SortKey
	}{
		{
			name:    "the fixture's registered event",
			eventID: "0023788689981259776-0000000000",
			want:    SortKey{Ledger: 5538736, Tx: 20, Op: 0, Event: 0},
		},
		{
			name:    "a non-zero event index",
			eventID: "0023788711456026624-0000000001",
			want:    SortKey{Ledger: 5538741, Tx: 3, Op: 0, Event: 1},
		},
		{
			name:    "an unparseable id yields the zero key",
			eventID: "nonsense",
			want:    SortKey{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Event{EventID: tc.eventID}.SortKey())
		})
	}
}

func Test_SortKey_Cmp(t *testing.T) {
	testCases := []struct {
		name string
		a, b SortKey
		want int
	}{
		{name: "equal", a: SortKey{1, 2, 3, 4}, b: SortKey{1, 2, 3, 4}, want: 0},
		{name: "ledger dominates", a: SortKey{1, 9, 9, 9}, b: SortKey{2, 0, 0, 0}, want: -1},
		{name: "then transaction", a: SortKey{1, 2, 9, 9}, b: SortKey{1, 3, 0, 0}, want: -1},
		{name: "then operation", a: SortKey{1, 2, 3, 9}, b: SortKey{1, 2, 4, 0}, want: -1},
		{name: "then event index", a: SortKey{1, 2, 3, 4}, b: SortKey{1, 2, 3, 5}, want: -1},
		{name: "greater is symmetric", a: SortKey{2, 0, 0, 0}, b: SortKey{1, 9, 9, 9}, want: 1},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.a.Cmp(tc.b))
			assert.Equal(t, -tc.want, tc.b.Cmp(tc.a))
		})
	}
}

func Test_DecodeEvent_fixtureOrderingMatchesLedgerOrder(t *testing.T) {
	// The fixture was captured across two vaults, so this is not a claim about
	// RPC's ordering — it checks that SortKey derives a strict total order
	// from real event ids, which is what the indexer relies on to apply a
	// batch deterministically.
	raw, err := os.ReadFile("testdata/events.json")
	require.NoError(t, err)

	var infos []protocol.EventInfo
	require.NoError(t, json.Unmarshal(raw, &infos))

	seen := map[SortKey]string{}
	for _, info := range infos {
		decoded, err := DecodeEvent(info)
		require.NoError(t, err)

		key := decoded.SortKey()
		assert.NotEqual(t, SortKey{}, key, "every real event id must parse")

		previous, duplicate := seen[key]
		require.False(t, duplicate, "%s and %s share a sort key", previous, decoded.EventID)
		seen[key] = decoded.EventID
	}
}
