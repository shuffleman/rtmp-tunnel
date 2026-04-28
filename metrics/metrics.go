package metrics

import (
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	once sync.Once

	queueWaitMs = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "rtmp_tunnel_queue_wait_ms",
		Help:    "Time from enqueue to first-byte scheduled (ms).",
		Buckets: []float64{0.1, 0.3, 1, 3, 10, 30, 100, 300, 1000, 3000, 10000},
	}, []string{"conn", "prio"})

	lockHoldMs = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "rtmp_tunnel_lock_hold_ms",
		Help:    "Mutex hold time (ms).",
		Buckets: []float64{0.01, 0.03, 0.1, 0.3, 1, 3, 10, 30, 100, 300},
	}, []string{"lock"})

	socketWriteMs = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "rtmp_tunnel_socket_write_ms",
		Help:    "Socket write latency (ms).",
		Buckets: []float64{0.05, 0.1, 0.3, 1, 3, 10, 30, 100, 300, 1000, 3000},
	}, []string{"conn", "kind"})

	perConnBacklogBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "rtmp_tunnel_per_conn_backlog_bytes",
		Help: "Approx pending plaintext bytes in scheduler per connection.",
	}, []string{"conn"})

	perStreamBacklog = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "rtmp_tunnel_per_stream_backlog",
		Help: "Pending queued frames per stream/session (approx).",
	}, []string{"conn", "stream"})

	smallPacketLatencyMs = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "rtmp_tunnel_small_packet_latency_ms",
		Help:    "Latency for small packets (ms), enqueue to scheduled.",
		Buckets: []float64{0.1, 0.3, 1, 3, 10, 30, 100, 300, 1000},
	}, []string{"conn"})

	dnsRttMs = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "rtmp_tunnel_dns_rtt_ms",
		Help:    "DNS RTT measured by bench tooling (ms).",
		Buckets: []float64{0.3, 1, 3, 10, 30, 100, 300, 1000, 3000, 10000},
	}, []string{"conn"})

	schedulerDropCount = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "rtmp_tunnel_scheduler_drop_count",
		Help: "Dropped frames due to queue limits/backpressure.",
	}, []string{"conn", "prio"})

	flushBatchBytes = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "rtmp_tunnel_flush_batch_bytes",
		Help:    "Total embedded proxy bytes per video frame.",
		Buckets: []float64{0, 128, 512, 1024, 2048, 4096, 8192, 16384, 32768, 65536},
	}, []string{"conn"})
)

func initOnce() {
	once.Do(func() {
		prometheus.MustRegister(
			queueWaitMs,
			lockHoldMs,
			socketWriteMs,
			perConnBacklogBytes,
			perStreamBacklog,
			smallPacketLatencyMs,
			dnsRttMs,
			schedulerDropCount,
			flushBatchBytes,
		)
	})
}

func ObserveQueueWait(conn, prio string, d time.Duration) {
	initOnce()
	queueWaitMs.WithLabelValues(conn, prio).Observe(float64(d) / float64(time.Millisecond))
}

func ObserveSmallPacketLatency(conn string, d time.Duration) {
	initOnce()
	smallPacketLatencyMs.WithLabelValues(conn).Observe(float64(d) / float64(time.Millisecond))
}

func ObserveLockHold(lock string, d time.Duration) {
	initOnce()
	lockHoldMs.WithLabelValues(lock).Observe(float64(d) / float64(time.Millisecond))
}

func ObserveSocketWrite(conn, kind string, d time.Duration) {
	initOnce()
	socketWriteMs.WithLabelValues(conn, kind).Observe(float64(d) / float64(time.Millisecond))
}

func SetPerConnBacklogBytes(conn string, v int) {
	initOnce()
	perConnBacklogBytes.WithLabelValues(conn).Set(float64(v))
}

func SetPerStreamBacklog(conn, stream string, v int) {
	initOnce()
	perStreamBacklog.WithLabelValues(conn, stream).Set(float64(v))
}

func IncSchedulerDrop(conn, prio string) {
	initOnce()
	schedulerDropCount.WithLabelValues(conn, prio).Inc()
}

func ObserveFlushBatchBytes(conn string, v int) {
	initOnce()
	flushBatchBytes.WithLabelValues(conn).Observe(float64(v))
}

func ObserveDNSRTT(conn string, d time.Duration) {
	initOnce()
	dnsRttMs.WithLabelValues(conn).Observe(float64(d) / float64(time.Millisecond))
}

func Start(addr string) *http.Server {
	initOnce()
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	srv := &http.Server{Addr: addr, Handler: mux}
	go srv.ListenAndServe()
	return srv
}

