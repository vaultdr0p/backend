package jobs

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/stellar/stellar-disbursement-platform-backend/internal/monitor"
	monitorMocks "github.com/stellar/stellar-disbursement-platform-backend/internal/monitor/mocks"
	"github.com/stellar/stellar-disbursement-platform-backend/internal/vault"
)

const testVaultJobContract = "CB5HTYC4GZW67ET23SACDCWCCDQ3AKU4TYX7SAUQJSKS6LLQLYA4HG53"

func missedLabels(eventType string) map[string]string {
	return map[string]string{"tenant_name": "tenant", "contract_id": testVaultJobContract, "event_type": eventType}
}

func Test_vaultEventIndexerJob_reportMissedSummaryUpdates(t *testing.T) {
	t.Run("one increment per missed update, labelled by event type", func(t *testing.T) {
		monitorService := monitorMocks.NewMockMonitorService(t)
		monitorService.On("MonitorCounters", monitor.VaultIndexerSummaryUpdateMissedTag, missedLabels("funded")).
			Return(nil).Once()
		monitorService.On("MonitorCounters", monitor.VaultIndexerSummaryUpdateMissedTag, missedLabels("withdrawn")).
			Return(nil).Twice()

		job := vaultEventIndexerJob{contractID: testVaultJobContract, monitorService: monitorService}
		job.reportMissedSummaryUpdates(context.Background(), "tenant", &vault.Result{
			SummaryUpdatesMissed: map[vault.EventType]int{vault.EventTypeFunded: 1, vault.EventTypeWithdrawn: 2},
		})
	})

	t.Run("nothing missed, nothing emitted", func(t *testing.T) {
		monitorService := monitorMocks.NewMockMonitorService(t)
		job := vaultEventIndexerJob{contractID: testVaultJobContract, monitorService: monitorService}
		job.reportMissedSummaryUpdates(context.Background(), "tenant", &vault.Result{})
		monitorService.AssertNotCalled(t, "MonitorCounters")
	})

	t.Run("a metrics failure does not panic or fail the job", func(t *testing.T) {
		monitorService := monitorMocks.NewMockMonitorService(t)
		monitorService.On("MonitorCounters", monitor.VaultIndexerSummaryUpdateMissedTag, missedLabels("funded")).
			Return(errors.New("registry down")).Once()
		job := vaultEventIndexerJob{contractID: testVaultJobContract, monitorService: monitorService}
		assert.NotPanics(t, func() {
			job.reportMissedSummaryUpdates(context.Background(), "tenant", &vault.Result{
				SummaryUpdatesMissed: map[vault.EventType]int{vault.EventTypeFunded: 3},
			})
		})
	})

	t.Run("no monitor service configured", func(t *testing.T) {
		job := vaultEventIndexerJob{contractID: testVaultJobContract}
		assert.NotPanics(t, func() {
			job.reportMissedSummaryUpdates(context.Background(), "tenant", &vault.Result{
				SummaryUpdatesMissed: map[vault.EventType]int{vault.EventTypeFunded: 1},
			})
		})
	})
}

func Test_VaultIndexerSummaryUpdateMissedTag_isRegistered(t *testing.T) {
	counter, ok := monitor.CounterVecMetrics[monitor.VaultIndexerSummaryUpdateMissedTag]
	assert.True(t, ok, "the counter must be registered, or MonitorCounters only logs an error")
	assert.NotNil(t, counter.With(missedLabels("funded")), "labels must match the registered label names")
}
