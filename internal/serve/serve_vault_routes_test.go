package serve

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/stellar/stellar-disbursement-platform-backend/db"
	"github.com/stellar/stellar-disbursement-platform-backend/internal/data"
	"github.com/stellar/stellar-disbursement-platform-backend/internal/serve/middleware"
	"github.com/stellar/stellar-disbursement-platform-backend/pkg/schema"
	"github.com/stellar/stellar-disbursement-platform-backend/stellar-auth/pkg/auth"
	"github.com/stellar/stellar-disbursement-platform-backend/stellar-multitenant/pkg/tenant"
)

// These tests drive the real handleHTTP mux, so they pin where the vault
// routes are mounted, not only what their handlers do. The handler tests in
// httphandler mount the handlers on a bare router and could not see that the
// routes had been registered outside the authenticated group — where
// RequirePermission and AnyRoleMiddleware find no API key or token in the
// context and reject every caller with 401.

const vaultRoutesTestContract = "CB5HTYC4GZW67ET23SACDCWCCDQ3AKU4TYX7SAUQJSKS6LLQLYA4HG53"

type vaultRoute struct {
	method string
	path   string
	roles  []data.UserRole
	perm   data.APIKeyPermission
}

var (
	vaultReadRoles = []data.UserRole{
		data.OwnerUserRole, data.FinancialControllerUserRole, data.BusinessUserRole,
		data.InitiatorUserRole, data.ApproverUserRole,
	}
	// vaultRoutes mirrors the role lists in serve.go. If either changes, this
	// table must change with it, deliberately.
	vaultRoutes = []vaultRoute{
		{http.MethodGet, "", vaultReadRoles, data.ReadVault},
		{http.MethodGet, "/allocations", vaultReadRoles, data.ReadVault},
		{http.MethodGet, "/allocations/GCNMZYNV3NVFLK25K3KLZ2LM6HAAD4BVYHULQX746XERD6HQRGGJ6SLD", vaultReadRoles, data.ReadVault},
		{http.MethodGet, "/events", vaultReadRoles, data.ReadVault},
		{http.MethodGet, "/indexer", vaultReadRoles, data.ReadVault},
		{http.MethodGet, "/status", vaultReadRoles, data.ReadVault},
		{http.MethodPost, "/transactions", []data.UserRole{data.OwnerUserRole, data.FinancialControllerUserRole}, data.WriteVault},
		{http.MethodPost, "/resync", []data.UserRole{data.OwnerUserRole}, data.WriteVault},
	}
)

// vaultRoutesHandler builds the real mux with the vault enabled. authManager
// may be nil to keep the default one.
func vaultRoutesHandler(t *testing.T, dbConnectionPool db.DBConnectionPool, enableVault bool, authManager auth.AuthManager) http.Handler {
	t.Helper()

	opts := getServeOptionsForTests(t, dbConnectionPool)
	opts.EnableVault = enableVault
	opts.VaultContractID = vaultRoutesTestContract
	opts.VaultIndexerIntervalSeconds = 10
	opts.VaultStatusLaggingLedgers = 60
	opts.VaultStatusStallSeconds = 300
	opts.VaultStatusRPCCacheSeconds = 5
	if authManager != nil {
		opts.authManager = authManager
	}

	tenantMock, ok := opts.tenantManager.(*tenant.TenantManagerMock)
	require.True(t, ok)
	tenantMock.On("GetTenantByID", mock.Anything, "tenant1").
		Return(&schema.Tenant{ID: "tenant1", Name: "aid-org"}, nil).Maybe()

	return handleHTTP(opts)
}

func vaultRouteRequest(t *testing.T, mux http.Handler, route vaultRoute, authorization string) *httptest.ResponseRecorder {
	t.Helper()

	var body *strings.Reader
	if route.method == http.MethodPost {
		body = strings.NewReader(`{}`)
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(route.method, "/vault/"+vaultRoutesTestContract+route.path, body)
	req.Header.Set(middleware.TenantHeaderKey, "aid-org")
	req.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	req.RemoteAddr = "192.168.1.0:4000"

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

// passedAuth is true when the request got past authentication and
// authorisation. The handler may still answer 400/404/500 — no vault is
// indexed and no transaction builder is configured here — which is fine:
// those are the handler's answers, and reaching the handler is the point.
func passedAuth(code int) bool {
	return code != http.StatusUnauthorized && code != http.StatusForbidden
}

func Test_handleHTTP_vaultRoutes_flagOffIs404(t *testing.T) {
	mux := vaultRoutesHandler(t, getConnectionPool(t), false, nil)
	for _, route := range vaultRoutes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			assert.Equal(t, http.StatusNotFound, vaultRouteRequest(t, mux, route, "").Code)
		})
	}
}

func Test_handleHTTP_vaultRoutes_requireAuthentication(t *testing.T) {
	mux := vaultRoutesHandler(t, getConnectionPool(t), true, nil)
	for _, route := range vaultRoutes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			assert.Equal(t, http.StatusUnauthorized, vaultRouteRequest(t, mux, route, "").Code)
		})
	}
}

func Test_handleHTTP_vaultRoutes_roleMatrix(t *testing.T) {
	dbConnectionPool := getConnectionPool(t)

	for _, role := range data.GetAllRoles() {
		token := "token-" + string(role)
		hasRole := func(required []string) bool { return slices.Contains(required, string(role)) }

		authMock := &auth.AuthManagerMock{}
		authMock.On("GetUserID", mock.Anything, token).Return("user-"+string(role), nil)
		authMock.On("GetTenantID", mock.Anything, token).Return("tenant1", nil)
		authMock.On("AnyRolesInTokenUser", mock.Anything, token, mock.MatchedBy(hasRole)).Return(true, nil)
		authMock.On("AnyRolesInTokenUser", mock.Anything, token,
			mock.MatchedBy(func(r []string) bool { return !hasRole(r) })).Return(false, nil)

		mux := vaultRoutesHandler(t, dbConnectionPool, true, authMock)

		for _, route := range vaultRoutes {
			t.Run(string(role)+" "+route.method+" "+route.path, func(t *testing.T) {
				code := vaultRouteRequest(t, mux, route, "Bearer "+token).Code
				if slices.Contains(route.roles, role) {
					assert.True(t, passedAuth(code), "role %s must reach the handler, got %d", role, code)
				} else {
					assert.Equal(t, http.StatusForbidden, code, "role %s must be refused", role)
				}
			})
		}
	}
}

func Test_handleHTTP_vaultRoutes_apiKeyPermissions(t *testing.T) {
	res := setupAPIKeyTestResources(t)
	mux := vaultRoutesHandler(t, res.DBPool, true, nil)

	readVaultKey := createTestAPIKey(t, res.DBPool, "vault reader", []data.APIKeyPermission{data.ReadVault}, nil, 30, res.TestUserID)
	writeVaultKey := createTestAPIKey(t, res.DBPool, "vault writer", []data.APIKeyPermission{data.WriteVault}, nil, 30, res.TestUserID)
	unrelatedKey := createTestAPIKey(t, res.DBPool, "disbursement reader", []data.APIKeyPermission{data.ReadDisbursements}, nil, 30, res.TestUserID)

	for _, route := range vaultRoutes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			readCode := vaultRouteRequest(t, mux, route, readVaultKey.Key).Code
			writeCode := vaultRouteRequest(t, mux, route, writeVaultKey.Key).Code
			unrelatedCode := vaultRouteRequest(t, mux, route, unrelatedKey.Key).Code

			assert.Equal(t, http.StatusForbidden, unrelatedCode, "a key without vault permissions is refused")
			if route.perm == data.ReadVault {
				assert.True(t, passedAuth(readCode), "read:vault reaches a read route, got %d", readCode)
			} else {
				assert.Equal(t, http.StatusForbidden, readCode, "read:vault must not reach a write route")
				assert.True(t, passedAuth(writeCode), "write:vault reaches a write route, got %d", writeCode)
			}
		})
	}
}
