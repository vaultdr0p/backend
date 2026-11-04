package httphandler

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/support/http/httpdecode"
	"github.com/stellar/go-stellar-sdk/support/render/httpjson"

	"github.com/stellar/stellar-disbursement-platform-backend/internal/data"
	"github.com/stellar/stellar-disbursement-platform-backend/internal/serve/httperror"
	"github.com/stellar/stellar-disbursement-platform-backend/internal/vault"
)

// VaultHandler serves the disbursement vault endpoints.
//
// Every route is read-only with one exception, and even that one does not
// move money: POST /transactions returns an **unsigned** envelope for the
// browser to sign with the admin's wallet. The backend holds no vault signing
// key and has no field that accepts one, so there is no code path on which it
// could sign a vault operation. That guarantee is structural, not procedural.
//
// Routes are registered only when ENABLE_VAULT is true. With the flag off they
// do not exist, so they return 404 rather than 403 — that is deliberate, so a
// dashboard can feature-detect the vault with a single call.
type VaultHandler struct {
	Models *data.Models
	// ContractID is the tenant's configured vault. Requests for any other
	// contract are refused rather than served, so a caller cannot use this
	// API to index or inspect an arbitrary contract.
	ContractID string
	// NetworkPassphrase is echoed on built envelopes so a wallet can refuse
	// to sign for the wrong network.
	NetworkPassphrase string
	// TransactionBuilder builds unsigned envelopes. Nil when the vault is
	// enabled without RPC, which startup validation rejects.
	TransactionBuilder *vault.TransactionBuilder
}

// ---------------------------------------------------------------------------
// Response types
//
// These mirror the OpenAPI spec in docs/VAULT_DESIGN.md. They deliberately do
// not reuse httpresponse.PaginatedResponse: the spec puts an `indexer` object
// alongside `data` and `pagination` on every list response, so the UI can show
// a staleness or degraded banner without a second request, and the shared
// envelope has no room for it.
//
// Every amount is a string. data.VaultAmount marshals that way by
// construction, because these are i128 stroops and a JSON number would be
// rounded by any JavaScript client.
// ---------------------------------------------------------------------------

// IndexerStatusResponse reports how fresh the indexed view is, and whether it
// is known to be incomplete.
type IndexerStatusResponse struct {
	Status         data.VaultIndexerStatus `json:"status"`
	LastLedger     int32                   `json:"last_ledger"`
	LatestLedger   int32                   `json:"latest_ledger"`
	LagLedgers     int32                   `json:"lag_ledgers"`
	LastIndexedAt  *time.Time              `json:"last_indexed_at"`
	GapFromLedger  *int32                  `json:"gap_from_ledger"`
	GapToLedger    *int32                  `json:"gap_to_ledger"`
	DegradedReason *string                 `json:"degraded_reason"`
}

// VaultOverviewResponse is the vault's cached snapshot plus indexer health.
type VaultOverviewResponse struct {
	ContractID     string                `json:"contract_id"`
	Admin          string                `json:"admin"`
	Token          string                `json:"token"`
	Deadline       time.Time             `json:"deadline"`
	IsExpired      bool                  `json:"is_expired"`
	TotalAllocated data.VaultAmount      `json:"total_allocated"`
	TotalClaimed   data.VaultAmount      `json:"total_claimed"`
	Balance        data.VaultAmount      `json:"balance"`
	Available      data.VaultAmount      `json:"available"`
	SnapshotLedger int32                 `json:"snapshot_ledger"`
	RefreshedAt    time.Time             `json:"refreshed_at"`
	Indexer        IndexerStatusResponse `json:"indexer"`
}

// VaultPaginationResponse matches the spec's pagination object.
type VaultPaginationResponse struct {
	Total int `json:"total"`
	Page  int `json:"page"`
	Pages int `json:"pages"`
}

// VaultAllocationsResponse is one page of allocations.
type VaultAllocationsResponse struct {
	Data       []data.VaultAllocation  `json:"data"`
	Pagination VaultPaginationResponse `json:"pagination"`
	Indexer    IndexerStatusResponse   `json:"indexer"`
}

// VaultEventsResponse is one page of the event history.
type VaultEventsResponse struct {
	Data       []data.VaultEvent       `json:"data"`
	Pagination VaultPaginationResponse `json:"pagination"`
	Indexer    IndexerStatusResponse   `json:"indexer"`
}

// ---------------------------------------------------------------------------
// Shared request plumbing
// ---------------------------------------------------------------------------

// contractIDFromRequest reads and validates the {contract_id} path parameter.
//
// A contract that is not the tenant's configured vault returns 404, not 403:
// from the caller's point of view that vault does not exist on this tenant,
// and saying "forbidden" would confirm the existence of a contract the caller
// has no business knowing about.
func (h VaultHandler) contractIDFromRequest(r *http.Request) (string, *httperror.HTTPError) {
	contractID := strings.TrimSpace(chi.URLParam(r, "contract_id"))

	if !strkey.IsValidContractAddress(contractID) {
		return "", httperror.BadRequest("", nil, map[string]interface{}{
			"contract_id": "contract_id must be a Stellar contract address (C...)",
		})
	}
	if contractID != h.ContractID {
		return "", httperror.NotFound("Vault not found.", nil, nil)
	}

	return contractID, nil
}

// pageFromRequest reads the page and page_limit query parameters.
//
// Both are clamped rather than rejected: an out-of-range page size is not
// worth failing a read over, and clamping stops `page_limit=100000` becoming a
// table scan served to a browser. A non-numeric value *is* rejected, because
// silently treating `page=abc` as page 1 hides a client bug.
func pageFromRequest(r *http.Request) (data.VaultPage, *httperror.HTTPError) {
	page := data.VaultPage{}
	query := r.URL.Query()

	if raw := query.Get("page"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return page, httperror.BadRequest("", err, map[string]interface{}{
				"page": "page must be an integer",
			})
		}
		page.Page = parsed
	}

	if raw := query.Get("page_limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return page, httperror.BadRequest("", err, map[string]interface{}{
				"page_limit": "page_limit must be an integer",
			})
		}
		page.Limit = parsed
	}

	return data.NormalisedVaultPage(page), nil
}

// optionalAddress validates an address query parameter if present.
func optionalAddress(r *http.Request, name string) (string, *httperror.HTTPError) {
	value := strings.TrimSpace(r.URL.Query().Get(name))
	if value == "" {
		return "", nil
	}

	if !isStellarAddress(value) {
		return "", httperror.BadRequest("", nil, map[string]interface{}{
			name: fmt.Sprintf("%s must be a Stellar account (G...) or contract (C...) address", name),
		})
	}

	return value, nil
}

// isStellarAddress accepts both address kinds a recipient can be. Restricting
// to accounts would reject a perfectly valid allocation to a contract.
func isStellarAddress(address string) bool {
	return strkey.IsValidEd25519PublicKey(address) || strkey.IsValidContractAddress(address)
}

// indexerStatus reads the indexer row and shapes it for a response.
//
// A vault that has never been indexed is reported as healthy with a zero
// position rather than as an error: the flag may have been turned on moments
// ago, and a 500 would be misleading. The zero last_ledger and a null
// last_indexed_at are what tell the UI nothing has been read yet.
func (h VaultHandler) indexerStatus(r *http.Request, contractID string) (IndexerStatusResponse, error) {
	ctx := r.Context()

	state, err := h.Models.Vault.GetIndexerState(ctx, h.Models.DBConnectionPool, contractID)
	if errors.Is(err, data.ErrRecordNotFound) {
		return IndexerStatusResponse{Status: data.VaultIndexerStatusHealthy}, nil
	}
	if err != nil {
		return IndexerStatusResponse{}, fmt.Errorf("reading vault indexer state: %w", err)
	}

	status := IndexerStatusResponse{
		Status:         state.Status,
		LastLedger:     state.LastLedger,
		LatestLedger:   state.LastLedger,
		LastIndexedAt:  state.LastIndexedAt,
		GapFromLedger:  state.GapFromLedger,
		GapToLedger:    state.GapToLedger,
		DegradedReason: state.DegradedReason,
	}

	// lag_ledgers is reported as zero rather than guessed. The chain tip is
	// not stored, and calling RPC on every read to compute a lag the indexer
	// already publishes as a metric would add a network round trip to every
	// dashboard page load. The metric is the right place for lag; this field
	// exists for the spec's shape and reflects the stored position.
	status.LagLedgers = 0

	return status, nil
}

// ---------------------------------------------------------------------------
// GET /vault/{contract_id}
// ---------------------------------------------------------------------------

// GetVault serves the vault overview from the cached snapshot.
func (h VaultHandler) GetVault(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	contractID, httpErr := h.contractIDFromRequest(r)
	if httpErr != nil {
		httpErr.Render(w)
		return
	}

	state, err := h.Models.Vault.GetState(ctx, h.Models.DBConnectionPool, contractID)
	if errors.Is(err, data.ErrRecordNotFound) {
		// The vault is configured but its `initialized` event has not been
		// indexed yet, so there is nothing to show. 404 is honest here; an
		// empty overview with zeroed amounts would read as a real, empty
		// vault.
		httperror.NotFound("Vault has not been indexed yet.", err, nil).Render(w)
		return
	}
	if err != nil {
		httperror.InternalError(ctx, "Cannot read vault state", err, nil).Render(w)
		return
	}

	indexer, err := h.indexerStatus(r, contractID)
	if err != nil {
		httperror.InternalError(ctx, "Cannot read vault indexer state", err, nil).Render(w)
		return
	}

	httpjson.Render(w, VaultOverviewResponse{
		ContractID:     state.ContractID,
		Admin:          state.AdminAddress,
		Token:          state.TokenAddress,
		Deadline:       state.Deadline,
		IsExpired:      state.IsExpired(time.Now()),
		TotalAllocated: state.TotalAllocated,
		TotalClaimed:   state.TotalClaimed,
		Balance:        state.Balance,
		Available:      state.Available(),
		SnapshotLedger: state.SnapshotLedger,
		RefreshedAt:    state.RefreshedAt,
		Indexer:        indexer,
	}, httpjson.JSON)
}

// ---------------------------------------------------------------------------
// GET /vault/{contract_id}/allocations
// ---------------------------------------------------------------------------

// GetVaultAllocations lists allocations with their claim status.
func (h VaultHandler) GetVaultAllocations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	contractID, httpErr := h.contractIDFromRequest(r)
	if httpErr != nil {
		httpErr.Render(w)
		return
	}

	page, httpErr := pageFromRequest(r)
	if httpErr != nil {
		httpErr.Render(w)
		return
	}

	recipient, httpErr := optionalAddress(r, "recipient")
	if httpErr != nil {
		httpErr.Render(w)
		return
	}

	filter := data.VaultAllocationFilter{ContractID: contractID, Recipient: recipient}

	if raw := strings.TrimSpace(r.URL.Query().Get("status")); raw != "" {
		status := data.VaultAllocationStatus(strings.ToUpper(raw))
		switch status {
		case data.VaultAllocationStatusPending, data.VaultAllocationStatusClaimed, data.VaultAllocationStatusCancelled:
			filter.Status = status
		default:
			httperror.BadRequest("", nil, map[string]interface{}{
				"status": "status must be one of PENDING, CLAIMED, CANCELLED",
			}).Render(w)
			return
		}
	}

	total, err := h.Models.Vault.CountAllocations(ctx, h.Models.DBConnectionPool, filter)
	if err != nil {
		httperror.InternalError(ctx, "Cannot count vault allocations", err, nil).Render(w)
		return
	}

	allocations, err := h.Models.Vault.ListAllocations(ctx, h.Models.DBConnectionPool, filter, page)
	if err != nil {
		httperror.InternalError(ctx, "Cannot list vault allocations", err, nil).Render(w)
		return
	}

	indexer, err := h.indexerStatus(r, contractID)
	if err != nil {
		httperror.InternalError(ctx, "Cannot read vault indexer state", err, nil).Render(w)
		return
	}

	httpjson.Render(w, VaultAllocationsResponse{
		Data: allocations,
		Pagination: VaultPaginationResponse{
			Total: total,
			Page:  page.Page,
			Pages: data.CountPages(total, page),
		},
		Indexer: indexer,
	}, httpjson.JSON)
}

// ---------------------------------------------------------------------------
// GET /vault/{contract_id}/allocations/{recipient}
// ---------------------------------------------------------------------------

// GetVaultAllocation reads one recipient's allocation.
func (h VaultHandler) GetVaultAllocation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	contractID, httpErr := h.contractIDFromRequest(r)
	if httpErr != nil {
		httpErr.Render(w)
		return
	}

	recipient := strings.TrimSpace(chi.URLParam(r, "recipient"))
	if !isStellarAddress(recipient) {
		httperror.BadRequest("", nil, map[string]interface{}{
			"recipient": "recipient must be a Stellar account (G...) or contract (C...) address",
		}).Render(w)
		return
	}

	allocation, err := h.Models.Vault.GetAllocation(ctx, h.Models.DBConnectionPool, contractID, recipient)
	if errors.Is(err, data.ErrRecordNotFound) {
		httperror.NotFound("No allocation for this address.", err, nil).Render(w)
		return
	}
	if err != nil {
		httperror.InternalError(ctx, "Cannot read vault allocation", err, nil).Render(w)
		return
	}

	httpjson.Render(w, allocation, httpjson.JSON)
}

// ---------------------------------------------------------------------------
// GET /vault/{contract_id}/events
// ---------------------------------------------------------------------------

// GetVaultEvents lists the indexed event history, newest first.
func (h VaultHandler) GetVaultEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	contractID, httpErr := h.contractIDFromRequest(r)
	if httpErr != nil {
		httpErr.Render(w)
		return
	}

	page, httpErr := pageFromRequest(r)
	if httpErr != nil {
		httpErr.Render(w)
		return
	}

	address, httpErr := optionalAddress(r, "address")
	if httpErr != nil {
		httpErr.Render(w)
		return
	}

	filter := data.VaultEventFilter{ContractID: contractID, Address: address}

	if raw := strings.TrimSpace(r.URL.Query().Get("event_type")); raw != "" {
		eventType := vault.EventType(strings.ToLower(raw))
		if !vault.IsKnownEventType(eventType) {
			httperror.BadRequest("", nil, map[string]interface{}{
				"event_type": fmt.Sprintf("event_type must be one of %s", strings.Join(vault.KnownEventTypeNames(), ", ")),
			}).Render(w)
			return
		}
		filter.EventType = string(eventType)
	}

	total, err := h.Models.Vault.CountEvents(ctx, h.Models.DBConnectionPool, filter)
	if err != nil {
		httperror.InternalError(ctx, "Cannot count vault events", err, nil).Render(w)
		return
	}

	events, err := h.Models.Vault.ListEvents(ctx, h.Models.DBConnectionPool, filter, page)
	if err != nil {
		httperror.InternalError(ctx, "Cannot list vault events", err, nil).Render(w)
		return
	}

	indexer, err := h.indexerStatus(r, contractID)
	if err != nil {
		httperror.InternalError(ctx, "Cannot read vault indexer state", err, nil).Render(w)
		return
	}

	httpjson.Render(w, VaultEventsResponse{
		Data: events,
		Pagination: VaultPaginationResponse{
			Total: total,
			Page:  page.Page,
			Pages: data.CountPages(total, page),
		},
		Indexer: indexer,
	}, httpjson.JSON)
}

// ---------------------------------------------------------------------------
// GET /vault/{contract_id}/indexer
// ---------------------------------------------------------------------------

// GetVaultIndexer serves indexer health on its own, for an operator or a
// monitoring check that does not want the whole overview.
func (h VaultHandler) GetVaultIndexer(w http.ResponseWriter, r *http.Request) {
	contractID, httpErr := h.contractIDFromRequest(r)
	if httpErr != nil {
		httpErr.Render(w)
		return
	}

	indexer, err := h.indexerStatus(r, contractID)
	if err != nil {
		httperror.InternalError(r.Context(), "Cannot read vault indexer state", err, nil).Render(w)
		return
	}

	httpjson.Render(w, indexer, httpjson.JSON)
}

// ---------------------------------------------------------------------------
// POST /vault/{contract_id}/resync
// ---------------------------------------------------------------------------

// ResyncVaultRequest rewinds the indexer.
type ResyncVaultRequest struct {
	// FromLedger is where to resume from. Zero means the RPC retention floor,
	// which is as far back as a replay can usefully go.
	FromLedger int32 `json:"from_ledger"`
}

// ResyncVault rewinds the cursor so the indexer replays.
//
// This is safe at any time, which is the point: event application is
// idempotent on event_id, so a replay converges to the same state rather than
// double-counting. That is what makes resync an operational tool rather than a
// risk, and why it needs no confirmation step.
func (h VaultHandler) ResyncVault(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	contractID, httpErr := h.contractIDFromRequest(r)
	if httpErr != nil {
		httpErr.Render(w)
		return
	}

	var request ResyncVaultRequest
	if r.Body != nil && r.ContentLength > 0 {
		if err := httpdecode.DecodeJSON(r, &request); err != nil {
			httperror.BadRequest("Invalid request body.", err, nil).Render(w)
			return
		}
	}
	if request.FromLedger < 0 {
		httperror.BadRequest("", nil, map[string]interface{}{
			"from_ledger": "from_ledger cannot be negative",
		}).Render(w)
		return
	}

	// last_ledger is the last ledger *scanned*, so resuming from N means
	// storing N-1. The cursor is cleared because it is a position inside a
	// ledger and no longer corresponds to the new start point.
	resumeAfter := request.FromLedger - 1
	if resumeAfter < 0 {
		resumeAfter = 0
	}

	err := h.Models.Vault.AdvanceCursor(ctx, h.Models.DBConnectionPool, contractID, "", resumeAfter)
	if errors.Is(err, data.ErrRecordNotFound) {
		httperror.NotFound("Vault has not been indexed yet, so there is nothing to resync.", err, nil).Render(w)
		return
	}
	if err != nil {
		httperror.InternalError(ctx, "Cannot resync vault", err, nil).Render(w)
		return
	}

	w.WriteHeader(http.StatusAccepted)
	httpjson.Render(w, map[string]any{
		"message":     "Resync scheduled. The indexer will replay from the next run.",
		"from_ledger": resumeAfter + 1,
	}, httpjson.JSON)
}

// ---------------------------------------------------------------------------
// POST /vault/{contract_id}/transactions
// ---------------------------------------------------------------------------

// BuildVaultTransaction returns an unsigned, pre-simulated envelope for the
// requested operation.
//
// The response is inert without the admin's signature, which SDP does not
// have and cannot obtain. The role gate on this route is therefore defence in
// depth rather than the control that protects the funds — the contract's own
// require_auth() is.
func (h VaultHandler) BuildVaultTransaction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	contractID, httpErr := h.contractIDFromRequest(r)
	if httpErr != nil {
		httpErr.Render(w)
		return
	}

	if h.TransactionBuilder == nil {
		// Reachable only if the vault is enabled without RPC, which
		// ValidateRPC rejects at startup. Handled rather than panicking.
		httperror.InternalError(ctx, "Vault transaction building is not configured",
			errors.New("vault is enabled but no transaction builder was configured"), nil).Render(w)
		return
	}

	var request vault.BuildRequest
	if err := httpdecode.DecodeJSON(r, &request); err != nil {
		httperror.BadRequest("Invalid request body.", err, nil).Render(w)
		return
	}

	response, err := h.TransactionBuilder.Build(ctx, request)
	if err != nil {
		h.renderBuildError(w, r, contractID, err)
		return
	}

	httpjson.Render(w, response, httpjson.JSON)
}

// renderBuildError maps a build failure onto the right status code.
//
// A contract rejection is a 400, not a 500: the request was well-formed but
// the contract would not accept it, and the operator needs to see *which*
// contract rule they hit. Returning 500 would hide an actionable message
// behind "an internal error occurred".
func (h VaultHandler) renderBuildError(w http.ResponseWriter, r *http.Request, contractID string, err error) {
	ctx := r.Context()

	var validationErr *vault.ValidationError
	if errors.As(err, &validationErr) {
		httperror.BadRequest("", err, map[string]interface{}{
			validationErr.Field: validationErr.Message,
		}).Render(w)
		return
	}

	var contractErr *vault.ContractError
	if errors.As(err, &contractErr) {
		extras := map[string]interface{}{"detail": contractErr.Message}
		if contractErr.Code > 0 {
			extras["contract_error_code"] = contractErr.Code
			extras["contract_error"] = contractErr.Name
		}
		httperror.BadRequest(contractErr.Message, err, extras).Render(w)
		return
	}

	if errors.Is(err, data.ErrRecordNotFound) {
		httperror.NotFound("Vault has not been indexed yet, so a transaction cannot be built for it.", err, nil).Render(w)
		return
	}

	httperror.InternalError(ctx, "Cannot build vault transaction", err, map[string]interface{}{
		"contract_id": contractID,
	}).Render(w)
}
