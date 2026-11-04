package vault

import (
	"encoding/json"
	"fmt"
	"strings"
)

// This file decodes the subset of the ScVal JSON representation that vault
// events actually use.
//
// Asking RPC for `xdrFormat: "json"` makes it return `topicJson` / `valueJson`
// instead of base64 XDR, so SDP does not have to carry an ScVal decoder. The
// shapes below were captured from real responses out of
// soroban-testnet.stellar.org — see internal/vault/testdata/events.json, which
// is verbatim RPC output and is what the decoder tests run against:
//
//	topicJson: [{"symbol": "registered"}, {"address": "GCNM...SLD"}]
//	valueJson: {"map": [
//	    {"key": {"symbol": "amount"},          "val": {"i128": "10000000"}},
//	    {"key": {"symbol": "total_allocated"}, "val": {"i128": "10000000"}}
//	]}
//
// Two properties of that encoding matter:
//
//   - 64-bit and wider integers (u64, i128) arrive as JSON **strings**, so they
//     survive the range that a JSON number could not. scNumber accepts a bare
//     number too, defensively, but the amount parser still rejects anything
//     that is not an exact integer.
//   - map entries are ordered by key, but nothing here depends on that: every
//     field is looked up by name, so adding a field to a contract event cannot
//     shift the meaning of an existing one.

// scVal is the subset of an ScVal's JSON form that vault events contain.
// Fields are pointers so that "absent" is distinguishable from "present and
// empty" — a `{"symbol": ""}` is a malformed event, not a missing topic.
type scVal struct {
	Symbol  *string      `json:"symbol,omitempty"`
	Address *string      `json:"address,omitempty"`
	String  *string      `json:"string,omitempty"`
	I128    *scNumber    `json:"i128,omitempty"`
	U64     *scNumber    `json:"u64,omitempty"`
	U32     *scNumber    `json:"u32,omitempty"`
	Map     []scMapEntry `json:"map,omitempty"`
}

type scMapEntry struct {
	Key scVal `json:"key"`
	Val scVal `json:"val"`
}

// scNumber holds a numeric ScVal as its raw decimal text, accepting both the
// JSON string form RPC emits for wide integers and a bare JSON number.
// Keeping it as text rather than parsing to int64 here is deliberate: an i128
// does not fit in any Go integer, and the caller is the right place to decide
// the target type.
type scNumber string

func (n *scNumber) UnmarshalJSON(b []byte) error {
	var asString string
	if err := json.Unmarshal(b, &asString); err == nil {
		*n = scNumber(asString)
		return nil
	}

	var asNumber json.Number
	if err := json.Unmarshal(b, &asNumber); err != nil {
		return fmt.Errorf("expected a numeric ScVal as a string or number, got %s", truncateForError(b))
	}
	*n = scNumber(asNumber.String())
	return nil
}

func (n scNumber) String() string { return string(n) }

// symbol returns the ScVal's symbol, or an error naming what was found
// instead. The error path exists because a topic of the wrong type means the
// event is not shaped the way the contract emits it.
func (v scVal) symbol() (string, error) {
	if v.Symbol == nil {
		return "", fmt.Errorf("expected a symbol, got %s", v.kind())
	}
	return *v.Symbol, nil
}

func (v scVal) address() (string, error) {
	if v.Address == nil {
		return "", fmt.Errorf("expected an address, got %s", v.kind())
	}
	return *v.Address, nil
}

// kind names the ScVal variant present, for error messages. It reports
// "nothing" for an empty object rather than guessing, so a decode failure says
// what it actually saw.
func (v scVal) kind() string {
	var kinds []string
	if v.Symbol != nil {
		kinds = append(kinds, "symbol")
	}
	if v.Address != nil {
		kinds = append(kinds, "address")
	}
	if v.String != nil {
		kinds = append(kinds, "string")
	}
	if v.I128 != nil {
		kinds = append(kinds, "i128")
	}
	if v.U64 != nil {
		kinds = append(kinds, "u64")
	}
	if v.U32 != nil {
		kinds = append(kinds, "u32")
	}
	if v.Map != nil {
		kinds = append(kinds, "map")
	}
	if len(kinds) == 0 {
		return "nothing"
	}
	return strings.Join(kinds, "+")
}

// eventData is a decoded event data map, keyed by field name.
type eventData map[string]scVal

// decodeEventData reads the event's `valueJson` into a field map.
//
// The contract's #[contractevent] structs with more than one non-topic field
// serialise as an ScMap. A single-field event could in principle arrive as a
// bare value, but the contract has none: `claimed`, `cancelled` and
// `withdrawn` each emit a one-entry map, which the captured fixture confirms.
// A non-map value is therefore rejected rather than guessed at.
func decodeEventData(raw json.RawMessage) (eventData, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("event has no valueJson: ask RPC for xdrFormat=json")
	}

	var val scVal
	if err := json.Unmarshal(raw, &val); err != nil {
		return nil, fmt.Errorf("unmarshalling event data: %w", err)
	}
	if val.Map == nil {
		return nil, fmt.Errorf("expected event data to be a map, got %s", val.kind())
	}

	fields := make(eventData, len(val.Map))
	for i, entry := range val.Map {
		key, err := entry.Key.symbol()
		if err != nil {
			return nil, fmt.Errorf("event data key %d: %w", i, err)
		}
		if _, exists := fields[key]; exists {
			// ScMap keys are unique by construction, so a duplicate means the
			// response was not produced by the host.
			return nil, fmt.Errorf("event data has a duplicate key %q", key)
		}
		fields[key] = entry.Val
	}

	return fields, nil
}

// requireI128 reads a required i128 field as its decimal text.
func (d eventData) requireI128(field string) (string, error) {
	val, ok := d[field]
	if !ok {
		return "", fmt.Errorf("event data is missing field %q", field)
	}
	if val.I128 == nil {
		return "", fmt.Errorf("event data field %q: expected an i128, got %s", field, val.kind())
	}
	return val.I128.String(), nil
}

// requireU64 reads a required u64 field as its decimal text.
func (d eventData) requireU64(field string) (string, error) {
	val, ok := d[field]
	if !ok {
		return "", fmt.Errorf("event data is missing field %q", field)
	}
	if val.U64 == nil {
		return "", fmt.Errorf("event data field %q: expected a u64, got %s", field, val.kind())
	}
	return val.U64.String(), nil
}

// requireAddress reads a required address field.
func (d eventData) requireAddress(field string) (string, error) {
	val, ok := d[field]
	if !ok {
		return "", fmt.Errorf("event data is missing field %q", field)
	}
	addr, err := val.address()
	if err != nil {
		return "", fmt.Errorf("event data field %q: %w", field, err)
	}
	return addr, nil
}

// truncateForError keeps a malformed payload out of the logs at full length
// while still saying enough to identify it.
func truncateForError(b []byte) string {
	const limit = 64
	if len(b) <= limit {
		return string(b)
	}
	return string(b[:limit]) + "…"
}
