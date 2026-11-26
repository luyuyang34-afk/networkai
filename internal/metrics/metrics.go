// Package metrics defines the Prometheus metrics both processes export.
//
// Conventions worth knowing:
//   - HTTP metrics are labelled by route *pattern* ("GET /posts/{id}"), never
//     by raw path. Raw paths contain ids, and every distinct label value
//     creates a new time series ("cardinality explosion").
//   - Latency is a histogram, not an average: P95/P99 are computed from
//     buckets at query time with histogram_quantile().
package metrics

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"networkai/internal/assist"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Latency buckets in seconds, dense around typical API latencies.
var latencyBuckets = []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5}

type Registry struct {
	reg *prometheus.Registry

	httpRequests *prometheus.CounterVec
	httpDuration *prometheus.HistogramVec

	outboxEvents   *prometheus.CounterVec
	outboxHandle   prometheus.Histogram
	outboxPaused   prometheus.Gauge
	searchRequests *prometheus.CounterVec

	readRoutes *prometheus.CounterVec

	consumerEvents *prometheus.CounterVec
	consumerLag    *prometheus.GaugeVec
	relayEvents    prometheus.Counter
	relayDelay     prometheus.Histogram

	fanoutFollowers prometheus.Histogram
	fanoutDuration  prometheus.Histogram

	llmRequests *prometheus.CounterVec
	llmTokens   *prometheus.CounterVec
	llmCost     prometheus.Counter
	llmLatency  prometheus.Histogram
}

func New() *Registry {
	r := &Registry{
		reg: prometheus.NewRegistry(),
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "networkai_http_requests_total",
			Help: "HTTP requests by route pattern and status code.",
		}, []string{"route", "status"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "networkai_http_request_duration_seconds",
			Help:    "HTTP request latency by route pattern.",
			Buckets: latencyBuckets,
		}, []string{"route"}),
		outboxEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "networkai_outbox_events_total",
			Help: "Outbox events handled, by result (ok, retry, dead, paused).",
		}, []string{"result"}),
		outboxHandle: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "networkai_outbox_handle_duration_seconds",
			Help:    "Time to apply one outbox event (PostgreSQL read + Elasticsearch write).",
			Buckets: latencyBuckets,
		}),
		outboxPaused: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "networkai_outbox_paused",
			Help: "1 while the worker is paused because a dependency is unavailable.",
		}),
		searchRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "networkai_search_requests_total",
			Help: "Search requests by outcome (ok, unavailable, error).",
		}, []string{"outcome"}),
		readRoutes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "networkai_db_read_routes_total",
			Help: "Read routing decisions: replica, primary_token (replica behind the client's token), primary_forced, primary_error.",
		}, []string{"target"}),
		consumerEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "networkai_consumer_events_total",
			Help: "Events handled per consumer group, by result (ok, retry, paused, dead).",
		}, []string{"group", "result"}),
		consumerLag: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "networkai_consumer_lag_seconds",
			Help: "End-to-end lag of the latest event a group touched (outbox insert -> handled).",
		}, []string{"group"}),
		relayEvents: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "networkai_relay_events_total",
			Help: "Outbox events published from the WAL to the broker.",
		}),
		relayDelay: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "networkai_relay_delay_seconds",
			Help:    "Commit to broker-acknowledged delay per transaction.",
			Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 5},
		}),
		fanoutFollowers: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "networkai_timeline_fanout_followers",
			Help:    "Followers written to per pushed post (fan-out on write).",
			Buckets: prometheus.ExponentialBuckets(1, 4, 10),
		}),
		fanoutDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "networkai_timeline_fanout_duration_seconds",
			Help:    "Time to push one post to all followers' timelines.",
			Buckets: []float64{.001, .005, .01, .05, .1, .5, 1, 5, 15, 60},
		}),
		llmRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "networkai_llm_requests_total",
			Help: "Writing-assistant LLM requests by outcome (ok, error, rate_limited).",
		}, []string{"outcome"}),
		llmTokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "networkai_llm_tokens_total",
			Help: "LLM tokens consumed, by direction (input, output).",
		}, []string{"direction"}),
		llmCost: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "networkai_llm_cost_usd_total",
			Help: "Estimated LLM spend in USD (from configured per-token prices).",
		}),
		llmLatency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "networkai_llm_request_duration_seconds",
			Help:    "LLM call latency.",
			Buckets: []float64{.1, .25, .5, 1, 2, 4, 8},
		}),
	}
	r.reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		r.httpRequests, r.httpDuration, r.outboxEvents, r.outboxHandle, r.outboxPaused, r.searchRequests,
		r.llmRequests, r.llmTokens, r.llmCost, r.llmLatency, r.fanoutFollowers, r.fanoutDuration,
		r.consumerEvents, r.consumerLag, r.relayEvents, r.relayDelay, r.readRoutes,
	)
	return r
}

func (r *Registry) Handler() http.Handler {
	return promhttp.HandlerFor(r.reg, promhttp.HandlerOpts{})
}

// ObserveHTTP records one finished request.
func (r *Registry) ObserveHTTP(route string, status int, d time.Duration) {
	if route == "" {
		route = "unmatched" // 404s for unknown paths: one series, not one per path
	}
	r.httpRequests.WithLabelValues(route, strconv.Itoa(status)).Inc()
	r.httpDuration.WithLabelValues(route).Observe(d.Seconds())
}

// ObserveLLM implements assist.Observer. Average cost per request is
// rate(networkai_llm_cost_usd_total) / rate(networkai_llm_requests_total{outcome="ok"}).
func (r *Registry) ObserveLLM(_ string, outcome string, u assist.Usage, costUSD float64, d time.Duration) {
	r.llmRequests.WithLabelValues(outcome).Inc()
	if outcome != "ok" {
		return
	}
	r.llmTokens.WithLabelValues("input").Add(float64(u.InputTokens))
	r.llmTokens.WithLabelValues("output").Add(float64(u.OutputTokens))
	r.llmCost.Add(costUSD)
	r.llmLatency.Observe(d.Seconds())
}

func (r *Registry) ObserveReadRoute(target string) { r.readRoutes.WithLabelValues(target).Inc() }

// ReplicaLagSource is implemented by store.Store.
type ReplicaLagSource interface {
	ReplicaLag(ctx context.Context) (bytes, seconds float64, ok bool, err error)
}

// RegisterReplicaLag exports replica lag, read at scrape time.
func (r *Registry) RegisterReplicaLag(src ReplicaLagSource) {
	r.reg.MustRegister(&replicaLagCollector{src: src})
}

var (
	replicaBytesDesc   = prometheus.NewDesc("networkai_replica_lag_bytes", "WAL the read replica has not replayed yet.", nil, nil)
	replicaSecondsDesc = prometheus.NewDesc("networkai_replica_lag_seconds",
		"Seconds since the replica replayed its last transaction (grows while idle, too).", nil, nil)
)

type replicaLagCollector struct{ src ReplicaLagSource }

func (c *replicaLagCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- replicaBytesDesc
	ch <- replicaSecondsDesc
}

func (c *replicaLagCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	b, sec, ok, err := c.src.ReplicaLag(ctx)
	if !ok || err != nil {
		return
	}
	ch <- prometheus.MustNewConstMetric(replicaBytesDesc, prometheus.GaugeValue, b)
	ch <- prometheus.MustNewConstMetric(replicaSecondsDesc, prometheus.GaugeValue, sec)
}

func (r *Registry) ObserveConsumer(group, result string, lag time.Duration) {
	r.consumerEvents.WithLabelValues(group, result).Inc()
	if lag > 0 {
		r.consumerLag.WithLabelValues(group).Set(lag.Seconds())
	}
}

func (r *Registry) ObserveRelay(n int, delay time.Duration) {
	r.relayEvents.Add(float64(n))
	r.relayDelay.Observe(delay.Seconds())
}

func (r *Registry) ObserveFanout(followers int, d time.Duration) {
	r.fanoutFollowers.Observe(float64(followers))
	r.fanoutDuration.Observe(d.Seconds())
}

func (r *Registry) ObserveSearch(outcome string) { r.searchRequests.WithLabelValues(outcome).Inc() }

func (r *Registry) ObserveOutboxEvent(result string, d time.Duration) {
	r.outboxEvents.WithLabelValues(result).Inc()
	if d > 0 {
		r.outboxHandle.Observe(d.Seconds())
	}
}

func (r *Registry) SetOutboxPaused(paused bool) {
	v := 0.0
	if paused {
		v = 1
	}
	r.outboxPaused.Set(v)
}

// RegisterOutboxBacklog exports pending/dead counts and the age of the oldest
// pending event, read from PostgreSQL at scrape time. Oldest-pending age is
// the best single "is search falling behind?" signal: it grows whether the
// cause is an outage, a stalled worker or a write spike.
func (r *Registry) RegisterOutboxBacklog(pool *pgxpool.Pool) {
	r.reg.MustRegister(&backlogCollector{pool: pool})
}

var (
	pendingDesc = prometheus.NewDesc("networkai_outbox_pending", "Outbox events waiting to be indexed.", nil, nil)
	deadDesc    = prometheus.NewDesc("networkai_outbox_dead", "Outbox events that exhausted their retries.", nil, nil)
	oldestDesc  = prometheus.NewDesc("networkai_outbox_oldest_pending_seconds", "Age of the oldest pending outbox event.", nil, nil)
	slotLagDesc = prometheus.NewDesc("networkai_replication_slot_lag_bytes",
		"WAL retained by the CDC replication slot. Grows without bound if the relay stops; alert on it.", nil, nil)
)

type backlogCollector struct{ pool *pgxpool.Pool }

func (c *backlogCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- pendingDesc
	ch <- deadDesc
	ch <- oldestDesc
	ch <- slotLagDesc
}

func (c *backlogCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var pending, dead int64
	var oldest float64
	err := c.pool.QueryRow(ctx, `
		SELECT
		  count(*) FILTER (WHERE processed_at IS NULL AND dead_at IS NULL),
		  count(*) FILTER (WHERE dead_at IS NOT NULL),
		  COALESCE(EXTRACT(EPOCH FROM now() - min(created_at)
		           FILTER (WHERE processed_at IS NULL AND dead_at IS NULL)), 0)::float8
		FROM outbox`).Scan(&pending, &dead, &oldest)
	if err != nil {
		ch <- prometheus.NewInvalidMetric(pendingDesc, err)
		return
	}
	ch <- prometheus.MustNewConstMetric(pendingDesc, prometheus.GaugeValue, float64(pending))
	ch <- prometheus.MustNewConstMetric(deadDesc, prometheus.GaugeValue, float64(dead))
	ch <- prometheus.MustNewConstMetric(oldestDesc, prometheus.GaugeValue, oldest)
	var slotLag float64
	if err := c.pool.QueryRow(ctx, `SELECT COALESCE(max(pg_wal_lsn_diff(pg_current_wal_lsn(), confirmed_flush_lsn)), 0)::float8
		FROM pg_replication_slots`).Scan(&slotLag); err == nil {
		ch <- prometheus.MustNewConstMetric(slotLagDesc, prometheus.GaugeValue, slotLag)
	}
}
