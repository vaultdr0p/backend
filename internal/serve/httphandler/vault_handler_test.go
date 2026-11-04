package httphandler

import (
	"context"
	"encoding/json"
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
	assert.Contains(t, recorder.Body.String(), "not been indexed")
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
