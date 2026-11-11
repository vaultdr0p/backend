package vault

import (
	"fmt"
	"time"

	"github.com/stellar/stellar-disbursement-platform-backend/internal/data"
	"github.com/stellar/stellar-disbursement-platform-backend/internal/stellar"
)

// Indexer health, as served by GET /vault/{contract_id}/status.
//
// Every other indexer signal — the lag gauges, the LagMonitor log line — is
// produced *by* the indexer job, after a run. If the job stops running, they
// stop being produced, and a dead indexer looks the same as a quiet healthy
// one. AssessHealth measures from outside the job: the stored cursor and
// commit time against the live chain tip. See docs/VAULT_DESIGN.md §9.
//
// It is a pure function — no I/O, clock passed in — so every status
// transition and threshold boundary can be pinned by a table test.

// HealthStatus is the headline answer.
type HealthStatus string

const (
	HealthStatusOK      HealthStatus = "ok"
	HealthStatusLagging HealthStatus = "lagging"
	HealthStatusStalled HealthStatus = "stalled"
)

// HealthReason says why the status is what it is. It is a closed enum so a
// monitor can switch on it; Detail carries the human-readable explanation.
type HealthReason string

const (
	HealthReasonRPCUnreachable   HealthReason = "rpc_unreachable"
	HealthReasonNeverIndexed     HealthReason = "never_indexed"
	HealthReasonNoRecentCommit   HealthReason = "no_recent_commit"
	HealthReasonLagOverThreshold HealthReason = "lag_over_threshold"
	HealthReasonNone             HealthReason = "none"
)

// HealthThresholds are the operator-tunable limits. Both are inclusive: a
// value *at* the threshold trips it, matching AssessLag's convention.
type HealthThresholds struct {
	// LaggingLedgers is the lag at or above which the indexer is "lagging".
	LaggingLedgers uint32
	// StallAfter is how long since the last commit before the indexer is
	// "stalled".
	StallAfter time.Duration
}

// HealthInput is everything AssessHealth looks at.
type HealthInput struct {
	ContractID string
	// State is nil when the vault has no indexer row yet.
	State *data.VaultIndexerState
	// LatestLedger is the chain tip, valid only when LatestLedgerErr is nil.
	LatestLedger    uint32
	LatestLedgerErr error
	// LastEventAt is the close time of the newest indexed event, nil if none.
	LastEventAt *time.Time
}

// Health is the response body of GET /vault/{contract_id}/status.
//
// Fields that depend on RPC are pointers so an unreachable RPC renders them as
// null. A zero would read as "caught up", which is the opposite of the truth.
type Health struct {
	ContractID         string       `json:"contract_id"`
	Status             HealthStatus `json:"status"`
	Reason             HealthReason `json:"reason"`
	Detail             string       `json:"detail"`
	Degraded           bool         `json:"degraded"`
	CursorLedger       *int32       `json:"cursor_ledger"`
	RPCLatestLedger    *uint32      `json:"rpc_latest_ledger"`
	LagLedgers         *uint32      `json:"lag_ledgers"`
	LagSecondsEstimate *uint64      `json:"lag_seconds_estimate"`
	LastEventAt        *time.Time   `json:"last_event_at"`
	RetentionHeadroom  *uint32      `json:"retention_headroom"`
	CheckedAt          time.Time    `json:"checked_at"`
}

// AssessHealth classifies indexer health. Rules are evaluated in order and
// the first match wins:
//
//  1. chain-tip lookup failed           → stalled / rpc_unreachable
//  2. no state row, or never committed  → stalled / never_indexed
//  3. now - last_indexed_at ≥ StallAfter → stalled / no_recent_commit
//  4. lag ≥ LaggingLedgers              → lagging / lag_over_threshold
//  5. otherwise                         → ok / none
//
// A recorded retention gap sets Degraded but never changes Status: a gap is
// permanent history, not current progress, and folding it in would leave a
// fully recovered indexer red forever.
func AssessHealth(in HealthInput, th HealthThresholds, now time.Time) Health {
	h := Health{
		ContractID:  in.ContractID,
		LastEventAt: in.LastEventAt,
		CheckedAt:   now.UTC(),
	}

	if in.State != nil {
		cursor := in.State.LastLedger
		h.CursorLedger = &cursor
		h.Degraded = in.State.Status == data.VaultIndexerStatusDegraded
	}

	rpcOK := in.LatestLedgerErr == nil
	if rpcOK {
		latest := in.LatestLedger
		h.RPCLatestLedger = &latest

		if in.State != nil {
			lag := lagBetween(in.State.LastLedger, latest)
			lagSeconds := uint64(lag) * uint64(stellar.AverageLedgerCloseSeconds)
			headroom := uint32(0)
			if lag < stellar.EventRetentionWindowLedgers {
				headroom = stellar.EventRetentionWindowLedgers - lag
			}
			h.LagLedgers = &lag
			h.LagSecondsEstimate = &lagSeconds
			h.RetentionHeadroom = &headroom
		}
	}

	switch {
	case !rpcOK:
		h.Status, h.Reason = HealthStatusStalled, HealthReasonRPCUnreachable
		// The underlying error is deliberately not echoed: it can carry the
		// RPC URL, which may embed a provider API key. It is logged instead.
		h.Detail = "the RPC latest-ledger lookup failed, so lag cannot be measured; see server logs"

	case in.State == nil || in.State.LastIndexedAt == nil:
		h.Status, h.Reason = HealthStatusStalled, HealthReasonNeverIndexed
		h.Detail = "the indexer has not committed a batch for this vault yet"

	case now.Sub(*in.State.LastIndexedAt) >= th.StallAfter:
		h.Status, h.Reason = HealthStatusStalled, HealthReasonNoRecentCommit
		h.Detail = fmt.Sprintf("no indexer commit for %s (threshold %s); the indexer job may not be running",
			now.Sub(*in.State.LastIndexedAt).Truncate(time.Second), th.StallAfter)

	case *h.LagLedgers >= th.LaggingLedgers:
		h.Status, h.Reason = HealthStatusLagging, HealthReasonLagOverThreshold
		h.Detail = fmt.Sprintf("indexer is %d ledgers behind the chain tip (threshold %d); %d ledgers of retention headroom remain",
			*h.LagLedgers, th.LaggingLedgers, *h.RetentionHeadroom)

	default:
		h.Status, h.Reason = HealthStatusOK, HealthReasonNone
		h.Detail = fmt.Sprintf("indexer is %d ledgers behind the chain tip (threshold %d)",
			*h.LagLedgers, th.LaggingLedgers)
	}

	return h
}

// lagBetween is how far the cursor trails the tip, floored at zero. A cursor
// ahead of the tip happens legitimately when the cached tip is a few seconds
// old; it means "caught up", not negative lag.
func lagBetween(cursor int32, latest uint32) uint32 {
	if cursor < 0 {
		return latest
	}
	if uint32(cursor) >= latest {
		return 0
	}
	return latest - uint32(cursor)
}
