package vault

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stellar/stellar-disbursement-platform-backend/internal/data"
	"github.com/stellar/stellar-disbursement-platform-backend/internal/stellar"
)

var (
	healthNow        = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	healthThresholds = HealthThresholds{LaggingLedgers: 60, StallAfter: 300 * time.Second}
	errRPCDown       = errors.New("dial tcp: connection refused")
)

// stateAt builds an indexer row at `cursor`, last committed `age` before
// healthNow.
func stateAt(cursor int32, age time.Duration) *data.VaultIndexerState {
	committed := healthNow.Add(-age)
	return &data.VaultIndexerState{
		ContractID:    vaultContractID,
		LastLedger:    cursor,
		Status:        data.VaultIndexerStatusHealthy,
		LastIndexedAt: &committed,
	}
}

func degraded(s *data.VaultIndexerState) *data.VaultIndexerState {
	s.Status = data.VaultIndexerStatusDegraded
	return s
}

func neverCommitted(cursor int32) *data.VaultIndexerState {
	return &data.VaultIndexerState{ContractID: vaultContractID, LastLedger: cursor, Status: data.VaultIndexerStatusHealthy}
}

func Test_AssessHealth_statusTransitionsAndBoundaries(t *testing.T) {
	const tip = uint32(10_000)
	fresh := 10 * time.Second

	testCases := []struct {
		name       string
		state      *data.VaultIndexerState
		latest     uint32
		latestErr  error
		wantStatus HealthStatus
		wantReason HealthReason
	}{
		// Rule 5: ok.
		{"caught up", stateAt(int32(tip), fresh), tip, nil, HealthStatusOK, HealthReasonNone},
		{"small normal lag", stateAt(int32(tip-2), fresh), tip, nil, HealthStatusOK, HealthReasonNone},

		// Rule 4: lag boundary at LaggingLedgers = 60.
		{"lag threshold-1 is ok", stateAt(int32(tip-59), fresh), tip, nil, HealthStatusOK, HealthReasonNone},
		{"lag at threshold is lagging", stateAt(int32(tip-60), fresh), tip, nil, HealthStatusLagging, HealthReasonLagOverThreshold},
		{"lag threshold+1 is lagging", stateAt(int32(tip-61), fresh), tip, nil, HealthStatusLagging, HealthReasonLagOverThreshold},
		{"lag past the retention window is lagging", stateAt(1, fresh), 200_000, nil, HealthStatusLagging, HealthReasonLagOverThreshold},

		// Rule 3: stall boundary at StallAfter = 300s.
		{"commit age threshold-1s is ok", stateAt(int32(tip), 299*time.Second), tip, nil, HealthStatusOK, HealthReasonNone},
		{"commit age at threshold is stalled", stateAt(int32(tip), 300*time.Second), tip, nil, HealthStatusStalled, HealthReasonNoRecentCommit},
		{"commit age threshold+1s is stalled", stateAt(int32(tip), 301*time.Second), tip, nil, HealthStatusStalled, HealthReasonNoRecentCommit},
		{"commit in the future (clock skew) is not stalled", stateAt(int32(tip), -30*time.Second), tip, nil, HealthStatusOK, HealthReasonNone},

		// Rule 2: never indexed.
		{"no state row", nil, tip, nil, HealthStatusStalled, HealthReasonNeverIndexed},
		{"row bootstrapped but never committed", neverCommitted(int32(tip - 5)), tip, nil, HealthStatusStalled, HealthReasonNeverIndexed},

		// Rule 1: RPC unreachable.
		{"rpc down, otherwise healthy", stateAt(int32(tip), fresh), 0, errRPCDown, HealthStatusStalled, HealthReasonRPCUnreachable},

		// Precedence: the first matching rule wins.
		{"rpc down beats never indexed", nil, 0, errRPCDown, HealthStatusStalled, HealthReasonRPCUnreachable},
		{"rpc down beats no recent commit", stateAt(int32(tip), time.Hour), 0, errRPCDown, HealthStatusStalled, HealthReasonRPCUnreachable},
		{"never indexed beats lag", neverCommitted(1), tip, nil, HealthStatusStalled, HealthReasonNeverIndexed},
		{"no recent commit beats lag", stateAt(int32(tip-500), time.Hour), tip, nil, HealthStatusStalled, HealthReasonNoRecentCommit},

		// Degraded is orthogonal to status.
		{"degraded but caught up stays ok", degraded(stateAt(int32(tip), fresh)), tip, nil, HealthStatusOK, HealthReasonNone},
		{"degraded and lagging is lagging", degraded(stateAt(int32(tip-100), fresh)), tip, nil, HealthStatusLagging, HealthReasonLagOverThreshold},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			h := AssessHealth(HealthInput{
				ContractID:      vaultContractID,
				State:           tc.state,
				LatestLedger:    tc.latest,
				LatestLedgerErr: tc.latestErr,
			}, healthThresholds, healthNow)

			assert.Equal(t, tc.wantStatus, h.Status)
			assert.Equal(t, tc.wantReason, h.Reason)
			assert.NotEmpty(t, h.Detail, "every status carries a human-readable detail")
			assert.Equal(t, healthNow, h.CheckedAt)
			assert.Equal(t, vaultContractID, h.ContractID)
			if tc.state != nil {
				assert.Equal(t, tc.state.Status == data.VaultIndexerStatusDegraded, h.Degraded)
			}
		})
	}
}

func Test_AssessHealth_derivedFields(t *testing.T) {
	window := stellar.EventRetentionWindowLedgers
	fresh := 10 * time.Second

	testCases := []struct {
		name         string
		cursor       int32
		latest       uint32
		wantLag      uint32
		wantSeconds  uint64
		wantHeadroom uint32
	}{
		{"caught up", 5_000, 5_000, 0, 0, window},
		{"cursor ahead of a cached tip floors lag at zero", 5_010, 5_000, 0, 0, window},
		{"ordinary lag", 4_880, 5_000, 120, 600, window - 120},
		{"negative cursor counts from zero", -1, 5_000, 5_000, 25_000, window - 5_000},
		{"headroom one ledger before the window", 1_000, 1_000 + window - 1, window - 1, uint64(window-1) * 5, 1},
		{"headroom at the window is zero", 1_000, 1_000 + window, window, uint64(window) * 5, 0},
		{"headroom past the window floors at zero", 1_000, 1_000 + window + 1, window + 1, uint64(window+1) * 5, 0},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			h := AssessHealth(HealthInput{
				ContractID:   vaultContractID,
				State:        stateAt(tc.cursor, fresh),
				LatestLedger: tc.latest,
			}, healthThresholds, healthNow)

			require.NotNil(t, h.CursorLedger)
			require.NotNil(t, h.RPCLatestLedger)
			require.NotNil(t, h.LagLedgers)
			require.NotNil(t, h.LagSecondsEstimate)
			require.NotNil(t, h.RetentionHeadroom)
			assert.Equal(t, tc.cursor, *h.CursorLedger)
			assert.Equal(t, tc.latest, *h.RPCLatestLedger)
			assert.Equal(t, tc.wantLag, *h.LagLedgers)
			assert.Equal(t, tc.wantSeconds, *h.LagSecondsEstimate)
			assert.Equal(t, tc.wantHeadroom, *h.RetentionHeadroom)
		})
	}
}

func Test_AssessHealth_rpcDownNullsOnlyTheRPCFields(t *testing.T) {
	lastEvent := healthNow.Add(-time.Minute)
	h := AssessHealth(HealthInput{
		ContractID:      vaultContractID,
		State:           degraded(stateAt(777, 10*time.Second)),
		LatestLedgerErr: errRPCDown,
		LastEventAt:     &lastEvent,
	}, healthThresholds, healthNow)

	assert.Nil(t, h.RPCLatestLedger)
	assert.Nil(t, h.LagLedgers)
	assert.Nil(t, h.LagSecondsEstimate)
	assert.Nil(t, h.RetentionHeadroom)

	// Database-backed fields are still served.
	require.NotNil(t, h.CursorLedger)
	assert.Equal(t, int32(777), *h.CursorLedger)
	assert.Equal(t, &lastEvent, h.LastEventAt)
	assert.True(t, h.Degraded)

	// The raw error can carry the RPC URL (and an API key in it); never echo it.
	assert.NotContains(t, h.Detail, errRPCDown.Error())
}

func Test_AssessHealth_noStateRowLeavesCursorFieldsNull(t *testing.T) {
	h := AssessHealth(HealthInput{ContractID: vaultContractID, LatestLedger: 5_000}, healthThresholds, healthNow)

	assert.Nil(t, h.CursorLedger)
	assert.Nil(t, h.LagLedgers)
	assert.Nil(t, h.RetentionHeadroom)
	require.NotNil(t, h.RPCLatestLedger)
	assert.Equal(t, uint32(5_000), *h.RPCLatestLedger)
	assert.False(t, h.Degraded)
}

func Test_AssessHealth_jsonShape(t *testing.T) {
	// The wire shape is the contract with monitors: every documented key is
	// always present, null rather than omitted, and checked_at is RFC 3339.
	local := time.FixedZone("WAT", 3600)
	h := AssessHealth(HealthInput{ContractID: vaultContractID, LatestLedgerErr: errRPCDown},
		healthThresholds, healthNow.In(local))

	raw, err := json.Marshal(h)
	require.NoError(t, err)

	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body))

	for _, key := range []string{
		"contract_id", "status", "reason", "detail", "degraded", "cursor_ledger",
		"rpc_latest_ledger", "lag_ledgers", "lag_seconds_estimate", "last_event_at",
		"retention_headroom", "checked_at",
	} {
		assert.Contains(t, body, key)
	}
	assert.Len(t, body, 12, "no undocumented keys")
	assert.Equal(t, "stalled", body["status"])
	assert.Equal(t, "rpc_unreachable", body["reason"])
	assert.Nil(t, body["lag_ledgers"])
	assert.Equal(t, "2026-11-11T12:00:00Z", body["checked_at"], "checked_at is normalised to UTC RFC 3339")
}
