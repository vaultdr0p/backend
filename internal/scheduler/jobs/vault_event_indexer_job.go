package jobs

import (
	"context"
	"fmt"
	"time"

	"github.com/stellar/go-stellar-sdk/support/log"

	"github.com/stellar/stellar-disbursement-platform-backend/internal/data"
	"github.com/stellar/stellar-disbursement-platform-backend/internal/monitor"
	"github.com/stellar/stellar-disbursement-platform-backend/internal/sdpcontext"
	"github.com/stellar/stellar-disbursement-platform-backend/internal/vault"
)

const vaultEventIndexerJobName = "vault_event_indexer_job"

// VaultEventIndexerJobOptions configures the job.
type VaultEventIndexerJobOptions struct {
	JobIntervalSeconds int
	Models             *data.Models
	MonitorService     monitor.MonitorServiceInterface
	RPCClient          vault.RPCClient
	ContractID         string
	IndexFromLedger    uint32
}

// vaultEventIndexerJob polls Stellar RPC for the vault contract's events and
// projects them into the tenant's schema.
type vaultEventIndexerJob struct {
	jobIntervalSeconds int
	models             *data.Models
	rpcClient          vault.RPCClient
	contractID         string
	indexFromLedger    uint32
	lagMonitor         *vault.LagMonitor
	monitorService     monitor.MonitorServiceInterface
}

// NewVaultEventIndexerJob builds the job. It is registered only when
// ENABLE_VAULT is true, so nothing here runs in a default deployment.
func NewVaultEventIndexerJob(opts VaultEventIndexerJobOptions) Job {
	if opts.JobIntervalSeconds < DefaultMinimumJobIntervalSeconds {
		log.Fatalf("job interval for %s is set below the minimum %d. Instantiation failed",
			vaultEventIndexerJobName, DefaultMinimumJobIntervalSeconds)
	}
	if opts.ContractID == "" {
		log.Fatalf("contract ID is required for %s. Instantiation failed", vaultEventIndexerJobName)
	}

	return &vaultEventIndexerJob{
		jobIntervalSeconds: opts.JobIntervalSeconds,
		models:             opts.Models,
		rpcClient:          opts.RPCClient,
		contractID:         opts.ContractID,
		indexFromLedger:    opts.IndexFromLedger,
		lagMonitor:         vault.NewLagMonitor(opts.MonitorService),
		monitorService:     opts.MonitorService,
	}
}

func (j vaultEventIndexerJob) GetInterval() time.Duration {
	return time.Duration(j.jobIntervalSeconds) * time.Second
}

func (j vaultEventIndexerJob) GetName() string {
	return vaultEventIndexerJobName
}

// IsJobMultiTenant is true so the scheduler runs this once per tenant with the
// tenant in context. The models' connection pool is then routed to that
// tenant's schema, which is what makes the cursor and every projection
// per-tenant without the indexer having to filter by tenant itself.
func (j vaultEventIndexerJob) IsJobMultiTenant() bool {
	return true
}

func (j vaultEventIndexerJob) Execute(ctx context.Context) error {
	t, err := sdpcontext.GetTenantFromContext(ctx)
	if err != nil {
		return fmt.Errorf("getting tenant from context for %s: %w", vaultEventIndexerJobName, err)
	}

	indexer, err := vault.NewIndexer(j.rpcClient, j.models, j.contractID, j.indexFromLedger)
	if err != nil {
		return fmt.Errorf("creating the vault indexer for tenant %s: %w", t.Name, err)
	}

	result, err := indexer.Run(ctx)
	if err != nil {
		return fmt.Errorf("indexing vault %s for tenant %s: %w", j.contractID, t.Name, err)
	}

	// Reported after every run, not only when something was read: lag is a
	// gauge, and a gauge that stops being published looks the same as a
	// healthy one that is simply not changing.
	j.lagMonitor.Report(ctx, t.Name, j.contractID, result)
	j.reportMissedSummaryUpdates(ctx, t.Name, result)

	if result.EventsApplied > 0 || result.EventsSkipped > 0 || result.Anomalies > 0 {
		log.Ctx(ctx).Infof(
			"vault %s: read %d events, applied %d, skipped %d, anomalies %d, now at ledger %d",
			j.contractID, result.EventsRead, result.EventsApplied,
			result.EventsSkipped, result.Anomalies, result.LastLedger)
	}

	return nil
}

// reportMissedSummaryUpdates increments the missed-summary-update counter once
// per event. The warning for each event is logged by the indexer itself.
func (j vaultEventIndexerJob) reportMissedSummaryUpdates(ctx context.Context, tenantName string, result *vault.Result) {
	if j.monitorService == nil {
		return
	}
	for eventType, count := range result.SummaryUpdatesMissed {
		labels := map[string]string{
			"tenant_name": tenantName,
			"contract_id": j.contractID,
			"event_type":  string(eventType),
		}
		for i := 0; i < count; i++ {
			if err := j.monitorService.MonitorCounters(monitor.VaultIndexerSummaryUpdateMissedTag, labels); err != nil {
				log.Ctx(ctx).Errorf("vault %s: recording a missed summary update: %v", j.contractID, err)
				return
			}
		}
	}
}

var _ Job = (*vaultEventIndexerJob)(nil)
