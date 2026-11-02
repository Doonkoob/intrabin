// Package metrics defines Prometheus operational metrics.
package metrics

import "github.com/prometheus/client_golang/prometheus"

var (
	Registry = prometheus.NewRegistry()

	ChecksTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "intrabin_checks_total", Help: "Probe executions by monitor type and outcome.",
	}, []string{"type", "outcome"})
	CheckErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "intrabin_check_errors_total", Help: "Failed probes by error category.",
	}, []string{"category"})
	CheckDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "intrabin_check_duration_seconds", Help: "Probe duration.", Buckets: prometheus.ExponentialBuckets(0.01, 2.5, 10),
	}, []string{"type"})
	RoundsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "intrabin_rounds_evaluated_total", Help: "Evaluated monitoring rounds by verdict.",
	}, []string{"verdict"})
	MonitorsByStatus = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "intrabin_monitors", Help: "Monitors by current status.",
	}, []string{"status"})
	OpenIncidents = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "intrabin_open_incidents", Help: "Unresolved incidents.",
	})
	StorageBytes = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "intrabin_storage_bytes", Help: "Database size on disk including WAL.",
	})
	ProbeQueueDropped = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "intrabin_probe_skipped_total", Help: "Probes skipped because the worker pool was saturated.",
	})
	HTTPRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "intrabin_http_requests_total", Help: "API requests by method and status class.",
	}, []string{"method", "code"})
	LoginFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "intrabin_login_failures_total", Help: "Failed login attempts.",
	})
	ClusterRole = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "intrabin_node_role", Help: "Current node role (1 for the active role).",
	}, []string{"role"})
)

func init() {
	Registry.MustRegister(ChecksTotal, CheckErrors, CheckDuration, RoundsTotal, MonitorsByStatus, OpenIncidents,
		StorageBytes, ProbeQueueDropped, HTTPRequests, LoginFailures, ClusterRole,
		prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
}
