package stellar

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	protocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_RPCClientWrapper_GetEvents_uninitialisedClient(t *testing.T) {
	// The zero value must fail loudly rather than nil-panic, matching how
	// SimulateTransaction and GetLatestLedgerSequence behave.
	w := &RPCClientWrapper{}

	resp, err := w.GetEvents(context.Background(), protocol.GetEventsRequest{})

	assert.Nil(t, resp)
	require.Error(t, err)
	assert.EqualError(t, err, "RPC client not initialized")
}

func Test_RPCClientWrapper_GetEvents(t *testing.T) {
	testCases := []struct {
		name            string
		rpcStatus       int
		rpcBody         string
		wantErrContains string
		assertResponse  func(t *testing.T, resp *protocol.GetEventsResponse)
	}{
		{
			name:      "🟢 decodes events, cursor and the retention bounds",
			rpcStatus: http.StatusOK,
			rpcBody: `{"jsonrpc":"2.0","id":1,"result":{
				"events":[{
					"type":"contract",
					"ledger":5523000,
					"ledgerClosedAt":"2026-11-02T12:00:00Z",
					"contractId":"CB5HTYC4GZW67ET23SACDCWCCDQ3AKU4TYX7SAUQJSKS6LLQLYA4HG53",
					"id":"0005523000-0000000001",
					"operationIndex":0,
					"transactionIndex":1,
					"txHash":"abc123",
					"inSuccessfulContractCall":true,
					"topicJson":[{"symbol":"claimed"}],
					"valueJson":{"map":[]}
				}],
				"cursor":"0005523000-0000000001",
				"latestLedger":5523037,
				"oldestLedger":5402078,
				"latestLedgerCloseTime":"1793664000",
				"oldestLedgerCloseTime":"1793059152"
			}}`,
			assertResponse: func(t *testing.T, resp *protocol.GetEventsResponse) {
				require.Len(t, resp.Events, 1)
				assert.Equal(t, int32(5523000), resp.Events[0].Ledger)
				assert.Equal(t, "abc123", resp.Events[0].TransactionHash)
				assert.Equal(t, protocol.EventTypeContract, resp.Events[0].EventType)
				assert.Equal(t, "0005523000-0000000001", resp.Events[0].ID)
				assert.Equal(t, "0005523000-0000000001", resp.Cursor)
				// These two are what the indexer compares its cursor against to
				// detect a retention gap, so they must survive decoding.
				assert.Equal(t, uint32(5523037), resp.LatestLedger)
				assert.Equal(t, uint32(5402078), resp.OldestLedger)
			},
		},
		{
			name:      "🟢 an empty page is not an error",
			rpcStatus: http.StatusOK,
			rpcBody: `{"jsonrpc":"2.0","id":1,"result":{
				"events":[],"cursor":"","latestLedger":100,"oldestLedger":1,
				"latestLedgerCloseTime":"1","oldestLedgerCloseTime":"1"
			}}`,
			assertResponse: func(t *testing.T, resp *protocol.GetEventsResponse) {
				assert.Empty(t, resp.Events)
				assert.Equal(t, uint32(100), resp.LatestLedger)
			},
		},
		{
			name:      "🔴 a startLedger outside the retention window is surfaced",
			rpcStatus: http.StatusOK,
			rpcBody: `{"jsonrpc":"2.0","id":1,"error":{"code":-32600,
				"message":"startLedger must be within the ledger range: 5402065 - 5523024"}}`,
			wantErrContains: "startLedger must be within the ledger range",
		},
		{
			name:            "🔴 a transport failure is wrapped",
			rpcStatus:       http.StatusInternalServerError,
			rpcBody:         `boom`,
			wantErrContains: "getting contract events",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Assert the wrapper really issues a getEvents JSON-RPC call.
				var body map[string]any
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				assert.Equal(t, "getEvents", body["method"])
				// Filtering to type "contract" is what excludes failed-call
				// noise: diagnostic events are the ones that describe
				// failures, and contract events are only emitted by
				// successful invocations.
				params, _ := body["params"].(map[string]any)
				filters, _ := params["filters"].([]any)
				require.Len(t, filters, 1)
				filter, _ := filters[0].(map[string]any)
				assert.Contains(t, filter["type"], protocol.EventTypeContract)

				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.rpcStatus)
				_, writeErr := w.Write([]byte(tc.rpcBody))
				require.NoError(t, writeErr)
			}))
			defer server.Close()

			wrapper := NewRPCClientWrapper(server.URL, server.Client())

			resp, err := wrapper.GetEvents(context.Background(), protocol.GetEventsRequest{
				Filters: []protocol.EventFilter{{
					EventType:   protocol.EventTypeSet{"contract": nil},
					ContractIDs: []string{"CB5HTYC4GZW67ET23SACDCWCCDQ3AKU4TYX7SAUQJSKS6LLQLYA4HG53"},
				}},
				Pagination: &protocol.PaginationOptions{Limit: 200},
			})

			if tc.wantErrContains != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErrContains)
				assert.Nil(t, resp)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, resp)
			tc.assertResponse(t, resp)
		})
	}
}

// The retention constants drive the lag alert, so pin them against the values
// measured from the configured provider. If RPC's retention default ever
// changes, this test is where it should be noticed.
func Test_EventRetentionWindowConstants(t *testing.T) {
	const (
		ledgersPerDay = 17_280 // at the 5.00s average close time measured
		expectedDays  = 7
	)

	assert.Equal(t, uint32(120_960), EventRetentionWindowLedgers,
		"retention window should be the RPC EVENT_LEDGER_RETENTION_WINDOW default")
	assert.Equal(t, uint32(expectedDays*ledgersPerDay), EventRetentionWindowLedgers,
		"120,960 ledgers should be exactly 7 days at 17,280 ledgers/day")

	assert.Equal(t, uint32(60_480), EventRetentionLagWarnLedgers,
		"warn threshold should be half the retention window")
	assert.Equal(t, EventRetentionWindowLedgers/2, EventRetentionLagWarnLedgers)

	// Sanity: warning must leave real time to react, not fire at the cliff.
	assert.Less(t, EventRetentionLagWarnLedgers, EventRetentionWindowLedgers)
	assert.Equal(t, 3.5, float64(EventRetentionLagWarnLedgers)/float64(ledgersPerDay),
		"warn threshold should correspond to 3.5 days of lag")
}
