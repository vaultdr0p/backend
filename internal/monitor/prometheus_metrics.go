package monitor

import (
	"github.com/prometheus/client_golang/prometheus"
)

func PrometheusMetrics() map[MetricTag]prometheus.Collector {
	metrics := make(map[MetricTag]prometheus.Collector)

	for tag, summaryVec := range SummaryVecMetrics {
		metrics[tag] = summaryVec
	}

	for tag, counter := range CounterMetrics {
		metrics[tag] = counter
	}

	for tag, histogramVec := range HistogramVecMetrics {
		metrics[tag] = histogramVec
	}

	for tag, counterVec := range CounterVecMetrics {
		metrics[tag] = counterVec
	}

	for tag, gaugeVec := range GaugeVecMetrics {
		metrics[tag] = gaugeVec
	}

	return metrics
}

var SummaryVecMetrics = map[MetricTag]*prometheus.SummaryVec{
	HTTPRequestDurationTag: prometheus.NewSummaryVec(prometheus.SummaryOpts{
		Namespace: DefaultNamespace, Subsystem: string(HTTPSubservice), Name: string(HTTPRequestDurationTag),
		Help: "HTTP requests durations, sliding window = 10m",
		Objectives: map[float64]float64{
			0.5:  0.05,  // 50th percentile with 5% error
			0.9:  0.01,  // 90th percentile with 1% error
			0.95: 0.01,  // 95th percentile with 1% error
			0.99: 0.001, // 99th percentile with 0.1% error
		},
	},
		[]string{"status", "route", "method", "tenant_name"},
	),
	SuccessfulQueryDurationTag: prometheus.NewSummaryVec(prometheus.SummaryOpts{
		Namespace: DefaultNamespace, Subsystem: string(DBSubservice), Name: string(SuccessfulQueryDurationTag),
		Help: "Successful DB query durations",
		// Match the HTTP summary's objectives so DB p50/p90/p95/p99 latency is chartable in Grafana
		// (previously only _sum/_count were exported, so no percentiles were available).
		Objectives: map[float64]float64{
			0.5:  0.05,
			0.9:  0.01,
			0.95: 0.01,
			0.99: 0.001,
		},
	},
		[]string{"query_type"},
	),
	FailureQueryDurationTag: prometheus.NewSummaryVec(prometheus.SummaryOpts{
		Namespace: DefaultNamespace, Subsystem: string(DBSubservice), Name: string(FailureQueryDurationTag),
		Help: "Failure DB query durations",
		Objectives: map[float64]float64{
			0.5:  0.05,
			0.9:  0.01,
			0.95: 0.01,
			0.99: 0.001,
		},
	},
		[]string{"query_type"},
	),
}

var CounterMetrics = map[MetricTag]prometheus.Counter{}

var HistogramVecMetrics = map[MetricTag]*prometheus.HistogramVec{
	CircleAPIRequestDurationTag: prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: DefaultNamespace, Subsystem: string(CircleSubservice), Name: string(CircleAPIRequestDurationTag),
		Help: "A histogram of the Circle API request durations",
	},
		CircleLabelNames,
	),
}

var CounterVecMetrics = map[MetricTag]*prometheus.CounterVec{
	DisbursementsCounterTag: prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: DefaultNamespace, Subsystem: string(BusinessSubservice), Name: string(DisbursementsCounterTag),
		Help: "Disbursements Counter",
	},
		[]string{"asset", "wallet", "wallet_id", "tenant_name"},
	),
	CircleAPIRequestsTotalTag: prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: DefaultNamespace, Subsystem: string(CircleSubservice), Name: string(CircleAPIRequestsTotalTag),
		Help: "A counter of the Circle API requests",
	},
		CircleLabelNames,
	),
}

// VaultLabelNames are the labels every vault indexer metric carries.
// contract_id is included because a tenant may run more than one vault, and a
// single degraded vault must be identifiable rather than averaged away.
var VaultLabelNames = []string{"tenant_name", "contract_id"}

var GaugeVecMetrics = map[MetricTag]*prometheus.GaugeVec{
	VaultIndexerLagLedgersTag: prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: DefaultNamespace, Subsystem: string(VaultSubservice), Name: string(VaultIndexerLagLedgersTag),
		Help: "Ledgers between the chain tip and the last ledger the vault event indexer has read. " +
			"Alert when this approaches the RPC event retention window: events beyond it cannot be " +
			"fetched again, and the recipient list becomes permanently incomplete.",
	},
		VaultLabelNames,
	),
	VaultIndexerDegradedTag: prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: DefaultNamespace, Subsystem: string(VaultSubservice), Name: string(VaultIndexerDegradedTag),
		Help: "1 when a vault has a known, unrecoverable gap in its indexed event history, 0 otherwise.",
	},
		VaultLabelNames,
	),
}
