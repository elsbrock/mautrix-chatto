// Package bridgemetrics holds the Prometheus collectors exported by the
// bridge. bridgev2 has no metrics of its own, so everything is defined here.
package bridgemetrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

const namespace = "mautrix_chatto"

// Registry is the registry all bridge metrics are registered on. It also
// carries the Go runtime and process collectors.
var Registry = prometheus.NewRegistry()

var (
	// RPCDuration observes ConnectRPC call latency by procedure and result code.
	RPCDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "rpc_duration_seconds",
		Help:      "Latency of ConnectRPC calls to Chatto.",
		Buckets:   []float64{.01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
	}, []string{"procedure", "code"})

	// RPCErrors counts failed ConnectRPC calls by procedure and code.
	RPCErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "rpc_errors_total",
		Help:      "Failed ConnectRPC calls to Chatto.",
	}, []string{"procedure", "code"})

	// RealtimeConnected is 1 while a login's realtime socket is subscribed.
	RealtimeConnected = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "realtime_connected",
		Help:      "Whether the realtime WebSocket of a login is connected (1) or not (0).",
	}, []string{"login"})

	// RealtimeReconnects counts reconnect attempts by reason.
	RealtimeReconnects = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "realtime_reconnects_total",
		Help:      "Realtime WebSocket reconnects by reason.",
	}, []string{"reason"})

	// RealtimeRecoveries counts how each subscription reached live delivery.
	RealtimeRecoveries = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "realtime_recoveries_total",
		Help:      "Realtime subscription recoveries by result (resumed, snapshot, live_only).",
	}, []string{"result"})

	// RealtimeEvents counts received realtime events by type.
	RealtimeEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "realtime_events_total",
		Help:      "Realtime events received from Chatto by type.",
	}, []string{"type"})

	// RealtimeEventLag observes the delay between an event's creation on
	// Chatto and its arrival at the bridge.
	RealtimeEventLag = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "realtime_event_lag_seconds",
		Help:      "Delay between event creation on Chatto and receipt by the bridge.",
		Buckets:   []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 300},
	})

	// MessagesBridged counts bridged messages by direction and kind.
	MessagesBridged = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "messages_bridged_total",
		Help:      "Events bridged, by direction (to_matrix, to_chatto) and type.",
	}, []string{"direction", "type"})

	// BridgeErrors counts events that failed to bridge.
	BridgeErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "bridge_errors_total",
		Help:      "Events that failed to bridge, by direction and type.",
	}, []string{"direction", "type"})

	// TokenRefreshes counts access token refreshes by result.
	TokenRefreshes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "token_refreshes_total",
		Help:      "Chatto access token refreshes by result.",
	}, []string{"result"})

	// Logins reports the number of logins in each bridge state.
	Logins = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "logins",
		Help:      "Number of Chatto logins by bridge state event.",
	}, []string{"state"})

	// ServerInfo exposes the Chatto server version as a label.
	ServerInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "chatto_server_info",
		Help:      "Chatto server version seen by the bridge (value is always 1).",
	}, []string{"version", "supported"})

	// BuildInfo exposes the bridge version.
	BuildInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "build_info",
		Help:      "Bridge build information (value is always 1).",
	}, []string{"version", "commit", "chatto_api"})
)

func init() {
	Registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		RPCDuration, RPCErrors,
		RealtimeConnected, RealtimeReconnects, RealtimeRecoveries, RealtimeEvents, RealtimeEventLag,
		MessagesBridged, BridgeErrors, TokenRefreshes, Logins, ServerInfo, BuildInfo,
	)
}
