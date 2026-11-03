package vault

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/stellar/stellar-disbursement-platform-backend/internal/monitor"
	monitorMocks "github.com/stellar/stellar-disbursement-platform-backend/internal/monitor/mocks"
	"github.com/stellar/stellar-disbursement-platform-backend/internal/stellar"
)

func Test_AssessLag_thresholds(t *testing.T) {
	window := stellar.EventRetentionWindowLedgers
	warn := stellar.EventRetentionLagWarnLedgers

	testCases := []struct {
		name   string
		result Result
		want   LagSeverity
	}{
		{name: "caught up", result: Result{LastLedger: 1000, LatestLedger: 1000}, want: LagSeverityOK},
		{name: "a little behind", result: Result{LastLedger: 1000, LatestLedger: 1100}, want: LagSeverityOK},
		// The boundary is the whole point of the threshold, so it is pinned
		// exactly rather than approximately.
		{
			name:   "one ledger below the warning threshold",
			result: Result{LastLedger: 1, LatestLedger: warn},
			want:   LagSeverityOK,
		},
		{
			name:   "exactly at the warning threshold",
			result: Result{LastLedger: 1, LatestLedger: warn + 1},
			want:   LagSeverityWarning,
		},
		{
			name:   "one ledger below the window",
			result: Result{LastLedger: 1, LatestLedger: window},
			want:   LagSeverityWarning,
		},
		{
			name:   "exactly at the window, where events start being lost",
			result: Result{LastLedger: 1, LatestLedger: window + 1},
			want:   LagSeverityCritical,
		},
		{
			name:   "far past the window",
			result: Result{LastLedger: 1, LatestLedger: window * 3},
			want:   LagSeverityCritical,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			alert := AssessLag(&tc.result)

			assert.Equal(t, tc.want, alert.Severity)
			assert.Equal(t, tc.result.LagLedgers(), alert.LagLedgers)
			// The alert carries what it was measured against, so a log line
			// never leaves a reader guessing which window applies.
			assert.Equal(t, warn, alert.WarnThreshold)
			assert.Equal(t, window, alert.Window)
		})
	}
}

func Test_AssessLag_warnsAtHalfTheRetentionWindow(t *testing.T) {
	// The requirement is specifically half the window, so assert the
	// relationship rather than the literal — if the measured window changes,
	// this must still hold.
	assert.Equal(t, stellar.EventRetentionWindowLedgers/2, stellar.EventRetentionLagWarnLedgers)

	halfway := Result{LastLedger: 1, LatestLedger: stellar.EventRetentionWindowLedgers/2 + 1}
	assert.Equal(t, LagSeverityWarning, AssessLag(&halfway).Severity)
}

func Test_AssessLag_degradedIsIndependentOfLag(t *testing.T) {
	// A vault can be fully caught up and still permanently degraded from an
	// earlier outage. Reporting it as "ok" because lag is zero would hide a
	// known, unrecoverable gap.
	alert := AssessLag(&Result{LastLedger: 1000, LatestLedger: 1000, Degraded: true})

	assert.Equal(t, LagSeverityOK, alert.Severity)
	assert.True(t, alert.Degraded)
}

func Test_LagAlert_Message(t *testing.T) {
	testCases := []struct {
		name     string
		alert    LagAlert
		contains []string
	}{
		{
			name:     "ok says the number plainly",
			alert:    LagAlert{ContractID: vaultContractID, LagLedgers: 3, Severity: LagSeverityOK},
			contains: []string{vaultContractID, "3 ledgers"},
		},
		{
			// An operator reading "lag 61000" cannot tell whether that is
			// fine, so the message has to explain what it was measured
			// against and what happens next.
			name: "warning explains the headroom",
			alert: LagAlert{
				ContractID: vaultContractID, LagLedgers: 60_481, Severity: LagSeverityWarning,
				WarnThreshold: 60_480, Window: 120_960,
			},
			contains: []string{"half", "120960", "headroom"},
		},
		{
			name: "critical says data is being lost now",
			alert: LagAlert{
				ContractID: vaultContractID, LagLedgers: 130_000, Severity: LagSeverityCritical,
				WarnThreshold: 60_480, Window: 120_960,
			},
			contains: []string{"aging out", "lost permanently"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			message := tc.alert.Message()

			for _, fragment := range tc.contains {
				assert.Contains(t, message, fragment)
			}
		})
	}
}

func Test_LagMonitor_Report_emitsBothGauges(t *testing.T) {
	monitorService := monitorMocks.NewMockMonitorService(t)
	expectedLabels := map[string]string{"tenant_name": "bluecorp", "contract_id": vaultContractID}

	monitorService.On("MonitorGauge", float64(100), monitor.VaultIndexerLagLedgersTag, expectedLabels).
		Return(nil).Once()
	monitorService.On("MonitorGauge", float64(0), monitor.VaultIndexerDegradedTag, expectedLabels).
		Return(nil).Once()

	lagMonitor := NewLagMonitor(monitorService)
	alert := lagMonitor.Report(context.Background(), "bluecorp", vaultContractID,
		&Result{LastLedger: 1000, LatestLedger: 1100})

	assert.Equal(t, uint32(100), alert.LagLedgers)
	assert.Equal(t, LagSeverityOK, alert.Severity)
	assert.Equal(t, vaultContractID, alert.ContractID)
}

func Test_LagMonitor_Report_degradedGaugeIsOne(t *testing.T) {
	monitorService := monitorMocks.NewMockMonitorService(t)
	expectedLabels := map[string]string{"tenant_name": "redcorp", "contract_id": vaultContractID}

	monitorService.On("MonitorGauge", mock.Anything, monitor.VaultIndexerLagLedgersTag, expectedLabels).
		Return(nil).Once()
	monitorService.On("MonitorGauge", float64(1), monitor.VaultIndexerDegradedTag, expectedLabels).
		Return(nil).Once()

	lagMonitor := NewLagMonitor(monitorService)
	alert := lagMonitor.Report(context.Background(), "redcorp", vaultContractID,
		&Result{LastLedger: 1000, LatestLedger: 1000, Degraded: true})

	assert.True(t, alert.Degraded)
}

func Test_LagMonitor_Report_survivesAMetricsFailure(t *testing.T) {
	// Indexing must not fail because a gauge could not be set. The indexer's
	// job is to keep the event log complete; dropping a batch over
	// observability would trade real data for a number.
	monitorService := monitorMocks.NewMockMonitorService(t)
	monitorService.On("MonitorGauge", mock.Anything, mock.Anything, mock.Anything).
		Return(errors.New("metrics backend unreachable")).Twice()

	lagMonitor := NewLagMonitor(monitorService)
	alert := lagMonitor.Report(context.Background(), "bluecorp", vaultContractID,
		&Result{LastLedger: 1000, LatestLedger: 1100})

	assert.Equal(t, uint32(100), alert.LagLedgers)
}

func Test_LagMonitor_Report_worksWithoutAMonitor(t *testing.T) {
	// Metrics are optional in SDP, so a nil monitor must still produce the
	// log-based alert rather than panicking. A deployment with no metrics
	// backend still has to learn its vault data is about to become
	// unrecoverable.
	lagMonitor := NewLagMonitor(nil)

	require.NotPanics(t, func() {
		alert := lagMonitor.Report(context.Background(), "bluecorp", vaultContractID,
			&Result{LastLedger: 1, LatestLedger: stellar.EventRetentionWindowLedgers + 1})

		assert.Equal(t, LagSeverityCritical, alert.Severity)
	})
}

func Test_VaultMetricTags_areRegistered(t *testing.T) {
	// A metric that is defined but not registered is exported as nothing at
	// all, so the alert would never fire. Assert the wiring, not just the
	// constant.
	registered := monitor.PrometheusMetrics()

	for _, tag := range []monitor.MetricTag{
		monitor.VaultIndexerLagLedgersTag,
		monitor.VaultIndexerDegradedTag,
	} {
		t.Run(string(tag), func(t *testing.T) {
			assert.Contains(t, registered, tag)
			assert.Contains(t, monitor.MetricTag("").ListAll(), tag)
		})
	}
}
