package vault

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	protocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stellar/go-stellar-sdk/strkey"

	"github.com/stellar/stellar-disbursement-platform-backend/internal/data"
)

// EventType is the name the contract publishes as an event's first topic. The
// values are the snake_case forms of the #[contractevent] struct names in the
// disbursement-vault contract, and they are also the values the
// vault_events_type_check constraint accepts — so adding one here without
// adding it to a migration will fail loudly at insert time rather than
// silently storing an unexpected type.
type EventType string

const (
	EventTypeInitialized EventType = "initialized"
	EventTypeFunded      EventType = "funded"
	EventTypeRegistered  EventType = "registered"
	EventTypeClaimed     EventType = "claimed"
	EventTypeCancelled   EventType = "cancelled"
	EventTypeWithdrawn   EventType = "withdrawn"
)

// ErrUnknownEventType marks an event the contract did not emit in any version
// this code knows about.
//
// The indexer **skips** these rather than failing the batch. That choice is
// load-bearing: a contract upgrade that adds a seventh event would otherwise
// wedge the indexer permanently, because the cursor could never advance past
// the first unrecognised event and every subsequent poll would re-fetch and
// re-fail it.
//
// A skipped event is counted and logged but **not stored**:
// vault_events_type_check only accepts the six known types, so recording it
// would need a migration anyway. Recovering one therefore means adding the
// type and replaying — which is possible only while the event is still inside
// the RPC retention window. That is the trade: a wedged indexer loses
// everything after the unknown event, whereas skipping loses only the unknown
// events themselves.
var ErrUnknownEventType = errors.New("unknown vault event type")

// knownEventTypes is the set an incoming event name is checked against.
var knownEventTypes = map[EventType]struct{}{
	EventTypeInitialized: {},
	EventTypeFunded:      {},
	EventTypeRegistered:  {},
	EventTypeClaimed:     {},
	EventTypeCancelled:   {},
	EventTypeWithdrawn:   {},
}

// Event is one decoded vault contract event.
//
// The shape is flat rather than a per-type struct because every consumer
// either projects it (and switches on Type anyway) or writes it to a single
// row. Fields that only some events carry are pointers, so "this event has no
// amount" and "this event has a zero amount" stay distinguishable — the
// difference between an `initialized` event and a corrupt `funded` one.
type Event struct {
	// EventID is RPC's event id: the stringified (ledger, tx, op, event)
	// cursor. It is unique and stable for the life of the network, which is
	// what makes it usable as the idempotency key.
	EventID        string
	ContractID     string
	Type           EventType
	Ledger         int32
	LedgerClosedAt time.Time
	TxHash         string

	// Address is the event's indexed topic: the recipient for registered,
	// claimed and cancelled; the funder for funded; the destination for
	// withdrawn; the admin for initialized.
	Address string

	// Amount is present on every event except initialized.
	Amount *data.VaultAmount

	// Balance is the vault's token balance after a funded event.
	Balance *data.VaultAmount
	// TotalAllocated is the encumbrance after a registered event.
	TotalAllocated *data.VaultAmount
	// Token and Deadline come from initialized.
	Token    string
	Deadline *time.Time

	// Payload is the event's topic and data as RPC returned them, stored in
	// vault_events.payload. Keeping the original means a projection bug can be
	// fixed and replayed from the database, without needing RPC to still have
	// the event — which, past the retention window, it will not.
	Payload []byte
}

// SortKey orders events the way the ledger did: by ledger, then transaction,
// then operation, then event index. RPC already returns them in this order,
// and the indexer re-sorts by it anyway so that ordering is a property of the
// code rather than an assumption about the server.
type SortKey struct {
	Ledger uint32
	Tx     uint32
	Op     uint32
	Event  uint32
}

// Cmp orders two keys, with the usual -1/0/+1 convention.
func (k SortKey) Cmp(other SortKey) int {
	for _, pair := range [][2]uint32{
		{k.Ledger, other.Ledger},
		{k.Tx, other.Tx},
		{k.Op, other.Op},
		{k.Event, other.Event},
	} {
		switch {
		case pair[0] < pair[1]:
			return -1
		case pair[0] > pair[1]:
			return 1
		}
	}
	return 0
}

// SortKey derives the event's ledger ordering from its id. The id is the
// cursor, so this is a parse rather than a lookup; a malformed id yields the
// zero key, which only happens on a response that is not RPC's.
func (e Event) SortKey() SortKey {
	cursor, err := protocol.ParseCursor(e.EventID)
	if err != nil {
		return SortKey{}
	}
	return SortKey{Ledger: cursor.Ledger, Tx: cursor.Tx, Op: cursor.Op, Event: cursor.Event}
}

// eventPayload is what gets stored in vault_events.payload: the event exactly
// as RPC presented it, minus the fields already in their own columns.
type eventPayload struct {
	Topic []json.RawMessage `json:"topic"`
	Value json.RawMessage   `json:"value"`
	TxIdx uint32            `json:"transaction_index"`
	OpIdx uint32            `json:"operation_index"`
}

// DecodeEvent turns one RPC event into a vault Event.
//
// It is strict about everything it can check locally — event type, topic
// arity, field presence, field type, address format, amount range — because
// this is the boundary where an external response becomes a database row.
// Anything that gets past here will be projected into balances.
func DecodeEvent(info protocol.EventInfo) (*Event, error) {
	// Only contract events are ever requested, so anything else means the
	// filter was built wrong. Checking here keeps a mis-filtered diagnostic
	// event from being projected as real state.
	if info.EventType != protocol.EventTypeContract {
		return nil, fmt.Errorf("expected a %q event, got %q", protocol.EventTypeContract, info.EventType)
	}
	if info.ID == "" {
		return nil, errors.New("event has no id, so it cannot be deduplicated")
	}
	if _, err := protocol.ParseCursor(info.ID); err != nil {
		return nil, fmt.Errorf("event id %q is not a cursor: %w", info.ID, err)
	}
	if !strkey.IsValidContractAddress(info.ContractID) {
		return nil, fmt.Errorf("event contract id %q is not a contract address", info.ContractID)
	}

	// Every vault event has exactly two topics: the event name and one
	// indexed address.
	if len(info.TopicJSON) != 2 {
		return nil, fmt.Errorf("expected 2 topics, got %d: ask RPC for xdrFormat=json", len(info.TopicJSON))
	}

	var nameTopic scVal
	if err := json.Unmarshal(info.TopicJSON[0], &nameTopic); err != nil {
		return nil, fmt.Errorf("unmarshalling event name topic: %w", err)
	}
	name, err := nameTopic.symbol()
	if err != nil {
		return nil, fmt.Errorf("event name topic: %w", err)
	}

	eventType := EventType(name)
	if _, known := knownEventTypes[eventType]; !known {
		return nil, fmt.Errorf("%q: %w", name, ErrUnknownEventType)
	}

	var addressTopic scVal
	if err := json.Unmarshal(info.TopicJSON[1], &addressTopic); err != nil {
		return nil, fmt.Errorf("unmarshalling event address topic: %w", err)
	}
	address, err := addressTopic.address()
	if err != nil {
		return nil, fmt.Errorf("event address topic: %w", err)
	}
	if !isStellarAddress(address) {
		return nil, fmt.Errorf("event address topic %q is not a Stellar address", address)
	}

	closedAt, err := time.Parse(time.RFC3339, info.LedgerClosedAt)
	if err != nil {
		return nil, fmt.Errorf("parsing ledgerClosedAt %q: %w", info.LedgerClosedAt, err)
	}

	fields, err := decodeEventData(info.ValueJSON)
	if err != nil {
		return nil, fmt.Errorf("decoding %s event data: %w", name, err)
	}

	payload, err := json.Marshal(eventPayload{
		Topic: info.TopicJSON,
		Value: info.ValueJSON,
		TxIdx: info.TxIndex,
		OpIdx: info.OpIndex,
	})
	if err != nil {
		return nil, fmt.Errorf("marshalling %s event payload: %w", name, err)
	}

	event := Event{
		EventID:        info.ID,
		ContractID:     info.ContractID,
		Type:           eventType,
		Ledger:         info.Ledger,
		LedgerClosedAt: closedAt.UTC(),
		TxHash:         info.TransactionHash,
		Address:        address,
		Payload:        payload,
	}

	if err := event.decodeFields(fields); err != nil {
		return nil, fmt.Errorf("decoding %s event: %w", name, err)
	}

	return &event, nil
}

// decodeFields reads the per-type data fields. Each branch requires exactly
// the fields the contract's event struct declares, so a renamed or dropped
// field is a decode error rather than a silently zeroed amount.
func (e *Event) decodeFields(fields eventData) error {
	switch e.Type {
	case EventTypeInitialized:
		token, err := fields.requireAddress("token")
		if err != nil {
			return err
		}
		if !strkey.IsValidContractAddress(token) {
			return fmt.Errorf("token %q is not a contract address", token)
		}
		e.Token = token

		rawDeadline, err := fields.requireU64("deadline")
		if err != nil {
			return err
		}
		deadline, err := parseUnixSeconds(rawDeadline)
		if err != nil {
			return fmt.Errorf("deadline: %w", err)
		}
		e.Deadline = &deadline

	case EventTypeFunded:
		if err := e.setAmount(fields, "amount", &e.Amount); err != nil {
			return err
		}
		if err := e.setAmount(fields, "balance", &e.Balance); err != nil {
			return err
		}

	case EventTypeRegistered:
		if err := e.setAmount(fields, "amount", &e.Amount); err != nil {
			return err
		}
		if err := e.setAmount(fields, "total_allocated", &e.TotalAllocated); err != nil {
			return err
		}

	case EventTypeClaimed, EventTypeCancelled, EventTypeWithdrawn:
		if err := e.setAmount(fields, "amount", &e.Amount); err != nil {
			return err
		}

	default:
		// Unreachable: DecodeEvent rejects unknown types before this runs.
		return fmt.Errorf("%q: %w", e.Type, ErrUnknownEventType)
	}

	// The contract only ever emits positive amounts — register and fund both
	// reject a non-positive amount with InvalidAmount — so a negative one here
	// means a decoding or encoding fault, not a legitimate value. It would
	// also violate vault_allocations_amount_check on insert; failing here says
	// why.
	if e.Amount != nil && e.Amount.IsNegative() {
		return fmt.Errorf("amount %s is negative", e.Amount)
	}

	return nil
}

func (e *Event) setAmount(fields eventData, field string, target **data.VaultAmount) error {
	raw, err := fields.requireI128(field)
	if err != nil {
		return err
	}
	amount, err := data.ParseVaultAmount(raw)
	if err != nil {
		return fmt.Errorf("field %q: %w", field, err)
	}
	*target = &amount
	return nil
}

// parseUnixSeconds converts the contract's u64 deadline to a time. The bound
// check is not pedantry: a u64 can hold values far outside the range time.Time
// renders sensibly, and a nonsense deadline would be stored as a nonsense
// TIMESTAMPTZ and shown to an operator as fact.
func parseUnixSeconds(raw string) (time.Time, error) {
	amount, err := data.ParseVaultAmount(raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing unix seconds: %w", err)
	}
	if !amount.BigInt().IsInt64() {
		return time.Time{}, fmt.Errorf("%s is not a representable unix timestamp", raw)
	}
	seconds := amount.BigInt().Int64()
	if seconds <= 0 {
		return time.Time{}, fmt.Errorf("%s is not a positive unix timestamp", raw)
	}
	return time.Unix(seconds, 0).UTC(), nil
}

// isStellarAddress accepts both address kinds a vault event topic can carry: a
// classic account (G...) and a contract (C...). Recipients may be either, so
// restricting to accounts would reject a perfectly valid allocation to a
// contract.
func isStellarAddress(address string) bool {
	return strkey.IsValidEd25519PublicKey(address) || strkey.IsValidContractAddress(address)
}
