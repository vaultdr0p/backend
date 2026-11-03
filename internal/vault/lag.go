package vault

import (
	"context"
	"fmt"

	"github.com/stellar/go-stellar-sdk/support/log"

	"github.com/stellar/stellar-disbursement-platform-backend/internal/monitor"
	"github.com/stellar/stellar-disbursement-platform-backend/internal/stellar"
)

// Indexer lag is the one number that predicts permanent data loss.
//
// RPC keeps contract events for a bounded window — measured against the
// configured provider as 120,960 ledgers, 7 days at a 5s close time (see
// stellar.EventRetentionWindowLedgers). Beyond it, events cannot be fetched
// from RPC at all. And because the contract exposes `get_allocation(address)`
// but no way to enumerate recipients, the event log is the *only* source of
// the recipient set: if the indexer falls behind the window, the identity of
// anyone registered during the gap is unrecoverable. No retry, resync or
// backfill can reconstruct it, because the data no longer exists anywhere
// except the ledger's own archives.
//
// So the alert fires at **half** the window — stellar.EventRetentionLagWarnLedgers,
// 60,480 ledgers — which leaves roughly 3.5 days to intervene. That is the
// point of alerting on a fraction rather than on the window itself: an alert
// that fires when events have already aged out is a post-mortem, not an alert.

// LagSeverity is how bad the indexer's lag is.
type LagSeverity string

const (
	// LagSeverityOK means the indexer is comfortably inside the window.
	LagSeverityOK LagSeverity = "ok"
	// LagSeverityWarning means lag has passed half the retention window.
	// Events have not been lost yet, but the remaining headroom is finite and
	// shrinking.
	LagSeverityWarning LagSeverity = "warning"
	// LagSeverityCritical means lag has passed the retention window itself:
	// events are aging out unread and data is being lost right now.
	LagSeverityCritical LagSeverity = "critical"
)

// LagAlert is the assessment of one vault's indexing lag.
type LagAlert struct {
	ContractID string
	LagLedgers uint32
	Severity   LagSeverity
	// WarnThreshold and Window are carried on the alert so a log line or a
	// response says what the lag was measured against, rather than leaving a
	// reader to guess which window applies to their RPC provider.
	WarnThreshold uint32
	Window        uint32
	// Degraded is true when a gap has already been recorded. This is
	// independent of lag: a vault can be caught up (zero lag) and still be
	// permanently degraded from an earlier outage.
	Degraded bool
}

// AssessLag classifies an indexing result against the retention window.
func AssessLag(result *Result) LagAlert {
	lag := result.LagLedgers()

	alert := LagAlert{
		ContractID:    "",
		LagLedgers:    lag,
		Severity:      LagSeverityOK,
		WarnThreshold: stellar.EventRetentionLagWarnLedgers,
		Window:        stellar.EventRetentionWindowLedgers,
		Degraded:      result.Degraded,
	}

	switch {
	case lag >= stellar.EventRetentionWindowLedgers:
		alert.Severity = LagSeverityCritical
	case lag >= stellar.EventRetentionLagWarnLedgers:
		alert.Severity = LagSeverityWarning
	}

	return alert
}

// Message renders the alert for a log line, saying what the number means
// rather than only what it is. An operator reading "lag 61000" has no way to
// know whether that is fine.
func (a LagAlert) Message() string {
	switch a.Severity {
	case LagSeverityCritical:
		return fmt.Sprintf(
			"vault %s indexer lag is %d ledgers, at or beyond the RPC event retention window of %d: "+
				"events are aging out before they are read and recipient registrations are being lost permanently",
			a.ContractID, a.LagLedgers, a.Window)
	case LagSeverityWarning:
		return fmt.Sprintf(
			"vault %s indexer lag is %d ledgers, past %d (half the RPC event retention window of %d): "+
				"roughly half the window of headroom remains before events start aging out unread",
			a.ContractID, a.LagLedgers, a.WarnThreshold, a.Window)
	default:
		return fmt.Sprintf("vault %s indexer lag is %d ledgers", a.ContractID, a.LagLedgers)
	}
}

// LagMonitor reports indexer lag as metrics and logs.
//
// It takes the narrow interface rather than *monitor.MonitorService so the job
// can be tested without a metrics backend, and so a nil monitor is a
// supported configuration: metrics are optional in SDP, and the vault indexer
// must run with or without them.
type LagMonitor struct {
	monitorService monitor.MonitorServiceInterface
}

// NewLagMonitor builds a lag monitor. A nil monitorService is allowed and
// turns metric emission into a no-op, leaving the log-based alert in place.
func NewLagMonitor(monitorService monitor.MonitorServiceInterface) *LagMonitor {
	return &LagMonitor{monitorService: monitorService}
}

// Report emits the lag gauge, the degraded gauge, and a log line at the
// severity the lag warrants.
//
// The log line is deliberately not left to Prometheus alone: a deployment
// without a metrics backend still has to learn that its vault data is about to
// become unrecoverable, and "the alert existed but nothing was scraping it" is
// not a defence.
func (lm *LagMonitor) Report(ctx context.Context, tenantName, contractID string, result *Result) LagAlert {
	alert := AssessLag(result)
	alert.ContractID = contractID

	switch alert.Severity {
	case LagSeverityCritical:
		log.Ctx(ctx).Error(alert.Message())
	case LagSeverityWarning:
		log.Ctx(ctx).Warn(alert.Message())
	default:
		log.Ctx(ctx).Debug(alert.Message())
	}

	if alert.Degraded {
		log.Ctx(ctx).Warnf(
			"vault %s has a recorded gap in its indexed history; the recipient list may be incomplete. "+
				"On-chain funds are unaffected and any known recipient can still claim.",
			contractID)
	}

	lm.emit(ctx, tenantName, contractID, alert)

	return alert
}

func (lm *LagMonitor) emit(ctx context.Context, tenantName, contractID string, alert LagAlert) {
	if lm.monitorService == nil {
		return
	}

	labels := map[string]string{
		"tenant_name": tenantName,
		"contract_id": contractID,
	}

	if err := lm.monitorService.MonitorGauge(float64(alert.LagLedgers), monitor.VaultIndexerLagLedgersTag, labels); err != nil {
		// A metrics failure must never fail indexing: the indexer's job is to
		// keep the event log complete, and dropping a batch because a gauge
		// could not be set would trade real data for observability.
		log.Ctx(ctx).Warnf("recording vault indexer lag metric: %v", err)
	}

	degraded := float64(0)
	if alert.Degraded {
		degraded = 1
	}
	if err := lm.monitorService.MonitorGauge(degraded, monitor.VaultIndexerDegradedTag, labels); err != nil {
		log.Ctx(ctx).Warnf("recording vault indexer degraded metric: %v", err)
	}
}
