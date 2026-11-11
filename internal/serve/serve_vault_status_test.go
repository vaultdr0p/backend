package serve

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/stellar/stellar-disbursement-platform-backend/stellar-auth/pkg/auth"
)

func validVaultStatusOptions() ServeOptions {
	return ServeOptions{
		EnableVault:                 true,
		VaultContractID:             vaultRoutesTestContract,
		VaultIndexerIntervalSeconds: 10,
		VaultStatusLaggingLedgers:   60,
		VaultStatusStallSeconds:     300,
		VaultStatusRPCCacheSeconds:  5,
	}
}

func Test_ServeOptions_validateVaultStatus(t *testing.T) {
	testCases := []struct {
		name    string
		mutate  func(o *ServeOptions)
		wantErr string
	}{
		{"defaults are valid", func(*ServeOptions) {}, ""},

		{"lagging 0 is rejected", func(o *ServeOptions) { o.VaultStatusLaggingLedgers = 0 }, "lagging threshold must be at least 1"},
		{"lagging 1 is accepted", func(o *ServeOptions) { o.VaultStatusLaggingLedgers = 1 }, ""},
		{"lagging window-1 is accepted", func(o *ServeOptions) { o.VaultStatusLaggingLedgers = 120_959 }, ""},
		{"lagging at the window is rejected", func(o *ServeOptions) { o.VaultStatusLaggingLedgers = 120_960 }, "below the RPC event retention window"},
		{"negative lagging is rejected", func(o *ServeOptions) { o.VaultStatusLaggingLedgers = -1 }, "lagging threshold"},

		{"stall equal to the indexer interval is rejected", func(o *ServeOptions) { o.VaultStatusStallSeconds = 10 }, "longer than the vault indexer interval"},
		{"stall interval+1 is accepted", func(o *ServeOptions) { o.VaultStatusStallSeconds = 11 }, ""},
		{"stall tracks a custom interval", func(o *ServeOptions) {
			o.VaultIndexerIntervalSeconds = 400
			o.VaultStatusStallSeconds = 300
		}, "longer than the vault indexer interval (400s)"},

		{"cache 0 is rejected", func(o *ServeOptions) { o.VaultStatusRPCCacheSeconds = 0 }, "between 1 and 60 seconds"},
		{"cache 1 is accepted", func(o *ServeOptions) { o.VaultStatusRPCCacheSeconds = 1 }, ""},
		{"cache 60 is accepted", func(o *ServeOptions) { o.VaultStatusRPCCacheSeconds = 60 }, ""},
		{"cache 61 is rejected", func(o *ServeOptions) { o.VaultStatusRPCCacheSeconds = 61 }, "between 1 and 60 seconds"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			opts := validVaultStatusOptions()
			tc.mutate(&opts)

			err := opts.validateVaultStatus()
			if tc.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.wantErr)
			}
		})
	}

	t.Run("ValidateRPC applies it when the vault is enabled", func(t *testing.T) {
		opts := validVaultStatusOptions()
		opts.RPCConfig.RPCUrl = "http://localhost:8000"
		opts.VaultStatusRPCCacheSeconds = 0
		require.ErrorContains(t, opts.ValidateRPC(), "between 1 and 60 seconds")
	})

	t.Run("ValidateRPC ignores it when the vault is off", func(t *testing.T) {
		opts := ServeOptions{VaultStatusRPCCacheSeconds: 0}
		require.NoError(t, opts.ValidateRPC())
	})
}

// Route-level coverage for /status (flag off 404, no credentials 401, the
// role matrix and API key permissions) lives in serve_vault_routes_test.go,
// where /status sits in the same table as every other vault route. This test
// adds the one thing that table cannot see: the body /status returns through
// the real mux.
func Test_handleHTTP_vaultStatus_answersStalledThroughTheRealMux(t *testing.T) {
	const token = "token-owner"
	authMock := &auth.AuthManagerMock{}
	authMock.On("GetUserID", mock.Anything, token).Return("user-owner", nil)
	authMock.On("GetTenantID", mock.Anything, token).Return("tenant1", nil)
	authMock.On("AnyRolesInTokenUser", mock.Anything, token, mock.Anything).Return(true, nil)

	mux := vaultRoutesHandler(t, getConnectionPool(t), true, authMock)
	w := vaultRouteRequest(t, mux, vaultRoute{method: http.MethodGet, path: "/status"}, "Bearer "+token)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	// The test options build no RPC cache, so the honest answer is stalled /
	// rpc_unreachable — as a 200, never a 5xx.
	assert.Equal(t, "stalled", body["status"])
	assert.Equal(t, "rpc_unreachable", body["reason"])
	assert.Equal(t, vaultRoutesTestContract, body["contract_id"])
	assert.Nil(t, body["lag_ledgers"])
}
