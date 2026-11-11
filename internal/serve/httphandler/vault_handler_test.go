package httphandler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stellar/stellar-disbursement-platform-backend/db"
	"github.com/stellar/stellar-disbursement-platform-backend/db/dbtest"
	"github.com/stellar/stellar-disbursement-platform-backend/internal/data"
	"github.com/stellar/stellar-disbursement-platform-backend/internal/vault"
)

const (
	testVaultContract = "CB5HTYC4GZW67ET23SACDCWCCDQ3AKU4TYX7SAUQJSKS6LLQLYA4HG53"
	otherContract     = "CBSZNLGKNYH456ZCGA2XLKSOHKFM43GXYMTYS6QPIB4VD3D4G3IHKRSX"
	testVaultToken    = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
	testVaultAdminKey = "GAODBHVR63Z56MVQRBEJSYM2H5423LJ4WAPUUBOFG4JYY72S6ROKVZRX"
	testVaultRcptA    = "GCNMZYNV3NVFLK25K3KLZ2LM6HAAD4BVYHULQX746XERD6HQRGGJ6SLD"
	testVaultRcptB    = "GD6KZDL3Q7DO5AV3FHR44INMTTWPRMHZVLHKGCTCIQ23OM22FHYXBLBF"
)

// newVaultHandlerTest builds a handler over a migrated database, with the
// routes mounted the way serve.go mounts them so the chi URL parameters
// resolve identically.
func newVaultHandlerTest(t *testing.T) (context.Context, *data.Models, http.Handler) {
	t.Helper()

	dbt := dbtest.Open(t)
	t.Cleanup(func() { dbt.Close() })

	dbConnectionPool, err := db.OpenDBConnectionPool(dbt.DSN)
	require.NoError(t, err)
	t.Cleanup(func() { dbConnectionPool.Close() })

	models, err := data.NewModels(dbConnectionPool)
	require.NoError(t, err)

	handler := VaultHandler{
		Models:            models,
		ContractID:        testVaultContract,
		NetworkPassphrase: "Test SDF Network ; September 2015",
	}

	router := chi.NewRouter()
	router.Route("/vault/{contract_id}", func(r chi.Router) {
		r.Get("/", handler.GetVault)
		r.Get("/allocations", handler.GetVaultAllocations)
		r.Get("/allocations/{recipient}", handler.GetVaultAllocation)
		r.Get("/events", handler.GetVaultEvents)
		r.Get("/indexer", handler.GetVaultIndexer)
		r.Post("/transactions", handler.BuildVaultTransaction)
		r.Post("/resync", handler.ResyncVault)
	})

	return context.Background(), models, router
}

func seedVault(t *testing.T, ctx context.Context, models *data.Models, deadline time.Time) {
	t.Helper()

	_, err := models.Vault.BootstrapIndexerState(ctx, models.DBConnectionPool, data.VaultIndexerState{
		ContractID: testVaultContract,
		LastCursor: "0000000000000001000-0000000000",
		LastLedger: 1000,
		Status:     data.VaultIndexerStatusHealthy,
	})
	require.NoError(t, err)

	require.NoError(t, models.Vault.UpsertState(ctx, models.DBConnectionPool, data.VaultState{
		ContractID:     testVaultContract,
		AdminAddress:   testVaultAdminKey,
		TokenAddress:   testVaultToken,
		Deadline:       deadline,
		TotalAllocated: data.NewVaultAmount(10_000_000),
		TotalClaimed:   data.NewVaultAmount(40_000_000),
		Balance:        data.NewVaultAmount(60_000_000),
		SnapshotLedger: 1000,
	}))
}

func doRequest(t *testing.T, router http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}

	request := httptest.NewRequest(method, path, reader)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	return recorder
}

// extractIndexer pulls the indexer object out of either a bare indexer
// response or a list response that embeds one.
func extractIndexer(t *testing.T, body []byte, bare bool) IndexerStatusResponse {
	t.Helper()

	if bare {
		var indexer IndexerStatusResponse
		require.NoError(t, json.Unmarshal(body, &indexer))
		return indexer
	}

	var wrapper struct {
		Indexer IndexerStatusResponse `json:"indexer"`
	}
	require.NoError(t, json.Unmarshal(body, &wrapper))
	return wrapper.Indexer
}

// ---------------------------------------------------------------------------
// GET /vault/{contract_id}
// ---------------------------------------------------------------------------

func Test_VaultHandler_GetVault(t *testing.T) {
	ctx, models, router := newVaultHandlerTest(t)
	deadline := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	seedVault(t, ctx, models, deadline)

	recorder := doRequest(t, router, http.MethodGet, "/vault/"+testVaultContract, "")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	var response VaultOverviewResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))

	assert.Equal(t, testVaultContract, response.ContractID)
	assert.Equal(t, testVaultAdminKey, response.Admin)
	assert.Equal(t, testVaultToken, response.Token)
	assert.False(t, response.IsExpired)
	// available = balance - total_allocated.
	assert.Equal(t, "50000000", response.Available.String())
	assert.Equal(t, data.VaultIndexerStatusHealthy, response.Indexer.Status)
	assert.Equal(t, int32(1000), response.Indexer.LastLedger)
}

func Test_VaultHandler_GetVault_amountsAreJSONStrings(t *testing.T) {
	// The whole point of the amount type: a JSON number would be rounded by
	// any JavaScript client, and these are i128 stroops.
	ctx, models, router := newVaultHandlerTest(t)
	seedVault(t, ctx, models, time.Now().Add(time.Hour))

	recorder := doRequest(t, router, http.MethodGet, "/vault/"+testVaultContract, "")
	require.Equal(t, http.StatusOK, recorder.Code)

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &raw))

	for _, field := range []string{"total_allocated", "total_claimed", "balance", "available"} {
		t.Run(field, func(t *testing.T) {
			value, ok := raw[field]
			require.True(t, ok, "%s must be present", field)
			assert.True(t, strings.HasPrefix(string(value), `"`),
				"%s must be a JSON string, got %s", field, value)
		})
	}
}

func Test_VaultHandler_GetVault_expiredDeadline(t *testing.T) {
	ctx, models, router := newVaultHandlerTest(t)
	seedVault(t, ctx, models, time.Now().Add(-time.Hour))

	recorder := doRequest(t, router, http.MethodGet, "/vault/"+testVaultContract, "")
	require.Equal(t, http.StatusOK, recorder.Code)

	var response VaultOverviewResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	assert.True(t, response.IsExpired, "a passed deadline must close claiming and open cancelling")
}

func Test_VaultHandler_GetVault_notIndexedYet(t *testing.T) {
	// An empty overview with zeroed amounts would read as a real, empty vault.
	// 404 is the honest answer.
	_, _, router := newVaultHandlerTest(t)

	recorder := doRequest(t, router, http.MethodGet, "/vault/"+testVaultContract, "")

	assert.Equal(t, http.StatusNotFound, recorder.Code)
	assertVaultError(t, recorder, "Vault has not been indexed yet.", "vault_not_indexed")
}

// assertVaultError checks a vault 404 body: the message is unchanged and the
// machine-readable code is added alongside it.
func assertVaultError(t *testing.T, recorder *httptest.ResponseRecorder, wantMessage, wantCode string) {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body), recorder.Body.String())
	assert.Equal(t, wantMessage, body["error"])
	assert.Equal(t, wantCode, body["error_code"])
}

func Test_VaultHandler_GetVault_reportsTheSummarySource(t *testing.T) {
	t.Run("event-derived", func(t *testing.T) {
		ctx, models, router := newVaultHandlerTest(t)
		seedVault(t, ctx, models, time.Now().Add(time.Hour))

		recorder := doRequest(t, router, http.MethodGet, "/vault/"+testVaultContract, "")
		require.Equal(t, http.StatusOK, recorder.Code)
		var raw map[string]any
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &raw))
		assert.Equal(t, "events", raw["source"])
		assert.Contains(t, raw, "bootstrap_ledger")
		assert.Nil(t, raw["bootstrap_ledger"])
	})

	t.Run("snapshot", func(t *testing.T) {
		ctx, models, router := newVaultHandlerTest(t)
		_, err := models.Vault.BootstrapIndexerState(ctx, models.DBConnectionPool, data.VaultIndexerState{
			ContractID: testVaultContract, LastLedger: 1000, Status: data.VaultIndexerStatusHealthy,
		})
		require.NoError(t, err)
		_, err = models.Vault.InsertSnapshotState(ctx, models.DBConnectionPool, data.VaultState{
			ContractID: testVaultContract, AdminAddress: testVaultAdminKey, TokenAddress: testVaultToken,
			Deadline: time.Now().Add(time.Hour).UTC(), Balance: data.NewVaultAmount(7), SnapshotLedger: 1234,
		})
		require.NoError(t, err)

		recorder := doRequest(t, router, http.MethodGet, "/vault/"+testVaultContract, "")
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		var response VaultOverviewResponse
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
		assert.Equal(t, data.VaultStateSourceSnapshot, response.Source)
		require.NotNil(t, response.BootstrapLedger)
		assert.Equal(t, int32(1234), *response.BootstrapLedger)
		assert.Equal(t, "7", response.Balance.String())
	})
}

func Test_VaultHandler_contractIDValidation(t *testing.T) {
	ctx, models, router := newVaultHandlerTest(t)
	seedVault(t, ctx, models, time.Now().Add(time.Hour))

	testCases := []struct {
		name       string
		contractID string
		wantStatus int
		wantBody   string
	}{
		{
			name:       "the configured vault",
			contractID: testVaultContract,
			wantStatus: http.StatusOK,
		},
		{
			name:       "an account address is not a contract",
			contractID: testVaultAdminKey,
			wantStatus: http.StatusBadRequest,
			wantBody:   "contract address",
		},
		{
			name:       "nonsense",
			contractID: "not-a-contract",
			wantStatus: http.StatusBadRequest,
			wantBody:   "contract address",
		},
		{
			// 404 rather than 403: from the caller's point of view that vault
			// does not exist here, and "forbidden" would confirm a contract
			// they have no business knowing about.
			name:       "a valid contract that is not this tenant's vault",
			contractID: otherContract,
			wantStatus: http.StatusNotFound,
			wantBody:   "Vault not found",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := doRequest(t, router, http.MethodGet, "/vault/"+tc.contractID, "")

			assert.Equal(t, tc.wantStatus, recorder.Code, recorder.Body.String())
			if tc.wantBody != "" {
				assert.Contains(t, recorder.Body.String(), tc.wantBody)
			}
		})
	}
}

func Test_VaultHandler_vaultNotFoundCarriesItsCodeOnEveryRoute(t *testing.T) {
	ctx, models, router := newVaultHandlerTest(t)
	seedVault(t, ctx, models, time.Now().Add(time.Hour))

	for _, route := range []struct{ method, path, body string }{
		{http.MethodGet, "", ""},
		{http.MethodGet, "/allocations", ""},
		{http.MethodGet, "/events", ""},
		{http.MethodGet, "/indexer", ""},
		{http.MethodPost, "/resync", `{"from_ledger":1}`},
	} {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			recorder := doRequest(t, router, route.method, "/vault/"+otherContract+route.path, route.body)
			require.Equal(t, http.StatusNotFound, recorder.Code)
			assertVaultError(t, recorder, "Vault not found.", "vault_not_found")
		})
	}
}

func Test_VaultHandler_buildNotIndexedCarriesItsCode(t *testing.T) {
	handler := VaultHandler{ContractID: testVaultContract}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/vault/"+testVaultContract+"/transactions", nil)

	handler.renderBuildError(recorder, request, testVaultContract, fmt.Errorf("reading state: %w", data.ErrRecordNotFound))

	require.Equal(t, http.StatusNotFound, recorder.Code)
	assertVaultError(t, recorder, "Vault has not been indexed yet, so a transaction cannot be built for it.", "vault_not_indexed")
}

// ---------------------------------------------------------------------------
// GET /vault/{contract_id}/allocations
// ---------------------------------------------------------------------------

func seedAllocations(t *testing.T, ctx context.Context, models *data.Models) {
	t.Helper()

	for i, recipient := range []string{testVaultRcptA, testVaultRcptB} {
		require.NoError(t, models.Vault.RegisterAllocation(ctx, models.DBConnectionPool, data.VaultAllocation{
			ContractID:       testVaultContract,
			Recipient:        recipient,
			Amount:           data.NewVaultAmount(int64(i+1) * 10_000_000),
			RegisteredLedger: int32(1001 + i),
			RegisteredAt:     time.Now().UTC(),
			LastEventID:      fmt.Sprintf("event-%d", i),
		}))
	}

	settled, err := models.Vault.SettleAllocation(ctx, models.DBConnectionPool,
		testVaultContract, testVaultRcptA, data.VaultAllocationStatusClaimed,
		1010, time.Now().UTC(), "event-claim")
	require.NoError(t, err)
	require.True(t, settled)
}

func Test_VaultHandler_GetVaultAllocations(t *testing.T) {
	ctx, models, router := newVaultHandlerTest(t)
	seedVault(t, ctx, models, time.Now().Add(time.Hour))
	seedAllocations(t, ctx, models)

	testCases := []struct {
		name       string
		query      string
		wantStatus int
		wantCount  int
		wantTotal  int
		wantPage   int
	}{
		{name: "no filter", query: "", wantStatus: http.StatusOK, wantCount: 2, wantTotal: 2, wantPage: 1},
		{name: "claimed only", query: "?status=CLAIMED", wantStatus: http.StatusOK, wantCount: 1, wantTotal: 1, wantPage: 1},
		{name: "pending only", query: "?status=PENDING", wantStatus: http.StatusOK, wantCount: 1, wantTotal: 1, wantPage: 1},
		// Status is upper-cased, so a lower-case query from a hand-written
		// client still works.
		{name: "lower-case status", query: "?status=claimed", wantStatus: http.StatusOK, wantCount: 1, wantTotal: 1, wantPage: 1},
		{name: "by recipient", query: "?recipient=" + testVaultRcptB, wantStatus: http.StatusOK, wantCount: 1, wantTotal: 1, wantPage: 1},
		{name: "paginated", query: "?page=2&page_limit=1", wantStatus: http.StatusOK, wantCount: 1, wantTotal: 2, wantPage: 2},
		{name: "a page past the end", query: "?page=99", wantStatus: http.StatusOK, wantCount: 0, wantTotal: 2, wantPage: 99},
		// An oversized limit is clamped rather than rejected, so it cannot
		// become a table scan served to a browser.
		{name: "oversized page_limit is clamped", query: "?page_limit=100000", wantStatus: http.StatusOK, wantCount: 2, wantTotal: 2, wantPage: 1},

		{name: "an unknown status", query: "?status=WITHDRAWN", wantStatus: http.StatusBadRequest},
		{name: "a non-numeric page", query: "?page=abc", wantStatus: http.StatusBadRequest},
		{name: "a non-numeric page_limit", query: "?page_limit=many", wantStatus: http.StatusBadRequest},
		{name: "a malformed recipient", query: "?recipient=nope", wantStatus: http.StatusBadRequest},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := doRequest(t, router, http.MethodGet,
				"/vault/"+testVaultContract+"/allocations"+tc.query, "")

			require.Equal(t, tc.wantStatus, recorder.Code, recorder.Body.String())
			if tc.wantStatus != http.StatusOK {
				return
			}

			var response VaultAllocationsResponse
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
			assert.Len(t, response.Data, tc.wantCount)
			assert.Equal(t, tc.wantTotal, response.Pagination.Total)
			assert.Equal(t, tc.wantPage, response.Pagination.Page)
		})
	}
}

func Test_VaultHandler_listResponsesCarryIndexerStatus(t *testing.T) {
	// Every list response carries `indexer`, so the UI can show a staleness or
	// degraded banner without a second request. This is why the vault does not
	// reuse the shared pagination envelope.
	ctx, models, router := newVaultHandlerTest(t)
	seedVault(t, ctx, models, time.Now().Add(time.Hour))

	require.NoError(t, models.Vault.MarkDegraded(ctx, models.DBConnectionPool,
		testVaultContract, 500, 999, "events aged out of the RPC retention window"))

	for _, path := range []string{"/allocations", "/events", "/indexer"} {
		t.Run(path, func(t *testing.T) {
			recorder := doRequest(t, router, http.MethodGet, "/vault/"+testVaultContract+path, "")
			require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

			// Parsed rather than string-matched: httpjson indents its output,
			// so matching `"status":"degraded"` would be matching the
			// formatter, not the contract.
			indexer := extractIndexer(t, recorder.Body.Bytes(), path == "/indexer")

			assert.Equal(t, data.VaultIndexerStatusDegraded, indexer.Status)
			require.NotNil(t, indexer.GapFromLedger)
			require.NotNil(t, indexer.GapToLedger)
			assert.Equal(t, int32(500), *indexer.GapFromLedger)
			assert.Equal(t, int32(999), *indexer.GapToLedger)
			require.NotNil(t, indexer.DegradedReason)
			assert.Contains(t, *indexer.DegradedReason, "retention window")
		})
	}
}

func Test_VaultHandler_GetVaultAllocation(t *testing.T) {
	ctx, models, router := newVaultHandlerTest(t)
	seedVault(t, ctx, models, time.Now().Add(time.Hour))
	seedAllocations(t, ctx, models)

	testCases := []struct {
		name       string
		recipient  string
		wantStatus int
	}{
		{name: "a known recipient", recipient: testVaultRcptA, wantStatus: http.StatusOK},
		{name: "an address with no allocation", recipient: testVaultAdminKey, wantStatus: http.StatusNotFound},
		{name: "a malformed address", recipient: "nope", wantStatus: http.StatusBadRequest},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := doRequest(t, router, http.MethodGet,
				"/vault/"+testVaultContract+"/allocations/"+tc.recipient, "")

			require.Equal(t, tc.wantStatus, recorder.Code, recorder.Body.String())
			if tc.wantStatus != http.StatusOK {
				return
			}

			var allocation data.VaultAllocation
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &allocation))
			assert.Equal(t, tc.recipient, allocation.Recipient)
			assert.Equal(t, data.VaultAllocationStatusClaimed, allocation.Status)
			assert.Equal(t, "10000000", allocation.Amount.String())

			// And the amount must be on the wire as a string, not a number.
			var raw map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &raw))
			assert.Equal(t, `"10000000"`, string(raw["amount"]))
		})
	}
}

// ---------------------------------------------------------------------------
// GET /vault/{contract_id}/events
// ---------------------------------------------------------------------------

func Test_VaultHandler_GetVaultEvents(t *testing.T) {
	ctx, models, router := newVaultHandlerTest(t)
	seedVault(t, ctx, models, time.Now().Add(time.Hour))

	amount := data.NewVaultAmount(10_000_000)
	for i, eventType := range []string{"funded", "registered", "claimed"} {
		address := testVaultRcptA
		inserted, err := models.Vault.InsertEvent(ctx, models.DBConnectionPool, data.VaultEvent{
			EventID:        fmt.Sprintf("000000000000000100%d-0000000000", i),
			ContractID:     testVaultContract,
			EventType:      eventType,
			Ledger:         int32(1001 + i),
			LedgerClosedAt: time.Now().UTC(),
			TxHash:         fmt.Sprintf("%064d", i),
			Address:        &address,
			Amount:         &amount,
			Payload:        json.RawMessage(`{}`),
		})
		require.NoError(t, err)
		require.True(t, inserted)
	}

	testCases := []struct {
		name       string
		query      string
		wantStatus int
		wantCount  int
	}{
		{name: "no filter, newest first", query: "", wantStatus: http.StatusOK, wantCount: 3},
		{name: "by event type", query: "?event_type=funded", wantStatus: http.StatusOK, wantCount: 1},
		{name: "upper-case event type", query: "?event_type=FUNDED", wantStatus: http.StatusOK, wantCount: 1},
		{name: "by address", query: "?address=" + testVaultRcptA, wantStatus: http.StatusOK, wantCount: 3},
		{name: "an address with no events", query: "?address=" + testVaultRcptB, wantStatus: http.StatusOK, wantCount: 0},

		// An unknown event_type is a 400, not an empty list: an empty list
		// would read as "no such events happened" and hide the typo.
		{name: "an unknown event type", query: "?event_type=topped_up", wantStatus: http.StatusBadRequest},
		{name: "a malformed address", query: "?address=nope", wantStatus: http.StatusBadRequest},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := doRequest(t, router, http.MethodGet,
				"/vault/"+testVaultContract+"/events"+tc.query, "")

			require.Equal(t, tc.wantStatus, recorder.Code, recorder.Body.String())
			if tc.wantStatus != http.StatusOK {
				return
			}

			var response VaultEventsResponse
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
			assert.Len(t, response.Data, tc.wantCount)
		})
	}
}

func Test_VaultHandler_GetVaultEvents_doesNotLeakThePayload(t *testing.T) {
	// The raw payload is kept for replay, not for the API. It is large and
	// duplicates the typed columns, so it must stay out of the response.
	ctx, models, router := newVaultHandlerTest(t)
	seedVault(t, ctx, models, time.Now().Add(time.Hour))

	address := testVaultRcptA
	_, err := models.Vault.InsertEvent(ctx, models.DBConnectionPool, data.VaultEvent{
		EventID:        "0000000000000001001-0000000000",
		ContractID:     testVaultContract,
		EventType:      "funded",
		Ledger:         1001,
		LedgerClosedAt: time.Now().UTC(),
		TxHash:         "abc",
		Address:        &address,
		Payload:        json.RawMessage(`{"secret_looking":"internal"}`),
	})
	require.NoError(t, err)

	recorder := doRequest(t, router, http.MethodGet, "/vault/"+testVaultContract+"/events", "")
	require.Equal(t, http.StatusOK, recorder.Code)

	assert.NotContains(t, recorder.Body.String(), "secret_looking")
	assert.NotContains(t, recorder.Body.String(), "payload")
}

// ---------------------------------------------------------------------------
// GET /vault/{contract_id}/indexer
// ---------------------------------------------------------------------------

func Test_VaultHandler_GetVaultIndexer_neverIndexed(t *testing.T) {
	// The flag may have been turned on moments ago. A 500 would be
	// misleading; a zero position with a null last_indexed_at is what tells
	// the UI nothing has been read yet.
	_, _, router := newVaultHandlerTest(t)

	recorder := doRequest(t, router, http.MethodGet, "/vault/"+testVaultContract+"/indexer", "")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	var response IndexerStatusResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	assert.Equal(t, data.VaultIndexerStatusHealthy, response.Status)
	assert.Zero(t, response.LastLedger)
	assert.Nil(t, response.LastIndexedAt)
}

// ---------------------------------------------------------------------------
// POST /vault/{contract_id}/resync
// ---------------------------------------------------------------------------

func Test_VaultHandler_ResyncVault(t *testing.T) {
	ctx, models, router := newVaultHandlerTest(t)
	seedVault(t, ctx, models, time.Now().Add(time.Hour))

	recorder := doRequest(t, router, http.MethodPost,
		"/vault/"+testVaultContract+"/resync", `{"from_ledger":500}`)
	require.Equal(t, http.StatusAccepted, recorder.Code, recorder.Body.String())

	state, err := models.Vault.GetIndexerState(ctx, models.DBConnectionPool, testVaultContract)
	require.NoError(t, err)
	// last_ledger is the last ledger *scanned*, so resuming from 500 stores
	// 499. The cursor is cleared because it is a position inside a ledger and
	// no longer corresponds to the new start point.
	assert.Equal(t, int32(499), state.LastLedger)
	assert.Empty(t, state.LastCursor)
}

func Test_VaultHandler_ResyncVault_requests(t *testing.T) {
	testCases := []struct {
		name       string
		body       string
		wantStatus int
		wantLedger int32
	}{
		{name: "an explicit ledger", body: `{"from_ledger":1000}`, wantStatus: http.StatusAccepted, wantLedger: 999},
		{name: "an empty body defaults to the retention floor", body: "", wantStatus: http.StatusAccepted, wantLedger: 0},
		{name: "an empty object", body: `{}`, wantStatus: http.StatusAccepted, wantLedger: 0},
		{name: "ledger 0", body: `{"from_ledger":0}`, wantStatus: http.StatusAccepted, wantLedger: 0},

		{name: "a negative ledger", body: `{"from_ledger":-5}`, wantStatus: http.StatusBadRequest},
		{name: "malformed JSON", body: `{`, wantStatus: http.StatusBadRequest},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, models, router := newVaultHandlerTest(t)
			seedVault(t, ctx, models, time.Now().Add(time.Hour))

			recorder := doRequest(t, router, http.MethodPost,
				"/vault/"+testVaultContract+"/resync", tc.body)

			require.Equal(t, tc.wantStatus, recorder.Code, recorder.Body.String())
			if tc.wantStatus != http.StatusAccepted {
				return
			}

			state, err := models.Vault.GetIndexerState(ctx, models.DBConnectionPool, testVaultContract)
			require.NoError(t, err)
			assert.Equal(t, tc.wantLedger, state.LastLedger)
		})
	}
}

func Test_VaultHandler_ResyncVault_notIndexedYet(t *testing.T) {
	_, _, router := newVaultHandlerTest(t)

	recorder := doRequest(t, router, http.MethodPost,
		"/vault/"+testVaultContract+"/resync", `{"from_ledger":500}`)

	assert.Equal(t, http.StatusNotFound, recorder.Code)
	assertVaultError(t, recorder, "Vault has not been indexed yet, so there is nothing to resync.", "vault_not_indexed")
}

// ---------------------------------------------------------------------------
// POST /vault/{contract_id}/transactions
// ---------------------------------------------------------------------------

func Test_VaultHandler_BuildVaultTransaction_withoutABuilder(t *testing.T) {
	// Reachable only if the vault is enabled without RPC, which startup
	// validation rejects. It must be handled rather than panic.
	ctx, models, router := newVaultHandlerTest(t)
	seedVault(t, ctx, models, time.Now().Add(time.Hour))

	recorder := doRequest(t, router, http.MethodPost,
		"/vault/"+testVaultContract+"/transactions", `{"operation":"fund","amount":"10000000"}`)

	assert.Equal(t, http.StatusInternalServerError, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "not configured")
}

func Test_VaultHandler_BuildVaultTransaction_rejectsASecretKeyInTheBody(t *testing.T) {
	// A secret key must never be accepted, logged or echoed. The request is
	// refused before anything is built, and the body is not reflected back.
	ctx, models, router := newVaultHandlerTest(t)
	seedVault(t, ctx, models, time.Now().Add(time.Hour))

	// Deliberately not a real key: the S prefix the validator keys on, with an
	// all-A body and therefore an invalid checksum.
	const secretLooking = "S" + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	recorder := doRequest(t, router, http.MethodPost,
		"/vault/"+testVaultContract+"/transactions",
		fmt.Sprintf(`{"operation":"fund","amount":"10000000","source":%q}`, secretLooking))

	// The builder is nil in this harness, so the request cannot succeed; what
	// matters is that the key never appears in the response.
	assert.NotContains(t, recorder.Body.String(), secretLooking)
}

// ---------------------------------------------------------------------------
// GET /vault/{contract_id}/status
// ---------------------------------------------------------------------------

// fakeTipReader is a scripted chain-tip source for the status handler.
type fakeTipReader struct {
	ledger uint32
	err    error
}

func (f fakeTipReader) Get(context.Context) (uint32, error) { return f.ledger, f.err }

// hangingTipRPC accepts the call and never answers until its context ends:
// the RPC failure mode a plain error test misses.
type hangingTipRPC struct{}

func (hangingTipRPC) GetLatestLedgerSequence(ctx context.Context) (uint32, error) {
	<-ctx.Done()
	return 0, ctx.Err()
}

var statusThresholds = vault.HealthThresholds{LaggingLedgers: 60, StallAfter: 300 * time.Second}

func newVaultStatusTest(t *testing.T, tip LatestLedgerReader, now time.Time) (context.Context, *data.Models, http.Handler) {
	t.Helper()

	dbt := dbtest.Open(t)
	t.Cleanup(func() { dbt.Close() })
	dbConnectionPool, err := db.OpenDBConnectionPool(dbt.DSN)
	require.NoError(t, err)
	t.Cleanup(func() { dbConnectionPool.Close() })
	models, err := data.NewModels(dbConnectionPool)
	require.NoError(t, err)

	handler := VaultHandler{
		Models:           models,
		ContractID:       testVaultContract,
		LatestLedger:     tip,
		HealthThresholds: statusThresholds,
		Now:              func() time.Time { return now },
	}
	router := chi.NewRouter()
	router.Route("/vault/{contract_id}", func(r chi.Router) {
		r.Get("/status", handler.GetVaultStatus)
	})
	return context.Background(), models, router
}

// seededCursor is where seedIndexedVault leaves the indexer.
const seededCursor int32 = 1000

// seedIndexedVault bootstraps the indexer at seededCursor and commits once, so
// last_indexed_at is set to the database's NOW().
func seedIndexedVault(t *testing.T, ctx context.Context, models *data.Models) {
	t.Helper()
	cursor := seededCursor
	_, err := models.Vault.BootstrapIndexerState(ctx, models.DBConnectionPool, data.VaultIndexerState{
		ContractID: testVaultContract, LastCursor: "", LastLedger: cursor, Status: data.VaultIndexerStatusHealthy,
	})
	require.NoError(t, err)
	require.NoError(t, models.Vault.AdvanceCursor(ctx, models.DBConnectionPool, testVaultContract,
		fmt.Sprintf("%019d-0000000000", uint64(cursor)<<32), cursor))
}

func getStatus(t *testing.T, router http.Handler) (int, map[string]any) {
	t.Helper()
	recorder := doRequest(t, router, http.MethodGet, "/vault/"+testVaultContract+"/status", "")
	var body map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body), recorder.Body.String())
	return recorder.Code, body
}

func Test_VaultHandler_GetVaultStatus_ok(t *testing.T) {
	now := time.Now().UTC()
	ctx, models, router := newVaultStatusTest(t, fakeTipReader{ledger: 1010}, now)
	seedIndexedVault(t, ctx, models)

	closedAt := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	amount := data.NewVaultAmount(1)
	address := testVaultAdminKey
	_, err := models.Vault.InsertEvent(ctx, models.DBConnectionPool, data.VaultEvent{
		EventID: "0000004294967296000-0000000000", ContractID: testVaultContract, EventType: "funded",
		Ledger: 999, LedgerClosedAt: closedAt, TxHash: fmt.Sprintf("%064d", 999),
		Address: &address, Amount: &amount, Payload: json.RawMessage(`{}`),
	})
	require.NoError(t, err)

	code, body := getStatus(t, router)
	require.Equal(t, http.StatusOK, code)

	assert.Equal(t, testVaultContract, body["contract_id"])
	assert.Equal(t, "ok", body["status"])
	assert.Equal(t, "none", body["reason"])
	assert.NotEmpty(t, body["detail"])
	assert.Equal(t, false, body["degraded"])
	assert.EqualValues(t, 1000, body["cursor_ledger"])
	assert.EqualValues(t, 1010, body["rpc_latest_ledger"])
	assert.EqualValues(t, 10, body["lag_ledgers"])
	assert.EqualValues(t, 50, body["lag_seconds_estimate"])
	assert.EqualValues(t, 120_950, body["retention_headroom"])
	assert.Equal(t, "2026-11-11T09:00:00Z", body["last_event_at"])
	assert.Equal(t, now.Format(time.RFC3339Nano), body["checked_at"])
	assert.Equal(t, map[string]any{
		"state": "missing", "bootstrap_ledger": nil,
		"bootstrap": map[string]any{"attempts": float64(0), "last_error": nil, "next_attempt_at": nil},
	}, body["summary"])
	assert.Len(t, body, 13)
}

func Test_VaultHandler_GetVaultStatus_summary(t *testing.T) {
	t.Run("a snapshot summary reports its ledger", func(t *testing.T) {
		ctx, models, router := newVaultStatusTest(t, fakeTipReader{ledger: 1010}, time.Now().UTC())
		seedIndexedVault(t, ctx, models)
		_, err := models.Vault.InsertSnapshotState(ctx, models.DBConnectionPool, data.VaultState{
			ContractID: testVaultContract, AdminAddress: testVaultAdminKey, TokenAddress: testVaultToken,
			Deadline: time.Now().Add(time.Hour).UTC(), SnapshotLedger: 1005,
		})
		require.NoError(t, err)

		code, body := getStatus(t, router)
		require.Equal(t, http.StatusOK, code)
		summary := body["summary"].(map[string]any)
		assert.Equal(t, "snapshot", summary["state"])
		assert.EqualValues(t, 1005, summary["bootstrap_ledger"])
	})

	t.Run("a failing snapshot reports its category and next attempt, never raw text", func(t *testing.T) {
		ctx, models, router := newVaultStatusTest(t, fakeTipReader{ledger: 1010}, time.Now().UTC())
		seedIndexedVault(t, ctx, models)
		next := time.Date(2026, 11, 11, 12, 0, 20, 0, time.UTC)
		require.NoError(t, models.Vault.RecordSnapshotFailure(ctx, models.DBConnectionPool, testVaultContract,
			data.SnapshotErrorRPCUnreachable, next))

		code, body := getStatus(t, router)
		require.Equal(t, http.StatusOK, code)
		summary := body["summary"].(map[string]any)
		assert.Equal(t, "missing", summary["state"])
		assert.Equal(t, map[string]any{
			"attempts": float64(1), "last_error": "rpc_unreachable", "next_attempt_at": "2026-11-11T12:00:20Z",
		}, summary["bootstrap"])
		// The reason enum is unchanged: summary state is reported separately.
		assert.Equal(t, "ok", body["status"])
	})
}

func Test_VaultHandler_GetVaultStatus_transitions(t *testing.T) {
	// The DB stamps last_indexed_at with its own NOW(), so "now" for the
	// handler is expressed relative to the moment of seeding.
	secretErr := errors.New(`Post "https://rpc.example/?apikey=SUPERSECRET": dial tcp: connection refused`)

	testCases := []struct {
		name       string
		seed       bool
		tip        LatestLedgerReader
		nowOffset  time.Duration
		wantStatus string
		wantReason string
	}{
		{"lag at threshold is lagging", true, fakeTipReader{ledger: 1060}, 0, "lagging", "lag_over_threshold"},
		{"lag threshold-1 is ok", true, fakeTipReader{ledger: 1059}, 0, "ok", "none"},
		{"no commit for longer than the stall threshold", true, fakeTipReader{ledger: 1000}, 10 * time.Minute, "stalled", "no_recent_commit"},
		{"never indexed", false, fakeTipReader{ledger: 1000}, 0, "stalled", "never_indexed"},
		{"rpc unreachable", true, fakeTipReader{err: secretErr}, 0, "stalled", "rpc_unreachable"},
		{"no tip source configured", true, nil, 0, "stalled", "rpc_unreachable"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().Add(tc.nowOffset)
			ctx, models, router := newVaultStatusTest(t, tc.tip, now)
			if tc.seed {
				seedIndexedVault(t, ctx, models)
			}

			code, body := getStatus(t, router)
			require.Equal(t, http.StatusOK, code, "health results are never a 5xx")
			assert.Equal(t, tc.wantStatus, body["status"])
			assert.Equal(t, tc.wantReason, body["reason"])

			if tc.wantReason == "rpc_unreachable" {
				for _, key := range []string{"rpc_latest_ledger", "lag_ledgers", "lag_seconds_estimate", "retention_headroom"} {
					assert.Contains(t, body, key)
					assert.Nil(t, body[key], "%s is null, not zero, when RPC is down", key)
				}
				assert.EqualValues(t, 1000, body["cursor_ledger"], "database fields are still served")
			}
			if !tc.seed {
				assert.Nil(t, body["cursor_ledger"])
			}
		})
	}

	t.Run("the RPC error text is never echoed", func(t *testing.T) {
		ctx, models, router := newVaultStatusTest(t, fakeTipReader{err: secretErr}, time.Now())
		seedIndexedVault(t, ctx, models)
		recorder := doRequest(t, router, http.MethodGet, "/vault/"+testVaultContract+"/status", "")
		assert.NotContains(t, recorder.Body.String(), "SUPERSECRET")
		assert.NotContains(t, recorder.Body.String(), "rpc.example")
	})
}

func Test_VaultHandler_GetVaultStatus_hungRPCThroughTheRealCache(t *testing.T) {
	cache, err := vault.NewLatestLedgerCache(hangingTipRPC{}, 5*time.Second, 50*time.Millisecond, nil)
	require.NoError(t, err)
	ctx, models, router := newVaultStatusTest(t, cache, time.Now())
	seedIndexedVault(t, ctx, models)

	start := time.Now()
	code, body := getStatus(t, router)
	assert.Less(t, time.Since(start), 2*time.Second)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "stalled", body["status"])
	assert.Equal(t, "rpc_unreachable", body["reason"])
}

func Test_VaultHandler_GetVaultStatus_contractScoping(t *testing.T) {
	_, _, router := newVaultStatusTest(t, fakeTipReader{ledger: 1}, time.Now())

	recorder := doRequest(t, router, http.MethodGet, "/vault/"+otherContract+"/status", "")
	assert.Equal(t, http.StatusNotFound, recorder.Code, "another contract does not exist on this tenant")

	recorder = doRequest(t, router, http.MethodGet, "/vault/not-a-contract/status", "")
	assert.Equal(t, http.StatusBadRequest, recorder.Code)
}
