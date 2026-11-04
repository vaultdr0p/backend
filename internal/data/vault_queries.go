package data

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/stellar/stellar-disbursement-platform-backend/db"
)

// Read queries for the vault endpoints.
//
// These take explicit typed filter structs rather than the generic
// QueryParams/FilterKey map used by the older models. The map form is
// untyped — `map[FilterKey]interface{}` — and its value shapes are only
// documented by the query builder that consumes them; for four filters across
// two tables, naming them in a struct lets the compiler check the call sites
// and keeps the SQL readable. Pagination still uses the same Page/PageLimit
// semantics the dashboard already sends.

// VaultAllocationFilter narrows a list of allocations. Zero values mean "no
// filter", so the zero struct lists everything for the contract.
type VaultAllocationFilter struct {
	ContractID string
	Status     VaultAllocationStatus
	Recipient  string
}

// VaultEventFilter narrows a list of events.
type VaultEventFilter struct {
	ContractID string
	EventType  string
	Address    string
}

// VaultPage is a page request. Values outside the supported range are clamped
// rather than rejected: a bad page size is not worth failing a read over, and
// clamping keeps an unbounded `page_limit=100000` from turning into a table
// scan served to a browser.
type VaultPage struct {
	Page  int
	Limit int
}

const (
	defaultVaultPageLimit = 20
	maxVaultPageLimit     = 100
)

// normalise clamps the page request and returns the SQL offset.
func (p VaultPage) normalise() (page, limit, offset int) {
	page, limit = p.Page, p.Limit
	if page < 1 {
		page = 1
	}
	switch {
	case limit <= 0:
		limit = defaultVaultPageLimit
	case limit > maxVaultPageLimit:
		limit = maxVaultPageLimit
	}
	return page, limit, (page - 1) * limit
}

// where builds the shared predicate for allocation queries. Returning the
// clause and its arguments together keeps the count and the page query using
// exactly the same filter — the usual bug here is a total that does not match
// the rows.
func (f VaultAllocationFilter) where() (string, []any) {
	conditions := []string{"contract_id = $1"}
	args := []any{f.ContractID}

	if f.Status != "" {
		args = append(args, f.Status)
		conditions = append(conditions, fmt.Sprintf("status = $%d", len(args)))
	}
	if f.Recipient != "" {
		args = append(args, f.Recipient)
		conditions = append(conditions, fmt.Sprintf("recipient = $%d", len(args)))
	}

	return " WHERE " + strings.Join(conditions, " AND "), args
}

func (f VaultEventFilter) where() (string, []any) {
	conditions := []string{"contract_id = $1"}
	args := []any{f.ContractID}

	if f.EventType != "" {
		args = append(args, f.EventType)
		conditions = append(conditions, fmt.Sprintf("event_type = $%d", len(args)))
	}
	if f.Address != "" {
		args = append(args, f.Address)
		conditions = append(conditions, fmt.Sprintf("address = $%d", len(args)))
	}

	return " WHERE " + strings.Join(conditions, " AND "), args
}

const vaultAllocationFields = `
	contract_id,
	recipient,
	amount,
	status,
	registered_ledger,
	registered_at,
	settled_ledger,
	settled_at,
	last_event_id,
	created_at,
	updated_at
`

// CountAllocations returns how many allocations match the filter.
func (m VaultModel) CountAllocations(ctx context.Context, sqlExec db.SQLExecuter, filter VaultAllocationFilter) (int, error) {
	whereClause, args := filter.where()

	var count int
	if err := sqlExec.GetContext(ctx, &count, `SELECT COUNT(*) FROM vault_allocations`+whereClause, args...); err != nil {
		return 0, fmt.Errorf("counting vault allocations: %w", err)
	}

	return count, nil
}

// ListAllocations returns one page of allocations.
//
// The ordering is (registered_ledger DESC, recipient) rather than just the
// ledger: ledger alone is not unique — a single transaction can register
// several recipients — and a non-deterministic tiebreak makes rows repeat or
// vanish between pages.
func (m VaultModel) ListAllocations(ctx context.Context, sqlExec db.SQLExecuter, filter VaultAllocationFilter, page VaultPage) ([]VaultAllocation, error) {
	whereClause, args := filter.where()
	_, limit, offset := page.normalise()

	query := `SELECT ` + vaultAllocationFields + ` FROM vault_allocations` + whereClause +
		fmt.Sprintf(` ORDER BY registered_ledger DESC, recipient ASC LIMIT $%d OFFSET $%d`, len(args)+1, len(args)+2)
	args = append(args, limit, offset)

	allocations := []VaultAllocation{}
	if err := sqlExec.SelectContext(ctx, &allocations, query, args...); err != nil {
		return nil, fmt.Errorf("listing vault allocations: %w", err)
	}

	return allocations, nil
}

// GetAllocation reads one recipient's allocation, returning ErrRecordNotFound
// when the recipient has none.
func (m VaultModel) GetAllocation(ctx context.Context, sqlExec db.SQLExecuter, contractID, recipient string) (*VaultAllocation, error) {
	const query = `SELECT ` + vaultAllocationFields + ` FROM vault_allocations WHERE contract_id = $1 AND recipient = $2`

	var allocation VaultAllocation
	if err := sqlExec.GetContext(ctx, &allocation, query, contractID, recipient); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("vault allocation for %s: %w", recipient, ErrRecordNotFound)
		}
		return nil, fmt.Errorf("getting vault allocation: %w", err)
	}

	return &allocation, nil
}

const vaultEventFields = `
	id,
	event_id,
	contract_id,
	event_type,
	ledger,
	ledger_closed_at,
	tx_hash,
	address,
	amount,
	payload,
	created_at
`

// CountEvents returns how many events match the filter.
func (m VaultModel) CountEvents(ctx context.Context, sqlExec db.SQLExecuter, filter VaultEventFilter) (int, error) {
	whereClause, args := filter.where()

	var count int
	if err := sqlExec.GetContext(ctx, &count, `SELECT COUNT(*) FROM vault_events`+whereClause, args...); err != nil {
		return 0, fmt.Errorf("counting vault events: %w", err)
	}

	return count, nil
}

// ListEvents returns one page of the event history, newest first.
//
// Ordered by (ledger DESC, id DESC) to match
// vault_events_contract_ledger_idx, so the page is served from the index. The
// id tiebreak is both stable and meaningful: it is the insertion order, which
// within a ledger is the order the events were read in.
func (m VaultModel) ListEvents(ctx context.Context, sqlExec db.SQLExecuter, filter VaultEventFilter, page VaultPage) ([]VaultEvent, error) {
	whereClause, args := filter.where()
	_, limit, offset := page.normalise()

	query := `SELECT ` + vaultEventFields + ` FROM vault_events` + whereClause +
		fmt.Sprintf(` ORDER BY ledger DESC, id DESC LIMIT $%d OFFSET $%d`, len(args)+1, len(args)+2)
	args = append(args, limit, offset)

	events := []VaultEvent{}
	if err := sqlExec.SelectContext(ctx, &events, query, args...); err != nil {
		return nil, fmt.Errorf("listing vault events: %w", err)
	}

	return events, nil
}

// CountPages converts a total and a page request into a page count, which the
// pagination envelope reports. A total of zero yields zero pages, not one.
func CountPages(total int, page VaultPage) int {
	_, limit, _ := page.normalise()
	if total <= 0 {
		return 0
	}
	return (total + limit - 1) / limit
}

// NormalisedVaultPage reports the page and limit a request will actually use
// after clamping, so a response can echo back the real values rather than
// whatever the client asked for.
func NormalisedVaultPage(page VaultPage) VaultPage {
	normalisedPage, limit, _ := page.normalise()
	return VaultPage{Page: normalisedPage, Limit: limit}
}
